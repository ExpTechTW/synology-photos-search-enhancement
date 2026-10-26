package admin

import (
	"strings"
	"testing"
)

const dsmConf = `user http http;
worker_processes auto;
http {
    include mime.types;

    upstream synoscgi {
        server unix:/run/synoscgi.sock;
    }

    server {
        listen 5000;
        location ~ \.cgi {
            include scgi_params;
            scgi_pass synoscgi;
        }
    }
}
`

func mustPatch(t *testing.T, conf string) string {
	t.Helper()
	out, err := nginxPatch(conf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPristinePatchUnpatchRoundtrip(t *testing.T) {
	if k, why := nginxClassify(dsmConf); k != pristine {
		t.Fatal(why)
	}
	patched := mustPatch(t, dsmConf)
	if k, why := nginxClassify(patched); k != managed {
		t.Fatal(why)
	}
	for _, want := range []string{"        server unix:/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n",
		"        server unix:/run/synoscgi.sock backup;\n"} {
		if !strings.Contains(patched, want) {
			t.Errorf("找不到 %q", want)
		}
	}
	if back, err := nginxUnpatch(patched); err != nil || back != dsmConf { // 還原後逐字相同
		t.Errorf("還原：%v\n%s", err, back)
	}
	if back, err := nginxUnpatch(dsmConf); err != nil || back != dsmConf {
		t.Errorf("原廠設定不用還原：%v", err)
	}
	if _, err := nginxPatch(patched); err == nil { // 已套用的不會再套一次
		t.Error("已套用的又套用了一次")
	}
}

// Python 版套用過的 nginx.conf（逐字取自 Python 版的輸出）：Go 版要認得、還原得回去，自己套用的也要一模一樣。
func TestSameMarkersAsPythonVersion(t *testing.T) {
	cases := []struct{ conf, patched string }{
		{"user http http;\nhttp {\n    upstream synoscgi {\n        server unix:/run/synoscgi.sock;\n    }\n}\n",
			"user http http;\nhttp {\n    upstream synoscgi {\n        # >>> synology-search-enhancement original=\"server unix:/run/synoscgi.sock;\" >>>\n        server unix:/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n        server unix:/run/synoscgi.sock backup;\n        # <<< synology-search-enhancement <<<\n    }\n}\n"},
		{"upstream synoscgi {\n\t# 註解\n\tserver  unix:/run/synoscgi.sock;\t \n}\n",
			"upstream synoscgi {\n\t# 註解\n\t# >>> synology-search-enhancement original=\"server  unix:/run/synoscgi.sock;\" >>>\n\tserver unix:/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n\tserver unix:/run/synoscgi.sock backup;\n\t# <<< synology-search-enhancement <<<\t \n}\n"},
		{"upstream synoscgi {\n  server\tunix:/run/synoscgi.sock ;\n}\n",
			"upstream synoscgi {\n  # >>> synology-search-enhancement original=\"server\\tunix:/run/synoscgi.sock ;\" >>>\n  server unix:/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n  server unix:/run/synoscgi.sock backup;\n  # <<< synology-search-enhancement <<<\n}\n"},
	}
	for _, c := range cases {
		if got := mustPatch(t, c.conf); got != c.patched {
			t.Errorf("套用的結果跟 Python 版不同：\n%q\n%q", got, c.patched)
		}
		if k, why := nginxClassify(c.patched); k != managed {
			t.Errorf("%s：%q", why, c.patched)
		}
		if back, _ := nginxUnpatch(c.patched); back != c.conf {
			t.Errorf("還原：%q", back)
		}
	}
}

func TestTabsAndCommentsPreserved(t *testing.T) {
	conf := "upstream synoscgi {\n\t# 註解\n\tserver unix:/run/synoscgi.sock;  \n}\n"
	if k, _ := nginxClassify(conf); k != pristine {
		t.Fatal(k)
	}
	patched := mustPatch(t, conf)
	if !strings.Contains(patched, "\tserver unix:/run/synoscgi.sock backup;") {
		t.Error(patched)
	}
	if back, _ := nginxUnpatch(patched); back != conf {
		t.Errorf("%q", back)
	}
}

func assertUnknown(t *testing.T, conf, needle string) {
	t.Helper()
	kind, why := nginxClassify(conf)
	if kind != unknown || !strings.Contains(why, needle) {
		t.Errorf("%s：%s（要包含 %q）", kind, why, needle)
	}
	if _, err := nginxPatch(conf); err == nil {
		t.Error("不認得的格式不能套用")
	}
}

func TestRefusesUnexpectedLayouts(t *testing.T) {
	assertUnknown(t, "upstream synoscgi { server unix:/run/synoscgi.sock; }\n", "獨立一行")
	assertUnknown(t, strings.Replace(dsmConf, "/run/synoscgi.sock", "/run/synosse-filter.sock", 1), "其他 filter")
	assertUnknown(t, dsmConf+"upstream synoscgi {\n    server unix:/x;\n}\n", "2 個")
	assertUnknown(t, strings.Replace(dsmConf, "synoscgi.sock;", "synoscgi.sock max_fails=0;", 1), "不是預期")
	assertUnknown(t, strings.Replace(dsmConf, "synoscgi.sock;\n", "synoscgi.sock;\n        keepalive 8;\n", 1), "不是預期")
	assertUnknown(t, strings.Replace(dsmConf, "upstream synoscgi", "upstream other", 1), "0 個")
	assertUnknown(t, "upstream synoscgi {\n    server unix:/run/synoscgi.sock;\n", "沒有結尾")
}

func TestRefusesEditedManagedBlock(t *testing.T) {
	patched := mustPatch(t, dsmConf)
	assertUnknown(t, strings.Replace(patched, "fail_timeout=5s", "fail_timeout=60s", 1), "手動修改")
	assertUnknown(t, strings.Replace(patched, markEnd, "", 1), "不完整")
	assertUnknown(t, strings.Replace(patched, "    }\n\n    server", "        keepalive 8;\n    }\n\n    server", 1), "其他設定")
	assertUnknown(t, strings.Replace(patched, `original="server`, `original="sever`, 1), "手動修改")
	assertUnknown(t, strings.Replace(patched, `original="server unix:/run/synoscgi.sock;"`, `original="\x"`, 1), "無法解析")
	if _, err := nginxUnpatch(strings.Replace(patched, "fail_timeout=5s", "fail_timeout=60s", 1)); err == nil {
		t.Error("被改過的區塊不能還原")
	}
}

func TestPyJSON(t *testing.T) {
	for in, want := range map[string]string{
		"server unix:/run/synoscgi.sock;": `"server unix:/run/synoscgi.sock;"`,
		"a\tb\"c\\":                       `"a\tb\"c\\"`,
		"\u8a3b\x7f\x01\U0001F600":        "\"\\u8a3b\\u007f\\u0001\\ud83d\\ude00\"",
	} {
		if got := pyJSON(in); got != want {
			t.Errorf("%q → %s，預期 %s", in, got, want)
		}
	}
}
