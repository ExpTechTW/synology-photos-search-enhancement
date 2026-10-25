"""測試用的假 LiteLLM：/v1/embeddings 與 /v1/chat/completions。

embedding：每個詞對應一個固定的隨機方向。假圖片 FAKE:<概念> 的向量就是那個概念的方向；
查詢「task: search result | query: <詞>」是同方向加一點雜訊（相似度約 0.8），不相關的詞相似度接近 0。
OCR：回傳假圖片裡 TEXT: 後面的文字，沒有就回 NONE。
"""

import base64
import hashlib
import json
import random
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DIM = 768
PREFIX = "task: search result | query: "
DOC_PREFIX = "title: none | text: "
# 讓假的語意比對有「意思相近但字不同」的例子：文字裡的 invoice 跟查詢 receipt 同方向
SYNONYMS = {"invoice": "receipt"}


def direction(word):
    rnd = random.Random(hashlib.sha256(word.encode()).digest())
    v = [rnd.gauss(0, 1) for _ in range(DIM)]
    n = sum(x * x for x in v) ** 0.5
    return [x / n for x in v]


def unit(v):
    n = sum(x * x for x in v) ** 0.5
    return [x / n for x in v]


def parse_fake(img):
    s = img[6 if img[:3] == b"GIF" else 3:].decode("utf-8", "replace")
    concept, _, text = s.partition("|TEXT:")
    return concept.replace("FAKE:", ""), text


def embed_one(inp):
    if isinstance(inp, dict):
        concept, _ = parse_fake(base64.b64decode(inp["multimodal_data"][0]))
        return direction(concept)
    if inp.startswith(DOC_PREFIX):
        # 文件（照片裡的文字）：每個詞的方向加起來，所以「cat food」跟查詢「cat」有一定相似度
        words = [w for w in inp[len(DOC_PREFIX):].replace("/", " ").split() if w]
        vecs = [direction(SYNONYMS.get(w, w)) for w in words] or [direction("empty")]
        return unit([sum(c) for c in zip(*vecs)])
    q = inp[len(PREFIX):] if inp.startswith(PREFIX) else inp
    noise = direction("noise:" + q)
    return unit([0.8 * a + 0.6 * b for a, b in zip(direction(q), noise)])


class FakeLLM:
    def __init__(self):
        self.calls = {"embed_text": 0, "embed_image": 0, "ocr": 0}
        self.fail_ocr = False
        self.delay = 0               # 每個請求花多久（測試是否併發用）
        self.inflight = {"embed": 0, "ocr": 0}
        self.max_inflight = {"embed": 0, "ocr": 0}
        self.max_inputs = 0          # 一個 embedding 請求最多帶了幾筆
        self.lock = threading.Lock()

    def handle(self, path, body):
        kind = "embed" if path.endswith("/embeddings") else "ocr"
        with self.lock:
            self.inflight[kind] += 1
            self.max_inflight[kind] = max(self.max_inflight[kind], self.inflight[kind])
            if kind == "embed":
                self.max_inputs = max(self.max_inputs, len(body["input"]) if isinstance(body["input"], list) else 1)
        try:
            time.sleep(self.delay)
            return self._handle(path, body)
        finally:
            with self.lock:
                self.inflight[kind] -= 1

    def _handle(self, path, body):
        if path.endswith("/embeddings"):
            inputs = body["input"] if isinstance(body["input"], list) else [body["input"]]
            if any(isinstance(x, dict) and parse_fake(base64.b64decode(x["multimodal_data"][0]))[0] == "undecodable"
                   for x in inputs):
                return None  # 跟 llama.cpp 解不開圖片時一樣回 500，但服務本身正常
            for x in inputs:
                self.calls["embed_image" if isinstance(x, dict) else "embed_text"] += 1
            return {"data": [{"index": i, "embedding": embed_one(x)} for i, x in enumerate(inputs)],
                    "model": body["model"], "usage": {"prompt_tokens": 1}}
        if path.endswith("/chat/completions"):
            self.calls["ocr"] += 1
            if self.fail_ocr:
                return None
            url = body["messages"][0]["content"][0]["image_url"]["url"]
            _, text = parse_fake(base64.b64decode(url.split(",", 1)[1]))
            if text.startswith("LONG:"):   # 文字很多：輸出到上限被截斷
                return {"choices": [{"message": {"content": text[5:]}, "finish_reason": "length"}]}
            return {"choices": [{"message": {"content": text or "NONE"}, "finish_reason": "stop"}]}
        return None


def serve(llm):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            out = llm.handle(self.path, body)
            data = json.dumps(out if out is not None else {"error": "fake failure"}).encode()
            self.send_response(200 if out is not None else 500)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv
