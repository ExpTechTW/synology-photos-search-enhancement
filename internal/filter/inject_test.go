package filter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

var item = demoItem{raw: json.RawMessage(`{"id":90001,"owner_user_id":1,"time":1750000000,"type":"photo"}`),
	id: 90001, ownerUser: 1, time: 1750000000}

func resp(data string) []byte { return []byte(`{"data":` + data + `,"success":true}`) }

func qp(kv ...string) *params {
	p := newParams()
	for i := 0; i+1 < len(kv); i += 2 {
		p.set(kv[i], kv[i+1])
	}
	return p
}

// decoded 把改寫後的回應解成 map，方便比對。
func decoded(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("不是合法的 JSON：%v：%s", err, b)
	}
	return m
}

func listIDs(t *testing.T, b []byte) []float64 {
	t.Helper()
	var out []float64
	for _, x := range decoded(t, b)["data"].(map[string]any)["list"].([]any) {
		out = append(out, x.(map[string]any)["id"].(float64))
	}
	return out
}

func TestListItemDedupeAndFirstPageOnly(t *testing.T) {
	d := injectDemo("list_item", qp(), resp(`{"list":[{"id":1}]}`), []demoItem{item})
	if got := listIDs(t, d); !reflect.DeepEqual(got, []float64{90001, 1}) {
		t.Errorf("%v", got)
	}
	if injectDemo("list_item", qp(), resp(`{"list":[{"id":90001}]}`), []demoItem{item}) != nil {
		t.Error("已經有的不該重複注入")
	}
	if injectDemo("list_item", qp("offset", "100"), resp(`{"list":[]}`), []demoItem{item}) != nil {
		t.Error("第二頁不該注入")
	}
}

func TestSectionFormat(t *testing.T) {
	d := injectDemo("get_search_timeline", qp(), resp(`{"section":[]}`), []demoItem{item})
	top, _ := parseObject(d)
	body, _ := parseObject(top.get("data"))
	secs, _ := parseArray(body.get("section"))
	want := `{"limit":100,"list":[{"day":15,"item_count":1,"month":6,"year":2025}],"offset":0}`
	if string(secs[0]) != want {
		t.Errorf("%s", secs[0])
	}
}

func TestCountAndBasic(t *testing.T) {
	d := injectDemo("count_item", qp(), resp(`{"count":4}`), []demoItem{item})
	if c := decoded(t, d)["data"].(map[string]any)["count"]; c != 5.0 {
		t.Errorf("count %v", c)
	}
	d = injectDemo("list_basic_timeline_info", qp(), resp(`{"list":[]}`), []demoItem{item})
	top, _ := parseObject(d)
	body, _ := parseObject(top.get("data"))
	if got := string(body.get("list")); got != `[{"id":90001,"owner_user_id":1,"time":1750000000}]` {
		t.Errorf("%s", got)
	}
}

func TestLeavesErrorsAlone(t *testing.T) {
	cases := []struct{ method, data string }{
		{"list_item", `{"success":false,"error":{"code":120}}`},
		{"count_item", string(resp(`{"count":"4"}`))},
		{"list_item", string(resp(`{"list":"x"}`))},
	}
	for _, c := range cases {
		if injectDemo(c.method, qp(), []byte(c.data), []demoItem{item}) != nil {
			t.Errorf("%s %s：不該改", c.method, c.data)
		}
	}
}

