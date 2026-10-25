# 模型服務

[English](models.md)

外掛需要一個模型，另一個選用：

| | 模型 | API |
|---|---|---|
| **Embedding**（必要） | EmbeddingGemma 2 加上視覺編碼器 | OpenAI 相容、能吃圖片的 `/v1/embeddings`（llama.cpp） |
| **OCR**（選用） | 任何能看圖的聊天模型 | OpenAI 相容的 `/v1/chat/completions`，支援 `image_url` |

服務可以架在 NAS 連得到的任何地方：有 GPU 的電腦、家裡的伺服器，或雲端 API。每個模型外掛一次只送一個請求，
每個請求只有一張圖或一段文字。

## Embedding：在 llama.cpp 上跑 EmbeddingGemma 2

EmbeddingGemma 2 把文字和圖片放進同一個 768 維空間，所以搜尋字可以直接跟照片比對。圖片用的是 llama.cpp 的
multimodal prompt 格式（`/v1/embeddings` 不接受 OpenAI 的 `image_url`）：

```json
{"model": "embeddinggemma-2", "input": [{"prompt_string": "<__media__>", "multimodal_data": ["<base64>"]}]}
```

### 需要

1. **llama.cpp b11718 以後的版本**（從這版開始支援 EmbeddingGemma 2）。
2. [unsloth/embeddinggemma-2-GGUF](https://huggingface.co/unsloth/embeddinggemma-2-GGUF) 的**兩個檔案**：
   模型 `embeddinggemma-2-BF16.gguf` 和視覺編碼器 `mmproj-BF16.gguf`。
3. llama-server 的環境變數 **`LLAMA_MEDIA_MARKER=<__media__>`**。沒設的話 llama-server 每次啟動都會用隨機的 marker，
   圖片請求就會失效。這個值要跟 `config.yml` 的 `embedding.media_marker` 相同。
4. `--embeddings --pooling mean`，batch 要放得下一整段輸入：`-b 8192 -ub 8192`。

### 精度：不能用 FP16

EmbeddingGemma 的 activation 會超出 FP16 的範圍。依後端不同，會得到 `NaN`，或更糟：看起來正常、其實已經劣化的向量。
外掛遇到 `NaN` 會拒絕，錯誤訊息會指向這份文件。

| 硬體 | 用法 |
|---|---|
| NVIDIA Ampere（RTX 30 系列）以後的 GPU、CPU | 直接用 BF16 檔案 |
| 不支援 BF16 的舊 GPU（Pascal、Turing…） | 兩個檔案都轉成 **F32**（如下），並加上 `--flash-attn off --cache-type-k f32 --cache-type-v f32` |

不要用 F16 檔案。量化版（Q8_0、Q4…）沒有測試過，而且在 GPU 上一樣會用 FP16 計算。

轉成 F32（無損，大小約是 BF16 的兩倍）：

```sh
cd models
docker run --rm -v "$PWD":/models ghcr.io/ggml-org/llama.cpp:full-b11718 \
    --quantize /models/embeddinggemma-2-BF16.gguf /models/embeddinggemma-2-F32.gguf F32
docker run --rm -v "$PWD":/models ghcr.io/ggml-org/llama.cpp:full-b11718 \
    --quantize /models/mmproj-BF16.gguf /models/mmproj-embeddinggemma-2-F32.gguf F32
```

### 啟動

NVIDIA GPU 用的 Docker Compose 檔在 [deploy/llama.cpp/docker-compose.yml](../deploy/llama.cpp/docker-compose.yml)：

```sh
mkdir -p models && cd models
curl -LO https://huggingface.co/unsloth/embeddinggemma-2-GGUF/resolve/main/embeddinggemma-2-BF16.gguf
curl -LO https://huggingface.co/unsloth/embeddinggemma-2-GGUF/resolve/main/mmproj-BF16.gguf
cd .. && docker compose up -d
```

不用 Docker：

```sh
LLAMA_MEDIA_MARKER='<__media__>' llama-server \
    -m embeddinggemma-2-BF16.gguf --mmproj mmproj-BF16.gguf --alias embeddinggemma-2 \
    --embeddings --pooling mean -c 8192 -np 1 -b 8192 -ub 8192 -ngl 99 --host 0.0.0.0 --port 8080
```

外掛不會同時送兩個 embedding 請求，所以 `-np 1` 就夠了。有其他程式共用這個服務的話，調高 `-np`，`-c` 設成 `-np × 8192`。

確認服務正常：

```sh
curl http://SERVER:8080/v1/embeddings -H 'Content-Type: application/json' \
    -d '{"model": "embeddinggemma-2", "input": ["task: search result | query: cat"]}'
```

然後在 `config.yml` 設定 `embedding.base_url: http://SERVER:8080/v1`。安裝程式和 `sudo sse apply --check`
都會測試文字與圖片 embedding。

### 經過代理

LiteLLM 這類代理只要原封不動轉送 `input` 物件就可以用（LiteLLM 實測可用）。`base_url` 填代理的網址、`api_key`
填代理的 key、`model` 填代理上的模型名稱。

## OCR：任何能看圖的聊天模型

OCR 讀出每張照片裡的文字（發票、招牌、截圖…），之後就能逐字搜尋，也能用意思搜尋。任何支援 `image_url` 的
OpenAI 相容服務都可以：Ollama、LM Studio、vLLM、跑視覺模型的 llama.cpp、LiteLLM、OpenAI 等。

```yaml
ocr:
  enabled: true
  model: gemma3:12b                     # 服務上的模型名稱
  base_url: http://SERVER:11434/v1      # 空白 = 跟 embedding 同一個服務
  api_key: ""
```

- **選哪個模型**：OCR 的品質比模型大小重要。開發時用的是 Gemma 4 26B A4B；其他新的視覺模型
  （Gemma 3、Qwen2.5-VL / Qwen3-VL、GPT-4o mini…）應該也可以。用 `sudo sse apply --check` 試，它會拿一張測試圖做 OCR。
- **會思考的模型**：要關掉思考，不然 OCR 又慢又貴。用 `ocr.extra_body`（會併進每個請求），例如
  `{reasoning_effort: none}`，或是 llama.cpp / vLLM 上的 Qwen3 用 `{chat_template_kwargs: {enable_thinking: false}}`。
  請參考你的服務的說明。
- **圖片大小**：`ocr.image_size: xl`（預設）。小字需要大一點的縮圖。
- **很長的文字**：`ocr.max_tokens`（預設 4096）。超過上限被截斷的話，保留截斷前的部分。
- 動圖（GIF）會跳過。模型回「我看不到文字」之類的回答、或把提示詞原樣回傳，都當成沒有文字。

OCR 是最慢的一步，所以等每張照片都做完圖片 embedding 才開始，讓內容搜尋先能用。OCR 讀出的文字也會做成向量
（`ocr.text_embedding`），所以搜「收據」也找得到文字寫著「電子發票」的截圖。

## 雲端 API

兩者都可以用雲端 API，但每張照片的縮圖都會上傳到那家服務，照片多的話是好幾萬個請求。請先確認對方的隱私條款與價格。
