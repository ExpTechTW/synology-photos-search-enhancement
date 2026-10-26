package filter

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

type hdr struct{ k, v string }

func scgi(h []hdr, body string) []byte {
	var b bytes.Buffer
	b.WriteString("CONTENT_LENGTH\x00" + strconv.Itoa(len(body)) + "\x00")
	for _, x := range h {
		b.WriteString(x.k + "\x00" + x.v + "\x00")
	}
	return []byte(strconv.Itoa(b.Len()) + ":" + b.String() + "," + body)
}

const search = "api=SYNO.Foto.Search.Search&version=7&method=list_item&offset=0&limit=100&keyword=%22test%22"

func form(override ...hdr) []hdr {
	h := []hdr{{"REQUEST_METHOD", "POST"}, {"REQUEST_URI", "/webapi/entry.cgi"},
		{"CONTENT_TYPE", "application/x-www-form-urlencoded; charset=UTF-8"}}
	for _, o := range override {
		for i := range h {
			if h[i].k == o.k {
				h[i].v = o.v
			}
		}
	}
	return h
}

func sniff(raw []byte, step int) *sniffer {
	s := newSniffer(262144, 65536)
	if step == 0 {
		s.feed(raw)
		return s
	}
	for i := 0; i < len(raw); i += step {
		s.feed(raw[i:min(i+step, len(raw))])
	}
	return s
}

func TestSearchPlanEvenWhenFedByteByByte(t *testing.T) {
	for _, step := range []int{0, 1, 7} {
		p := sniff(scgi(form(), search), step).plan()
		if p == nil || p.method != "list_item" || p.keyword != "test" {
			t.Fatalf("step %d：%+v", step, p)
		}
	}
}

func TestQueryStringAndPathInfo(t *testing.T) {
	h := []hdr{{"REQUEST_METHOD", "GET"}, {"REQUEST_URI", "/webapi/entry.cgi/SYNO.Foto.Search.Search?" + search},
		{"QUERY_STRING", search}}
	if p := sniff(scgi(h, ""), 0).plan(); p == nil || p.method != "list_item" {
		t.Fatalf("%+v", p)
	}
}

func TestNotInspected(t *testing.T) {
	cases := [][]byte{
		scgi(form(hdr{"REQUEST_URI", "/webapi/other.cgi"}), search),
		scgi(form(hdr{"CONTENT_TYPE", "multipart/form-data; boundary=x"}), search),
		scgi(form(), strings.Replace(search, "SYNO.Foto.Search", "SYNO.FotoTeam.Search", 1)),
		scgi(form(), strings.Replace(search, "list_item", "suggest", 1)),
		scgi(form(), strings.Replace(search, "%22test%22", "", 1)),
		scgi(form(), strings.Repeat("x", 70000)),
		[]byte("garbage-without-netstring"),
		[]byte("5:abc,"),
	}
	for _, raw := range cases {
		if p := sniff(raw, 0).plan(); p != nil {
			t.Errorf("%q：不該檢查", raw[:min(len(raw), 60)])
		}
	}
}

func TestSkipReasons(t *testing.T) {
	cases := []struct {
		raw  []byte
		want string
	}{
		{scgi(form(hdr{"CONTENT_TYPE", "application/json"}), `{"api":1}`), "Content-Type application/json"},
		{scgi(form(hdr{"REQUEST_URI", "/x.cgi"}), search), "不是 entry.cgi"},
		{scgi(form(), strings.Repeat("x", 70000)), "body 太大（70000 bytes）"},
	}
	for _, c := range cases {
		if got := sniff(c.raw, 0).skipReason; got != c.want {
			t.Errorf("%q，要 %q", got, c.want)
		}
	}
}

func TestKeywordNormalization(t *testing.T) {
	cases := map[string]string{` "Test" `: "Test", `"貓"`: "貓", "'dog'": "dog", `"`: `"`}
	for in, want := range cases {
		if got := normalizeKeyword(in, true); got != want {
			t.Errorf("%q → %q，要 %q", in, got, want)
		}
	}
}
