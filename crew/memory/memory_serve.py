#!/usr/bin/env python3
"""forseti-memory — the shared agent memory service (Phase 10).

Runs in the laya venv (torch/transformers already present). SQLite +
sqlite-vec at ~/.config/forseti/memory.db; embeddings all-MiniLM-L6-v2.

  POST /write  {agent, text, tags?, source?, run?}  → {id}
  POST /recall {query, agent?, k?, since?}          → [{id, ts, agent, text, score}]
  POST /forget {id}                                 → {ok}
  GET  /health                                      → {status, rows, device}

Namespaces: rows carry `agent` ("shared" = cross-agent; anything else is that
agent's private memory). /recall filters by agent: callers see their own rows
+ shared rows, never another agent's private rows.

Advisory context only — never control flow. The service is user-invoked
(scripts/memory-serve.sh); clients degrade with a start hint when it's down.
"""
import json
import os
import sqlite3
import time
from datetime import datetime, timezone

import sqlite_vec
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from sentence_transformers import SentenceTransformer
from typing import Optional

DB_PATH = os.environ.get("FORSETI_MEMORY_DB", os.path.expanduser("~/.config/forseti/memory.db"))
MODEL_NAME = "all-MiniLM-L6-v2"
DIM = 384

_model = None


def model():
    global _model
    if _model is None:
        _model = SentenceTransformer(MODEL_NAME)
    return _model


def db():
    conn = sqlite3.connect(DB_PATH)
    conn.enable_load_extension(True)
    sqlite_vec.load(conn)
    conn.enable_load_extension(False)
    conn.execute(
        "CREATE TABLE IF NOT EXISTS memories ("
        " id INTEGER PRIMARY KEY AUTOINCREMENT,"
        " ts TEXT NOT NULL, agent TEXT NOT NULL, run TEXT, source TEXT,"
        " tags TEXT NOT NULL DEFAULT '[]', text TEXT NOT NULL)"
    )
    try:
        conn.execute(
            "CREATE VIRTUAL TABLE IF NOT EXISTS mem_vec USING vec0("
            f" embedding float[{DIM}] distance_metric=cosine, +mem_id INTEGER)"
        )
    except sqlite3.OperationalError:
        pass  # already created
    return conn


app = FastAPI(title="forseti-memory")


class WriteReq(BaseModel):
    agent: str = Field(min_length=1, max_length=32)
    text: str = Field(min_length=1, max_length=20000)
    tags: list[str] = []
    source: Optional[str] = None
    run: Optional[str] = None


class RecallReq(BaseModel):
    query: str = Field(min_length=1)
    agent: str = "shared"
    k: int = 5
    since: Optional[str] = None
    min_score: float = 0.3  # relevance floor — below it, noise injection


class ForgetReq(BaseModel):
    id: int


@app.get("/health")
def health():
    conn = db()
    n = conn.execute("SELECT COUNT(*) FROM memories").fetchone()[0]
    return {"status": "ok", "rows": n, "model": MODEL_NAME, "dim": DIM}


@app.post("/write")
def write(req: WriteReq):
    emb = model().encode([req.text])[0].tobytes()
    conn = db()
    ts = datetime.now(timezone.utc).isoformat()
    cur = conn.execute(
        "INSERT INTO memories(ts, agent, run, source, tags, text) VALUES (?,?,?,?,?,?)",
        (ts, req.agent, req.run, req.source, json.dumps(req.tags), req.text),
    )
    mem_id = cur.lastrowid
    conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
    conn.commit()
    return {"id": mem_id, "ts": ts}


@app.post("/recall")
def recall(req: RecallReq):
    emb = model().encode([req.query])[0].tobytes()
    conn = db()
    rows = conn.execute(
        "SELECT mem_id, distance FROM mem_vec WHERE embedding MATCH ? AND k = ?"
        " ORDER BY distance",
        (emb, max(1, min(req.k * 4, 64))),  # over-fetch, then filter + trim
    ).fetchall()
    out = []
    for mem_id, distance in rows:
        row = conn.execute(
            "SELECT ts, agent, run, source, tags, text FROM memories WHERE id = ?",
            (mem_id,),
        ).fetchone()
        if not row:
            continue
        ts, agent, run, source, tags, text = row
        # namespace filter: caller's own rows + shared only
        if agent != "shared" and agent != req.agent:
            continue
        if req.since and ts < req.since:
            continue
        out.append({
            "id": mem_id, "ts": ts, "agent": agent, "run": run,
            "source": source, "tags": json.loads(tags), "text": text,
            "score": round(1.0 - float(distance), 4),  # cosine similarity
        })
        if len(out) >= req.k:
            break
    return [r for r in out if r["score"] >= req.min_score]


@app.post("/forget")
def forget(req: ForgetReq):
    conn = db()
    conn.execute("DELETE FROM memories WHERE id = ?", (req.id,))
    conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (req.id,))
    conn.commit()
    return {"ok": True}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ.get("FORSETI_MEMORY_PORT", "8752")),
                log_level="warning")