func TestCGIResponseRoundtrip(t *testing.T) {
	body := `{"data":{"count":1},"success":true}`
	raw := []byte(fmt.Sprintf("Status: 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	parsed := parseCGIResponse(raw)
	if parsed == nil {
		t.Fatal("看不懂正常的回應")
	}
	out := buildCGIResponse(parsed.lines, injectDemo("count_item", qp(), parsed.body, []demoItem{item}))
	head, nb, _ := bytes.Cut(out, []byte("\r\n\r\n"))
	if c := decoded(t, nb)["data"].(map[string]any)["count"]; c != 2.0 {
		t.Errorf("count %v", c)
	}
	if !bytes.Contains(head, []byte(fmt.Sprintf("Content-Length: %d", len(nb)))) { // 長度跟著改
		t.Errorf("%s", head)
	}
	for _, bad := range [][]byte{
		bytes.Replace(raw, []byte("200 OK"), []byte("500 Error"), 1),
		bytes.Replace(raw, []byte("Content-Type"), []byte("Content-Encoding: gzip\r\nX"), 1),
		bytes.Replace(raw, []byte(`"success":true}`), []byte(`"success":tr`), 1),
		[]byte("no header separator"),
	} {
		if parseCGIResponse(bad) != nil {
			t.Errorf("%q：不該碰", bad)
		}
	}
}

const day = 86400

// itemsByDay 依序產生新到舊的照片：counts[i] 張在第 i 天（往回推）。
func itemsByDay(counts ...int) []basic {
	t0 := int64(1700000000 + 400*day)
	var out []basic
	uid := int64(0)
	for i, c := range counts {
		for range c {
			uid++
			out = append(out, basic{uid, json.RawMessage("1"), t0 - int64(i)*day})
		}
	}
	return out
}

type shape struct {
	limit, offset int
	counts        []int
}

func shapeOf(secs []section) []shape {
	var out []shape
	for _, s := range secs {
		var c []int
		for _, e := range s.List {
			c = append(c, e.ItemCount)
		}
		out = append(out, shape{s.Limit, s.Offset, c})
	}
	return out
}

// 分段規則由 Photos 1.9.0 的原生回應反推，並在真實 NAS 上用 30 個關鍵字 × day/month 逐筆比對過。

func TestAccumulateUntilOver100AndMergeTail(t *testing.T) {
	cases := []struct {
		counts []int
		want   []shape
	}{
		{[]int{60, 50, 30}, []shape{{140, 0, []int{60, 50, 30}}}},
		{[]int{60, 40, 30}, []shape{{130, 0, []int{60, 40, 30}}}},
		{[]int{5}, []shape{{5, 0, []int{5}}}},
	}
	for _, c := range cases {
		if got := shapeOf(buildSections(itemsByDay(c.counts...), "day")); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v：%v", c.counts, got)
		}
	}
	if got := string(mustMarshal(buildSections(nil, "day"))); got != "[]" {
		t.Errorf("%s", got)
	}
}

func TestBigGroupSplitEvenly(t *testing.T) {
	// 42 張還沒滿 → 前面沒有一般段可併，自成一段；350 張的那天拆成 3 段；最後 20 張前一段是大組，自成一段
	got := shapeOf(buildSections(itemsByDay(20, 22, 350, 20), "day"))
	want := []shape{{42, 0, []int{20, 22}}, {117, 0, []int{350}}, {117, 117, []int{350}}, {116, 234, []int{350}},
		{20, 0, []int{20}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestPendingMergesIntoPreviousNormalBeforeBigGroup(t *testing.T) {
	got := shapeOf(buildSections(itemsByDay(60, 50, 40, 200), "day"))
	want := []shape{{150, 0, []int{60, 50, 40}}, {100, 0, []int{200}}, {100, 100, []int{200}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestMonthUnitAndUTC(t *testing.T) {
	secs := buildSections([]basic{
		{1, json.RawMessage("1"), 1704067199}, // 2023-12-31 23:59:59 UTC
		{2, json.RawMessage("1"), 1704067200}, // 2024-01-01 UTC
	}, "month")
	want := `[{"limit":2,"list":[{"item_count":1,"month":12,"year":2023},{"item_count":1,"month":1,"year":2024}],"offset":0}]`
	if got := string(mustMarshal(secs)); got != want {
		t.Errorf("%s", got)
	}
}

func TestMergeKeepsNativeTieOrder(t *testing.T) {
	one := json.RawMessage("1")
	native := []basic{{5, one, 100}, {9, one, 100}, {3, one, 50}}
	ours := []basic{{7, one, 100}, {3, one, 50}}
	merged, err := mergeBasic(native, ours)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, x := range merged {
		ids = append(ids, x.id)
	}
	if !reflect.DeepEqual(ids, []int64{5, 9, 7, 3}) {
		t.Errorf("%v", ids)
	}
}
