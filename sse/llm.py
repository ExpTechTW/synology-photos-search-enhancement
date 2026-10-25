"""OpenAI 相容 API 的 client：圖片與文字 embedding、OCR。embedding 與 OCR 可以是不同的服務。

圖片 embedding 用的是 llama.cpp 的 multimodal prompt 格式（OpenAI 的 image_url 不適用於 /v1/embeddings）：
  {"input": [{"prompt_string": "<__media__>", "multimodal_data": ["<base64>"]}]}
llama-server 要用 LLAMA_MEDIA_MARKER 固定 marker，否則每次啟動都是隨機的。

不對模型服務併發：embedding 與 OCR 各自一次只送一個請求、一個請求只有一張圖或一段文字，
上一個完成才送下一個。索引器與搜尋服務是不同的 process，用資料目錄裡的鎖檔排隊。
"""

import base64
import contextlib
import fcntl
import json
import os
import re
import struct
import urllib.error
import urllib.request

QUERY_PREFIX = "task: search result | query: "
# 文件（照片裡的文字）的前綴：EmbeddingGemma 的檢索是「查詢前綴 vs 文件前綴」，兩邊都要加
DOC_PREFIX = "title: none | text: "
DOC_MAX_CHARS = 2000   # 文字很多的截圖只取前面一段：太長的文件向量會被稀釋，也比較慢
OCR_PROMPT = ("Transcribe all legible text in this image exactly as written, line by line, keeping the original "
              "language. Output only the text. If there is no legible text, output NONE.")


class LLMError(Exception):
    """transient=True：服務暫時有問題（連不上、逾時、5xx、429），不是這張照片的問題。"""

    def __init__(self, msg, transient=False):
        super().__init__(msg)
        self.transient = transient


class Endpoint:
    def __init__(self, base_url, api_key, timeout):
        self.base = base_url.rstrip("/")
        self.key = api_key
        self.timeout = timeout


