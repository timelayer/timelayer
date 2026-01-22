# rerank-http (C++ / ONNX Runtime)

A tiny HTTP server that runs an ONNX reranker model via **ONNX Runtime C++** and exposes a single endpoint:

- `POST /v1/rerank` → returns `{"scores": [...]}`

This service is designed to be called by **`tools/rerank-proxy`**, which performs text tokenization and forwards token IDs here.

## API

### `POST /v1/rerank`

Request (JSON):

```json
{
  "input_ids": [[101, 2054, 2003, 102], [101, 2023, 2003, 102]],
  "attention_mask": [[1, 1, 1, 1], [1, 1, 1, 1]],
  "token_type_ids": [[0, 0, 0, 0], [0, 0, 0, 0]],
  "shape": [2, 4]
}
```

Response:

```json
{"scores": [0.12, -1.03]}
```

## Build

### Prerequisites

- CMake ≥ 3.16
- A C++17 compiler
- ONNX Runtime SDK (prebuilt) with headers + shared library

Set **`ORT_ROOT`** to the ORT SDK directory (contains `include/` and `lib/`):

```bash
export ORT_ROOT="$HOME/local-ai/onnxruntime/onnxruntime-osx-arm64-1.23.2"
```

### Build commands

```bash
cd tools/rerank-http
mkdir -p build && cd build
cmake -DORT_ROOT="$ORT_ROOT" ..
cmake --build . -j
```

> This CMake uses **FetchContent** to download header-only deps (`cpp-httplib`, `nlohmann/json`). If you need an offline build, vendor the headers and remove FetchContent.

## Run

```bash
# Required
export RERANK_ONNX_PATH="/abs/path/to/model.onnx"

# Optional
export RERANK_HTTP_HOST="127.0.0.1"   # default
export RERANK_HTTP_PORT="8089"         # default
export RERANK_MAX_BATCH="512"          # default
export RERANK_MAX_SEQ="8192"           # default
export RERANK_RUN_MUTEX="1"            # default

./build/rerank_http \
  --ep cpu \
  --model /Users/kernel/local-ai/models/bge-reranker-v2-m3-onnx/model.onnx


# or
# ⚠️ CoreML EP 要求 ONNX 模型为「单文件且 < 2GB」
# 原因：ONNX 使用 protobuf 存储，存在 2GB message size 的硬限制，
# 超过后在模型加载阶段会直接失败（与内存大小无关）。
./build/rerank_http \
  --ep coreml \
  --model /Users/kernel/local-ai/models/bge-reranker-v2-m3/model_fp16.onnx
```

Health endpoints:

- `GET /health`
- `GET /metrics`

## Notes

- If your model output has shape `[B,2]`, the server will default to the **positive class** (index 1). Override with `RERANK_LOGITS_INDEX`.
- `token_type_ids` is auto-filled with zeros when the model declares it as an input and the request omits it.
