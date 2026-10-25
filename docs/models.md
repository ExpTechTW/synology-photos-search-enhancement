# Model servers

[繁體中文](models.zh-TW.md)

The plugin needs one model and can use a second one:

| | Model | API |
|---|---|---|
| **Embedding** (required) | EmbeddingGemma 2 with its vision encoder | OpenAI-compatible `/v1/embeddings` that accepts images (llama.cpp) |
| **OCR** (optional) | Any chat model that can read images | OpenAI-compatible `/v1/chat/completions` with `image_url` |

They run on any machine the NAS can reach: a PC with a GPU, a home server, or a cloud API. The plugin sends them
one request at a time per model, and each request carries one image or one text.

## Embedding: EmbeddingGemma 2 on llama.cpp

EmbeddingGemma 2 maps text and images into the same 768-dimensional space, so a search word can be compared directly
with a photo. Images go through llama.cpp's multimodal prompt format, since OpenAI's `image_url` is not accepted on
`/v1/embeddings`:

```json
{"model": "embeddinggemma-2", "input": [{"prompt_string": "<__media__>", "multimodal_data": ["<base64>"]}]}
```

### What you need

1. **llama.cpp build b11718 or newer** (the first one with EmbeddingGemma 2 support).
2. **Two files** from [unsloth/embeddinggemma-2-GGUF](https://huggingface.co/unsloth/embeddinggemma-2-GGUF):
   the model `embeddinggemma-2-BF16.gguf` and the vision encoder `mmproj-BF16.gguf`.
3. **`LLAMA_MEDIA_MARKER=<__media__>`** in llama-server's environment. Otherwise llama-server picks a random marker
   at every start, and image requests stop working. The value must match `embedding.media_marker` in `config.yml`.
4. `--embeddings --pooling mean`, and a batch large enough for one whole input: `-b 8192 -ub 8192`.

### Precision: never FP16

EmbeddingGemma's activations overflow FP16. Depending on the backend you get `NaN`, or worse, vectors that look fine
but are silently degraded. The plugin rejects `NaN` vectors with an error that points here.

| Hardware | Use |
|---|---|
| NVIDIA Ampere (RTX 30 series) or newer, CPU | The BF16 files as they are |
| Older NVIDIA GPUs (Pascal, Turing…) without BF16 | Convert both files to **F32** (below), and use `--flash-attn off --cache-type-k f32 --cache-type-v f32` |

Do not use the F16 files. The quantized files (Q8_0, Q4…) have not been tested, and on GPUs they compute in FP16 too.

Converting to F32 (lossless, about twice the size of BF16):

```sh
cd models
docker run --rm -v "$PWD":/models ghcr.io/ggml-org/llama.cpp:full-b11718 \
    --quantize /models/embeddinggemma-2-BF16.gguf /models/embeddinggemma-2-F32.gguf F32
docker run --rm -v "$PWD":/models ghcr.io/ggml-org/llama.cpp:full-b11718 \
    --quantize /models/mmproj-BF16.gguf /models/mmproj-embeddinggemma-2-F32.gguf F32
```

### Running it

A ready-to-use Docker Compose file for an NVIDIA GPU is in
[deploy/llama.cpp/docker-compose.yml](../deploy/llama.cpp/docker-compose.yml):

```sh
mkdir -p models && cd models
curl -LO https://huggingface.co/unsloth/embeddinggemma-2-GGUF/resolve/main/embeddinggemma-2-BF16.gguf
curl -LO https://huggingface.co/unsloth/embeddinggemma-2-GGUF/resolve/main/mmproj-BF16.gguf
cd .. && docker compose up -d
```

Without Docker:

```sh
LLAMA_MEDIA_MARKER='<__media__>' llama-server \
    -m embeddinggemma-2-BF16.gguf --mmproj mmproj-BF16.gguf --alias embeddinggemma-2 \
    --embeddings --pooling mean -c 8192 -np 1 -b 8192 -ub 8192 -ngl 99 --host 0.0.0.0 --port 8080
```

`-np 1` is enough for this plugin, because it never sends two embedding requests at once. If other programs share the
server, raise `-np` and set `-c` to `-np × 8192`.

Check that it works:

```sh
curl http://SERVER:8080/v1/embeddings -H 'Content-Type: application/json' \
    -d '{"model": "embeddinggemma-2", "input": ["task: search result | query: cat"]}'
```

Then set `embedding.base_url: http://SERVER:8080/v1` in `config.yml`. The installer, and `sudo sse apply --check`,
test both text and image embedding.

### Through a proxy

A proxy such as LiteLLM works as long as it passes the `input` objects through unchanged; this has been tested with
LiteLLM. Use the proxy's URL as `base_url`, its key as `api_key`, and its model name as `model`.

## OCR: any vision chat model

OCR reads the text in each photo (receipts, signs, screenshots…) so that it can be searched word for word and by
meaning. Any OpenAI-compatible server that takes `image_url` content works: Ollama, LM Studio, vLLM, llama.cpp with a
vision model, LiteLLM, OpenAI, and so on.

```yaml
ocr:
  enabled: true
  model: gemma3:12b                     # the server's model name
  base_url: http://SERVER:11434/v1      # empty = the embedding server
  api_key: ""
```

- **Which model**: OCR quality matters more than size. Developed with Gemma 4 26B A4B; other current vision models
  (Gemma 3, Qwen2.5-VL / Qwen3-VL, GPT-4o mini…) should work too. Try one with
  `sudo sse apply --check`, which runs OCR on a test image.
- **Thinking models**: turn thinking off, or OCR becomes slow and expensive. Use `ocr.extra_body`, which is merged into
  every request, for example `{reasoning_effort: none}`, or `{chat_template_kwargs: {enable_thinking: false}}` for
  Qwen3 on llama.cpp / vLLM. Check your server's documentation.
- **Image size**: `ocr.image_size: xl` (default). Small text needs the larger thumbnail.
- **Long text**: `ocr.max_tokens` (default 4096). Text cut off at the limit is kept up to that point.
- Animated GIFs are skipped. Answers like "I can't read this" or the prompt repeated back are discarded as empty.

OCR is the slowest step, so it runs only after every photo has its image embedding: content search becomes usable
first. Text found by OCR is also turned into a vector (`ocr.text_embedding`), so searching "receipt" can find a
screenshot that says "invoice".

## Cloud APIs

Both parts work with hosted APIs, but every photo's thumbnail is then uploaded to that provider, and indexing a large
library means tens of thousands of requests. Check the provider's privacy terms and pricing first.
