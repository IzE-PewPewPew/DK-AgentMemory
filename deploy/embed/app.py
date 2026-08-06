"""Embedding sidecar for DevKuong Memories.

A separate process by design. The embedding model is the only part of this
system that loads hundreds of megabytes of weights, allocates unpredictably,
and can be killed by the OOM reaper. Isolating it means that when it dies, the
API keeps serving: keyword search still works, writes still land, and the
backfill pass vectorises the gap once the sidecar comes back.

Contract, which internal/embed/embed.go depends on:

    POST /embed   {"texts": ["..."]}  ->  {"embeddings": [[...]], "dimensions": N}
    GET  /health                      ->  {"ok": true, "model": "...", "dimensions": N}
"""

from __future__ import annotations

import os
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

MODEL_NAME = os.environ.get("EMBED_MODEL", "BAAI/bge-small-en-v1.5")

# Bounded so one request cannot decide how much memory this process uses. The
# Go client batches to embedding.batch_size (32 by default), well under this.
MAX_TEXTS = 256
MAX_CHARS = 32_000

_state: dict[str, object] = {}


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Loaded once at startup rather than lazily on first request. A cold model
    # load takes tens of seconds, and paying that inside a request means the
    # first search after every restart times out.
    from fastembed import TextEmbedding

    model = TextEmbedding(model_name=MODEL_NAME)
    probe = list(model.embed(["dimension probe"]))[0]

    _state["model"] = model
    _state["dimensions"] = len(probe)
    print(f"embed: loaded {MODEL_NAME} ({len(probe)} dimensions)", flush=True)

    yield

    _state.clear()


api = FastAPI(title="dkm-embed", lifespan=lifespan)


class EmbedRequest(BaseModel):
    texts: list[str] = Field(default_factory=list)
    model: str | None = None


class EmbedResponse(BaseModel):
    embeddings: list[list[float]]
    dimensions: int
    model: str


@api.get("/health")
def health() -> dict:
    if "model" not in _state:
        # 503 while the model is still loading, so an orchestrator waits rather
        # than routing traffic to a process that cannot answer yet.
        raise HTTPException(status_code=503, detail="model is still loading")
    return {"ok": True, "model": MODEL_NAME, "dimensions": _state["dimensions"]}


@api.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest) -> EmbedResponse:
    if "model" not in _state:
        raise HTTPException(status_code=503, detail="model is still loading")

    if not req.texts:
        return EmbedResponse(embeddings=[], dimensions=_state["dimensions"], model=MODEL_NAME)

    if len(req.texts) > MAX_TEXTS:
        raise HTTPException(
            status_code=413,
            detail=f"{len(req.texts)} texts in one request; the maximum is {MAX_TEXTS}",
        )

    # Truncate rather than reject. A memory whose body is a pasted stack trace
    # is still worth embedding on its first few thousand characters, and the
    # alternative is a write path that fails on large inputs.
    texts = [t[:MAX_CHARS] if t else " " for t in req.texts]

    vectors = [v.tolist() for v in _state["model"].embed(texts)]

    # The Go client checks this too. Checking here as well means a model swap
    # is caught at the sidecar rather than as a Postgres error on every insert.
    if vectors and len(vectors[0]) != _state["dimensions"]:
        raise HTTPException(
            status_code=500,
            detail=f"model returned {len(vectors[0])} dimensions, expected {_state['dimensions']}",
        )

    return EmbedResponse(
        embeddings=vectors,
        dimensions=_state["dimensions"],
        model=MODEL_NAME,
    )