class LLM:
    def __init__(self, embed, ocr, embed_model, ocr_model, media_marker="<__media__>", ocr_extra_body=None,
                 ocr_max_tokens=4096, lock_dir=None):
        self.embed_ep = embed
        self.ocr_ep = ocr
        self.embed_model = embed_model
        self.ocr_model = ocr_model
        self.media_marker = media_marker
        self.ocr_extra_body = dict(ocr_extra_body or {})
        self.ocr_max_tokens = ocr_max_tokens
        self.lock_dir = lock_dir

    @classmethod
    def from_config(cls, cfg):
        c = cfg["llm"]
        return cls(Endpoint(c["embed_base_url"], c["embed_api_key"], c["embed_timeout"]),
                   Endpoint(c["ocr_base_url"], c["ocr_api_key"], c["ocr_timeout"]),
                   c["embed_model"], c["ocr_model"], c["media_marker"], c["ocr_extra_body"], c["ocr_max_tokens"],
                   lock_dir=os.path.dirname(cfg["index"]["db"]))

    @contextlib.contextmanager
    def _one_at_a_time(self, kind):
        """同一種請求排隊送。每次開新的檔案描述子再 flock：同一個 process 的不同 thread 之間也會排隊。"""
        if not self.lock_dir:
            yield
            return
        os.makedirs(self.lock_dir, exist_ok=True)
        # 唯讀開啟：鎖檔萬一被 root 建立，照片擁有者照樣能開來上鎖
        fd = os.open(os.path.join(self.lock_dir, kind + ".lock"), os.O_RDONLY | os.O_CREAT, 0o644)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            os.close(fd)

    def _post(self, ep, path, body):
        headers = {"Content-Type": "application/json"}
        if ep.key:
            headers["Authorization"] = "Bearer " + ep.key
        req = urllib.request.Request(ep.base + path, json.dumps(body).encode(), headers)
        try:
            with urllib.request.urlopen(req, timeout=ep.timeout) as r:
                return json.loads(r.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            detail = e.read(500).decode("utf-8", "replace")
            raise LLMError("%s 回應 HTTP %s：%s" % (path, e.code, detail), transient=e.code >= 500 or e.code == 429)
        except (urllib.error.URLError, OSError) as e:
            raise LLMError("%s 連線失敗：%s" % (path, e), transient=True)
        except ValueError as e:
            raise LLMError("%s 回應不是 JSON：%s" % (path, e), transient=True)

    def _embed(self, inp):
        """一張圖或一段文字的向量。"""
        with self._one_at_a_time("embed"):
            d = self._post(self.embed_ep, "/embeddings", {"model": self.embed_model, "input": [inp]})
        data = d.get("data") or []
        if len(data) != 1:
            raise LLMError("embedding 回傳 %d 筆，預期 1 筆" % len(data))
        v = data[0]["embedding"]
        if any(c != c for c in v):  # NaN：EmbeddingGemma 在 FP16 下會出現
            raise LLMError("embedding 含 NaN：EmbeddingGemma 不能用 FP16 執行，請改用 BF16 或 F32（見 docs/models.md）")
        return v

    def embed_image(self, image):
        return self._embed({"prompt_string": self.media_marker, "multimodal_data": [base64.b64encode(image).decode()]})

    def embed_query(self, text):
        return self._embed(QUERY_PREFIX + text)

    def embed_text(self, text):
        return self._embed(DOC_PREFIX + text[:DOC_MAX_CHARS])

    def ocr(self, image, mime="image/jpeg"):
        body = {
            "model": self.ocr_model, "temperature": 0, "max_tokens": self.ocr_max_tokens,
            "messages": [{"role": "user", "content": [
                {"type": "image_url", "image_url": {"url": "data:%s;base64,%s" % (mime, base64.b64encode(image).decode())}},
                {"type": "text", "text": OCR_PROMPT}]}]}
        body.update(self.ocr_extra_body)   # 例如會思考的模型要 {"reasoning_effort": "none"}，否則又慢又貴
        with self._one_at_a_time("ocr"):
            d = self._post(self.ocr_ep, "/chat/completions", body)
        try:
            choice = d["choices"][0]
            text = (choice["message"].get("content") or "").strip()
        except (KeyError, IndexError, TypeError):
            raise LLMError("OCR 回應格式不對：%s" % json.dumps(d)[:200])
        # 文字很多的截圖可能超過上限：保留已經讀到的部分（對搜尋一樣有用），不當成失敗
        return clean_ocr(text)


# 模型偶爾不轉錄，而是回一句話（例如圖片太小或全白時）
CHATTER = re.compile(r"^(please provide|i (can(no|')t|am unable|'m unable|'m sorry)|sorry|there (is|are) no|"
                     r"no (legible )?text|the image|this image|none\b)", re.I)


def clean_ocr(text):
    """清掉 OCR 的雜訊：聊天式回答、回傳提示詞本身、程式碼區塊標記、重複的行。

    重複的行只留第一次出現的：模型有時會陷入迴圈重複同一段直到字數上限（幻覺），
    而搜尋是子字串比對，同一行出現幾次都一樣。"""
    if "Transcribe all legible text" in text:
        return ""
    lines = [ln.strip() for ln in text.strip().strip("`").splitlines()]
    lines = [ln for ln in lines if ln and ln.lower() not in ("text", "plaintext")]
    if not lines or (len(lines) <= 2 and CHATTER.match(lines[0])):
        return ""
    out, seen = [], set()
    for ln in lines:
        if ln not in seen:
            seen.add(ln)
            out.append(ln)
    if len(out) == 1 and len(lines) >= 3 and len(out[0]) <= 2:
        return ""  # 同一個字重複好幾行，多半是幻覺
    return "\n".join(out)


def to_f16(v):
    return struct.pack("<%de" % len(v), *v)


def from_f16(b):
    return struct.unpack("<%de" % (len(b) // 2), b)
