"""建立 config.yml：互動式設定精靈，或從舊版的 config.toml 轉過來。

都是把值填進 data/config.example.yml（保留範本裡的說明），寫成權限 600 的檔案。
"""

import getpass
import json
import os
import re

TEMPLATE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "data", "config.example.yml")


def set_value(text, section, key, value):
    """把 section 底下的 key 改成 value；key 被註解掉（# key:）也會改成啟用。保留行尾的註解。"""
    lines = text.split("\n")
    in_section = False
    for i, line in enumerate(lines):
        if re.match(r"^[A-Za-z_]+:", line):
            in_section = line.split(":", 1)[0] == section
            continue
        if not in_section:
            continue
        m = re.match(r"^(\s+)(#\s*)?%s:(\s*)([^#]*?)(\s*#.*)?$" % re.escape(key), line)
        if m:
            head = "%s%s: %s" % (m.group(1), key, yaml_scalar(value))
            comment = (m.group(5) or "").strip()
            if comment:   # 行尾說明盡量維持在原本的欄位
                head = head.ljust(max(len(head) + 1, m.start(5) + len(m.group(5)) - len(m.group(5).lstrip()))) + comment
            lines[i] = head
            return "\n".join(lines)
    raise KeyError("範本裡找不到 %s.%s" % (section, key))


def yaml_scalar(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return str(v)
    if isinstance(v, dict):
        return "{%s}" % ", ".join("%s: %s" % (k, yaml_scalar(x)) for k, x in v.items())
    return json.dumps(str(v), ensure_ascii=False)   # JSON 字串也是合法的 YAML 雙引號字串


def render(values):
    """values：{(section, key): value}。"""
    with open(TEMPLATE, encoding="utf-8") as f:
        text = f.read()
    for (section, key), v in values.items():
        text = set_value(text, section, key, v)
    return text


def write_config(path, text, uid=None, gid=None):
    d = os.path.dirname(path)
    os.makedirs(d, exist_ok=True)
    tmp = path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write(text)
    os.chmod(tmp, 0o600)
    if uid is not None and os.geteuid() == 0:
        os.chown(tmp, uid, gid)
    os.replace(tmp, path)


def ask(prompt, default="", secret=False):
    shown = "%s%s: " % (prompt, " [%s]" % default if default and not secret else "")
    while True:
        v = getpass.getpass(shown) if secret else input(shown)
        v = v.strip() or default
        if v:
            return v
        print("  這一項必須填")


def ask_yes(prompt, default=True):
    v = input("%s [%s]: " % (prompt, "Y/n" if default else "y/N")).strip().lower()
    return default if not v else v in ("y", "yes")


def interactive(account_default):
    print("\n建立設定檔（之後可以直接編輯 config.yml，再執行 sudo sse apply）")
    print("Creating config.yml (you can edit it later and run: sudo sse apply)\n")
    v = {}
    v[("synology", "account")] = ask("照片擁有者的 DSM 帳號 / DSM account that owns the photos", account_default)
    v[("synology", "password")] = ask("DSM 密碼 / DSM password", secret=True)
    print("\nEmbedding 服務（OpenAI 相容、提供 EmbeddingGemma 2 圖片 embedding；架設方式見 docs/models.md）")
    v[("embedding", "base_url")] = ask("  網址 / base URL（例如 http://192.168.1.10:8080/v1）")
    v[("embedding", "api_key")] = getpass.getpass("  API key（沒有就按 Enter / leave empty if none）: ").strip()
    v[("embedding", "model")] = ask("  模型名稱 / model", "embeddinggemma-2")
    print()
    if ask_yes("要讀照片裡的文字（OCR）嗎？需要一個能看圖的模型 / Enable OCR (needs a vision chat model)?"):
        same = ask_yes("  OCR 跟 embedding 用同一個服務？/ Same server as embedding?")
        if not same:
            v[("ocr", "base_url")] = ask("  OCR 網址 / OCR base URL")
            v[("ocr", "api_key")] = getpass.getpass("  OCR API key（沒有就按 Enter）: ").strip()
        v[("ocr", "model")] = ask("  OCR 模型名稱 / OCR model（例如 gemma3:12b、gpt-4o-mini）")
    else:
        v[("ocr", "enabled")] = False
    return render(v)


def from_toml(toml_path):
    """舊版（config.toml + secrets/ 底下的密碼檔）轉成 config.yml。"""
    import tomllib

    with open(toml_path, "rb") as f:
        old = tomllib.load(f)
    base = os.path.dirname(toml_path)

    def secret(p):
        p = os.path.expanduser(p or "")
        if p and not os.path.isabs(p):
            p = os.path.join(base, p)
        with open(p, encoding="utf-8") as f:
            return f.read().strip()

    s, llm, idx, search = old.get("synology", {}), old.get("llm", {}), old.get("index", {}), old.get("search", {})
    v = {
        ("synology", "account"): s["account"],
        ("synology", "password"): secret(s["password_file"]),
        ("embedding", "base_url"): llm["base_url"],
        ("embedding", "api_key"): secret(llm["api_key_file"]) if llm.get("api_key_file") else "",
        ("embedding", "model"): llm.get("embed_model", "embeddinggemma-2"),
        ("ocr", "enabled"): idx.get("ocr", "all") != "off",
        ("ocr", "model"): llm.get("ocr_model", ""),
        # 舊版固定送 reasoning_effort: none，沿用以免 OCR 行為改變
        ("ocr", "extra_body"): {"reasoning_effort": "none"},
    }
    if s.get("url"):
        v[("synology", "url")] = s["url"]
    if s.get("thumb_size"):
        v[("embedding", "image_size")] = s["thumb_size"]
    if s.get("ocr_thumb_size"):
        v[("ocr", "image_size")] = s["ocr_thumb_size"]
    for k in ("min_score", "text_min_score"):
        if k in search:
            v[("search", k)] = search[k]
    return render(v)
