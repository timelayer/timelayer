# rerank-proxy (Python / FastAPI)

A lightweight HTTP proxy that:

1. Receives **text** (`query` + `documents`)
2. Tokenizes with a HuggingFace tokenizer
3. Calls **`rerank-http`** (C++ / ONNX Runtime) with token IDs
4. Returns `{"scores": [...]}` (plus some extra metadata)

TimeLayer can call this service directly via `TIMELAYER_RERANK_URL`.

## Install

```bash
conda create -n rerank-proxy python=3.10 -y
conda activate rerank-proxy
pip install -r requirements.txt
```

## Run

```bash
export RERANK_TOKENIZER_DIR="/path/to/tokenizer_dir"
export CPP_RERANK_URL="http://127.0.0.1:8089/v1/rerank"
export RERANK_PROXY_HOST="127.0.0.1"
export RERANK_PROXY_PORT="8090"

python rerank_proxy.py
```

Health check:

```bash
curl http://127.0.0.1:8090/health
```

## API

### `POST /v1/rerank_text`

Request:

```json
{
  "query": "...",
  "documents": ["...", "..."],
  "top_k": 10,
  "max_length": 512
}
```

Response:

- `scores`: one score per input document (same order as `documents`)
- `ranked_indices`: indices sorted by score desc
- `ranked_documents`: documents sorted by score desc

## Environment variables

- `RERANK_TOKENIZER_DIR` (required): HF tokenizer directory
- `CPP_RERANK_URL` (default: `http://127.0.0.1:8089/v1/rerank`)
- `RERANK_PROXY_HOST` (default: `127.0.0.1`)
- `RERANK_PROXY_PORT` (default: `8090`)
- `RERANK_MAX_LEN` (default: `512`)
- `RERANK_HTTP_TIMEOUT` (default: `10` seconds)
- `RERANK_FIX_MISTRAL_REGEX` (default: `0`): if your Transformers build supports it, pass `fix_mistral_regex=True` into `AutoTokenizer.from_pretrained()`.

## Troubleshooting

- **Tokenizer loads extremely slowly / huge `model_max_length`**
  - Set `RERANK_MAX_LEN=512` and/or provide `max_length` in request.
- **502 from proxy**
  - Verify `rerank-http` is listening and `CPP_RERANK_URL` is correct.

