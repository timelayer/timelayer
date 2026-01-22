import os
import time
import inspect
from typing import List, Optional, Dict, Any

import requests
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from transformers import AutoTokenizer

# ----------------------------
# Config (env)
# ----------------------------
TOKENIZER_DIR = os.environ.get(
    "RERANK_TOKENIZER_DIR",
    os.path.expanduser("~/local-ai/models/bge-reranker-v2-m3-onnx"),
)

CPP_RERANK_URL = os.environ.get(
    "CPP_RERANK_URL",
    "http://127.0.0.1:8089/v1/rerank",
)

PROXY_HOST = os.environ.get("RERANK_PROXY_HOST", "127.0.0.1")
PROXY_PORT = int(os.environ.get("RERANK_PROXY_PORT", "8090"))

DEFAULT_MAX_LEN = int(os.environ.get("RERANK_MAX_LEN", "512"))
HTTP_TIMEOUT = float(os.environ.get("RERANK_HTTP_TIMEOUT", "10"))

# Transformers security hardening / compatibility knobs
USE_FAST = os.environ.get("RERANK_USE_FAST", "1").strip() not in ("0", "false", "no")
TRUST_REMOTE_CODE = os.environ.get("RERANK_TRUST_REMOTE_CODE", "0").strip() in ("1", "true", "yes")
FIX_MISTRAL_REGEX = os.environ.get("RERANK_FIX_MISTRAL_REGEX", "0").strip() in ("1", "true", "yes")

# ----------------------------
# HTTP client (keep-alive)
# ----------------------------
_http = requests.Session()

# ----------------------------
# Load tokenizer (once)
# ----------------------------
_t0 = time.time()
_tok_kwargs = {
    "use_fast": USE_FAST,
    "trust_remote_code": TRUST_REMOTE_CODE,
}
# Some transformers versions support fix_mistral_regex; set it only when accepted.
if FIX_MISTRAL_REGEX:
    try:
        sig = inspect.signature(AutoTokenizer.from_pretrained)
        if "fix_mistral_regex" in sig.parameters:
            _tok_kwargs["fix_mistral_regex"] = True
    except Exception:
        pass

try:
    tokenizer = AutoTokenizer.from_pretrained(TOKENIZER_DIR, **_tok_kwargs)
except Exception as e:
    raise RuntimeError(
        f"Failed to load tokenizer from {TOKENIZER_DIR}: {e}\n"
        "Hint: set RERANK_TOKENIZER_DIR to a HuggingFace tokenizer directory."
    )

model_max_len = getattr(tokenizer, "model_max_length", None)
if not model_max_len or model_max_len > 100000:
    model_max_len = DEFAULT_MAX_LEN

load_cost = time.time() - _t0

# ----------------------------
# FastAPI
# ----------------------------
app = FastAPI(title="rerank-proxy", version="1.1.0")


class RerankTextRequest(BaseModel):
    query: str = Field(..., description="Query text")
    documents: List[str] = Field(..., description="Candidate documents", min_length=1)
    top_k: Optional[int] = Field(None, description="Return top_k results (default all)")
    max_length: Optional[int] = Field(None, description="Tokenizer max_length override")


class RerankTextResponse(BaseModel):
    scores: List[float]
    ranked_indices: List[int]
    ranked_documents: List[str]
    meta: Dict[str, Any]


@app.get("/health")
def health():
    return {
        "ok": True,
        "tokenizer_dir": TOKENIZER_DIR,
        "tokenizer_loaded_sec": round(load_cost, 3),
        "tokenizer_type": tokenizer.__class__.__name__,
        "model_max_length": int(model_max_len),
        "cpp_rerank_url": CPP_RERANK_URL,
        "http_timeout_sec": HTTP_TIMEOUT,
        "listening": f"http://{PROXY_HOST}:{PROXY_PORT}",
    }


def _call_cpp_reranker(payload: dict) -> List[float]:
    try:
        r = _http.post(CPP_RERANK_URL, json=payload, timeout=HTTP_TIMEOUT)
    except requests.RequestException as e:
        raise HTTPException(status_code=502, detail=f"cpp reranker request failed: {e}")

    if r.status_code != 200:
        try:
            err = r.json()
        except Exception:
            err = {"error": r.text.strip()}
        raise HTTPException(status_code=502, detail={"cpp_status": r.status_code, "cpp_error": err})

    try:
        data = r.json()
    except Exception:
        raise HTTPException(status_code=502, detail="cpp reranker returned non-json response")

    scores = data.get("scores")
    if not isinstance(scores, list):
        raise HTTPException(status_code=502, detail=f"cpp reranker response missing scores: {data}")

    try:
        return [float(x) for x in scores]
    except Exception:
        raise HTTPException(status_code=502, detail=f"cpp reranker scores not numeric: {scores[:3]}...")


@app.post("/v1/rerank_text", response_model=RerankTextResponse)
def rerank_text(req: RerankTextRequest):
    query = (req.query or "").strip()
    docs = [d.strip() for d in (req.documents or []) if d is not None and d.strip()]

    if not query:
        raise HTTPException(status_code=400, detail="query is empty")
    if not docs:
        raise HTTPException(status_code=400, detail="documents is empty")

    batch_size = len(docs)

    max_len = req.max_length or model_max_len
    if max_len <= 0:
        max_len = 1
    if max_len > 4096:
        max_len = 4096

    t1 = time.time()
    try:
        enc = tokenizer(
            [query] * batch_size,
            docs,
            padding=True,
            truncation=True,
            max_length=int(max_len),
            return_tensors=None,
        )
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"tokenizer failed: {e}")
    tok_cost = time.time() - t1

    if "input_ids" not in enc or "attention_mask" not in enc:
        raise HTTPException(status_code=500, detail=f"tokenizer output missing fields: {list(enc.keys())}")

    input_ids = enc["input_ids"]
    attention_mask = enc["attention_mask"]
    token_type_ids = enc.get("token_type_ids")

    B = len(input_ids)
    S = len(input_ids[0]) if B > 0 else 0
    if B != batch_size or S <= 0:
        raise HTTPException(status_code=500, detail=f"unexpected tokenized shape: B={B}, S={S}")

    payload = {
        "shape": [B, S],
        "input_ids": input_ids,
        "attention_mask": attention_mask,
    }
    if token_type_ids is not None:
        payload["token_type_ids"] = token_type_ids

    t2 = time.time()
    scores = _call_cpp_reranker(payload)
    cpp_cost = time.time() - t2

    if len(scores) != B:
        raise HTTPException(status_code=502, detail=f"cpp scores length mismatch: {len(scores)} != {B}")

    ranked_indices = sorted(range(B), key=lambda i: scores[i], reverse=True)

    top_k = req.top_k if req.top_k is not None else B
    if top_k <= 0:
        top_k = B
    top_k = min(top_k, B)

    ranked_indices = ranked_indices[:top_k]
    ranked_docs = [docs[i] for i in ranked_indices]

    return {
        "scores": scores,
        "ranked_indices": ranked_indices,
        "ranked_documents": ranked_docs,
        "meta": {
            "B": B,
            "S": S,
            "max_length": int(max_len),
            "tokenizer_dir": TOKENIZER_DIR,
            "cpp_rerank_url": CPP_RERANK_URL,
            "timing_sec": {
                "tokenize": round(tok_cost, 4),
                "cpp_rerank": round(cpp_cost, 4),
            },
            "tokenizer_keys": list(enc.keys()),
        },
    }


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=PROXY_HOST, port=PROXY_PORT)
