#!/usr/bin/env python3
"""forseti-memory — the shared agent memory service (Phase 10, v2 Phase 12).

Runs in the laya venv (torch/transformers already present). SQLite +
sqlite-vec + FTS5 at ~/.config/forseti/memory.db; embeddings all-MiniLM-L6-v2
(override with FORSETI_MEMORY_MODEL — the 12-6 evaluation dial).

  POST /write  {agent, text, tags?, source?, run?, type?, importance?,
                supersedes?, expires_at?}                     → {id}
  POST /recall {query, agent?, k?, since?, min_score?, type?,
                include_superseded?, expand?}                 → [rows]
  POST /forget {id}                                          → {ok}
  GET  /export                                               → JSONL dump
  POST /import                                               ← JSONL
  GET  /health                                               → {status, rows, ...}

v2 (S23 survey, docs/spikes.md): hybrid retrieval — sqlite-vec cosine + FTS5
BM25 fused by RRF (k=60); semantic pre-gate (min_score) BEFORE fusion (Mem0's
ordering: keyword can never rescue a semantically-dead candidate); recency +
importance soft boosts (Generative Agents); write-time dedup (exact hash +
near-dup cosine ≥ 0.95 → merge, not insert); supersede-not-delete (Zep: the
new fact marks the old one, history stays queryable); TTL; associative links
(A-MEM light: stored adjacency + bounded "box" expansion of the top hit).

Namespaces: rows carry `agent` ("shared" = cross-agent; anything else is that
agent's private memory). /recall filters by agent: callers see their own rows
+ shared rows, never another agent's private rows. Link expansion obeys the
same rule — a link into someone else's private rows resolves to nothing.

Advisory context only — never control flow. The service is user-invoked
(scripts/memory-serve.sh); clients degrade with a start hint when it's down.
"""
import hashlib
import json
import os
import re
import sqlite3
import time
from datetime import datetime, timezone

import sqlite_vec
from fastapi import FastAPI, HTTPException, Response
from pydantic import BaseModel, Field
from sentence_transformers import SentenceTransformer
from typing import Optional

DB_PATH = os.environ.get("FORSETI_MEMORY_DB", os.path.expanduser("~/.config/forseti/memory.db"))
MODEL_NAME = os.environ.get("FORSETI_MEMORY_MODEL", "all-MiniLM-L6-v2")
DIM = 384
SCHEMA_VERSION = 2

# S23 calibration caveat: every published constant was tuned on 1024-d
# embeddings — keep them in env config, never in code.
RRF_K = float(os.environ.get("FORSETI_MEMORY_RRF_K", "60"))
DEDUP_SIM = float(os.environ.get("FORSETI_MEMORY_DEDUP", "0.95"))  # Mem0's near-dup gate
RECENCY_DECAY = float(os.environ.get("FORSETI_MEMORY_RECENCY_DECAY", "0.995"))  # per hour
W_RECENCY = float(os.environ.get("FORSETI_MEMORY_W_RECENCY", "0.5"))
W_IMPORTANCE = float(os.environ.get("FORSETI_MEMORY_W_IMPORTANCE", "0.5"))
LINK_MAX = int(os.environ.get("FORSETI_MEMORY_LINK_MAX", "10"))  # bounded box expansion

VALID_TYPES = {"fact", "episode", "procedure", "preference"}

_model = None


def model():
    global _model
    if _model is None:
        _model = SentenceTransformer(MODEL_NAME)
    return _model


def now_iso():
    return datetime.now(timezone.utc).isoformat()


def fts_query(q: str) -> str:
    """FTS5-safe query: bare tokens only (MATCH syntax errors on punctuation)."""
    return " ".join(re.findall(r"\w+", q))


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
    _migrate(conn)
    conn.commit()
    return conn


def _migrate(conn):
    """PRAGMA user_version-gated migration. ALTERs are idempotent (a partially
    applied migration re-runs cleanly: duplicate-column errors are skipped)."""
    ver = conn.execute("PRAGMA user_version").fetchone()[0]
    if ver >= SCHEMA_VERSION:
        return
    for col, ddl in [
        ("hash", "TEXT"),
        ("type", "TEXT NOT NULL DEFAULT 'fact'"),
        ("importance", "REAL NOT NULL DEFAULT 0.5"),
        ("access_count", "INTEGER NOT NULL DEFAULT 0"),
        ("last_access", "TEXT"),
        ("superseded_by", "INTEGER"),
        ("expires_at", "TEXT"),
        ("links", "TEXT NOT NULL DEFAULT '[]'"),
    ]:
        try:
            conn.execute(f"ALTER TABLE memories ADD COLUMN {col} {ddl}")
        except sqlite3.OperationalError:
            pass  # column exists (partially applied migration)
    # backfill hashes for pre-v2 rows
    for mem_id, text in conn.execute("SELECT id, text FROM memories WHERE hash IS NULL").fetchall():
        conn.execute("UPDATE memories SET hash = ? WHERE id = ?", (md5(text), mem_id))
    # FTS5 external-content index: stores only the inverted index; triggers keep
    # it in sync (official FTS5 pattern; rebuild backfills pre-existing rows).
    conn.execute(
        "CREATE VIRTUAL TABLE IF NOT EXISTS mem_fts USING fts5("
        " text, content='memories', content_rowid='id', tokenize='porter unicode61')"
    )
    conn.execute(
        "CREATE TRIGGER IF NOT EXISTS mem_fts_ai AFTER INSERT ON memories BEGIN"
        " INSERT INTO mem_fts(rowid, text) VALUES (new.id, new.text); END"
    )
    conn.execute(
        "CREATE TRIGGER IF NOT EXISTS mem_fts_ad AFTER DELETE ON memories BEGIN"
        " INSERT INTO mem_fts(mem_fts, rowid, text) VALUES ('delete', old.id, old.text); END"
    )
    conn.execute(
        "CREATE TRIGGER IF NOT EXISTS mem_fts_au AFTER UPDATE ON memories BEGIN"
        " INSERT INTO mem_fts(mem_fts, rowid, text) VALUES ('delete', old.id, old.text);"
        " INSERT INTO mem_fts(rowid, text) VALUES (new.id, new.text); END"
    )
    conn.execute("INSERT INTO mem_fts(mem_fts) VALUES('rebuild')")
    conn.execute(f"PRAGMA user_version = {SCHEMA_VERSION}")


def md5(text: str) -> str:
    return hashlib.md5(text.encode()).hexdigest()


app = FastAPI(title="forseti-memory")


class WriteReq(BaseModel):
    agent: str = Field(min_length=1, max_length=32)
    text: str = Field(min_length=1, max_length=20000)
    tags: list[str] = []
    source: Optional[str] = None
    run: Optional[str] = None
    type: str = "fact"          # fact|episode|procedure|preference
    importance: float = 0.5     # 0–1
    supersedes: Optional[int] = None  # id of the fact this one replaces
    expires_at: Optional[str] = None  # ISO timestamp
    links: list[int] = []       # associative links (A-MEM light)


class RecallReq(BaseModel):
    query: str = Field(min_length=1)
    agent: str = "shared"
    k: int = 5
    since: Optional[str] = None
    min_score: float = 0.3  # semantic pre-gate — below it, noise injection
    type: Optional[str] = None        # memory-type filter
    include_superseded: bool = False
    expand: bool = True  # bounded A-MEM box expansion of the top hit


class ForgetReq(BaseModel):
    id: int


@app.get("/health")
def health():
    conn = db()
    n = conn.execute("SELECT COUNT(*) FROM memories").fetchone()[0]
    ver = conn.execute("PRAGMA user_version").fetchone()[0]
    return {"status": "ok", "rows": n, "model": MODEL_NAME, "dim": DIM,
            "schema": ver}


@app.post("/write")
def write(req: WriteReq):
    if req.type not in VALID_TYPES:
        raise HTTPException(status_code=422, detail=f"type must be one of {sorted(VALID_TYPES)}")
    if not 0 <= req.importance <= 1:
        raise HTTPException(status_code=422, detail="importance must be in [0,1]")
    emb = model().encode([req.text])[0].tobytes()
    conn = db()
    ts = now_iso()
    digest = md5(req.text)

    # explicit supersedes (Zep semantics) wins over every heuristic: insert the
    # new fact as current and mark the OLD row superseded (history queryable).
    if req.supersedes is not None:
        cur = conn.execute(
            "INSERT INTO memories(ts, agent, run, source, tags, text, hash, type,"
            " importance, superseded_by, expires_at, links) VALUES"
            " (?,?,?,?,?,?,?,?,?,NULL,?,?)",
            (ts, req.agent, req.run, req.source, json.dumps(req.tags), req.text,
             digest, req.type, req.importance, req.expires_at, json.dumps(req.links)),
        )
        mem_id = cur.lastrowid
        conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
        conn.execute(
            "UPDATE memories SET superseded_by = ? WHERE id = ? AND superseded_by IS NULL",
            (mem_id, req.supersedes),
        )
        conn.commit()
        return {"id": mem_id, "ts": ts, "supersedes": req.supersedes}

    # exact dedup: same text in the same namespace is the same memory
    row = conn.execute(
        "SELECT id FROM memories WHERE agent = ? AND hash = ? AND superseded_by IS NULL",
        (req.agent, digest),
    ).fetchone()
    if row:
        conn.execute("UPDATE memories SET ts = ? WHERE id = ?", (ts, row[0]))
        conn.commit()
        return {"id": row[0], "ts": ts, "dedup": "exact"}

    # near-dup merge (Mem0's 0.95 gate, inside the caller's namespace): update
    # the existing row instead of accreting near-identical facts
    near = conn.execute(
        "SELECT mem_id, distance FROM mem_vec WHERE embedding MATCH ? AND k = ?"
        " ORDER BY distance",
        (emb, 20),
    ).fetchall()
    for mem_id, distance in near:
        r = conn.execute(
            "SELECT agent, superseded_by FROM memories WHERE id = ?", (mem_id,)
        ).fetchone()
        if not r or r[0] != req.agent or r[1] is not None:
            continue
        if 1.0 - float(distance) >= DEDUP_SIM:
            old_tags = json.loads(conn.execute(
                "SELECT tags FROM memories WHERE id = ?", (mem_id,)
            ).fetchone()[0])
            merged = sorted(set(old_tags) | set(req.tags))
            conn.execute(
                "UPDATE memories SET ts = ?, text = ?, hash = ?, tags = ?,"
                " type = ?, importance = ? WHERE id = ?",
                (ts, req.text, digest, json.dumps(merged), req.type, req.importance, mem_id),
            )
            conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (mem_id,))
            conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
            conn.commit()
            return {"id": mem_id, "ts": ts, "dedup": "near"}

    cur = conn.execute(
        "INSERT INTO memories(ts, agent, run, source, tags, text, hash, type,"
        " importance, superseded_by, expires_at, links)"
        " VALUES (?,?,?,?,?,?,?,?,?,NULL,?,?)",
        (ts, req.agent, req.run, req.source, json.dumps(req.tags), req.text,
         digest, req.type, req.importance, req.expires_at, json.dumps(req.links)),
    )
    mem_id = cur.lastrowid
    conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
    conn.commit()
    return {"id": mem_id, "ts": ts}


def _visible(conn, mem_id, req):
    """Namespace + filter predicates for one candidate row (returns row or None)."""
    r = conn.execute(
        "SELECT ts, agent, run, source, tags, text, type, importance,"
        " superseded_by, expires_at, links FROM memories WHERE id = ?", (mem_id,)
    ).fetchone()
    if not r:
        return None
    ts, agent, run, source, tags, text, mtype, importance, sup, expires, links = r
    if agent != "shared" and agent != req.agent:  # namespace isolation
        return None
    if req.since and ts < req.since:
        return None
    if req.type and mtype != req.type:
        return None
    if sup is not None and not req.include_superseded:
        return None
    if expires and expires < now_iso():
        return None
    return {"id": mem_id, "ts": ts, "agent": agent, "run": run,
            "source": source, "tags": json.loads(tags), "text": text,
            "type": mtype, "importance": importance, "superseded_by": sup,
            "links": json.loads(links)}


@app.post("/recall")
def recall(req: RecallReq):
    emb = model().encode([req.query])[0].tobytes()
    conn = db()
    of = max(req.k * 4, 60)  # Mem0's over-fetch shape
    of = min(of, 200)

    # leg 1: vector cosine (rank order = ascending distance)
    v_leg = []
    for mem_id, distance in conn.execute(
        "SELECT mem_id, distance FROM mem_vec WHERE embedding MATCH ? AND k = ?"
        " ORDER BY distance", (emb, of),
    ).fetchall():
        v_leg.append((mem_id, 1.0 - float(distance)))

    # leg 2: FTS5 BM25 (rank order = ascending rank; bm25() is lower=better)
    f_leg = []
    fq = fts_query(req.query)
    if fq:
        for (mem_id,) in conn.execute(
            "SELECT rowid FROM mem_fts WHERE text MATCH ? ORDER BY rank LIMIT ?",
            (fq, of),
        ).fetchall():
            f_leg.append((mem_id, None))

    # semantic pre-gate BEFORE fusion (Mem0): keyword can never rescue a
    # semantically-dead candidate. FTS-only candidates carry no cosine and
    # pass the gate (their signal IS lexical presence).
    gate = {mem_id for mem_id, sim in v_leg if sim >= req.min_score}
    gate |= {mem_id for mem_id, _ in f_leg}

    # RRF fusion (k=60): rank-only — robust to incomparable scales
    rrf = {}
    sim_of = {}
    for rank, (mem_id, sim) in enumerate(v_leg):
        if mem_id in gate:
            rrf[mem_id] = rrf.get(mem_id, 0.0) + 1.0 / (RRF_K + rank + 1)
            sim_of[mem_id] = sim
    for rank, (mem_id, _) in enumerate(f_leg):
        if mem_id in gate:
            rrf[mem_id] = rrf.get(mem_id, 0.0) + 1.0 / (RRF_K + rank + 1)

    # soft boosts (Generative Agents): recency from last access, writer importance
    max_rrf = 2.0 / (RRF_K + 1.0)
    now = datetime.now(timezone.utc)
    out = []
    hit_ids = []
    for mem_id, score in sorted(rrf.items(), key=lambda kv: -kv[1]):
        row = _visible(conn, mem_id, req)
        if not row:
            continue
        last = row["ts"]  # never accessed → creation time
        conn_row = conn.execute(
            "SELECT last_access FROM memories WHERE id = ?", (mem_id,)
        ).fetchone()
        if conn_row and conn_row[0]:
            last = conn_row[0]
        try:
            last_dt = datetime.fromisoformat(last)
            hours = max(0.0, (now - last_dt).total_seconds() / 3600.0)
        except ValueError:
            hours = 24 * 365.0
        recency = RECENCY_DECAY ** hours
        final = (score / max_rrf) + W_RECENCY * recency + W_IMPORTANCE * row["importance"]
        r = dict(row)
        # score = cosine similarity (unchanged v1 meaning); FTS-only hits carry
        # lexical presence but no cosine → null
        r["score"] = round(sim_of[mem_id], 4) if mem_id in sim_of else None
        r["final"] = round(final, 4)
        out.append(r)
        hit_ids.append(mem_id)
        if len(out) >= req.k:
            break

    # bounded link expansion (A-MEM box propagation, user decision: default ON):
    # the top hit's links join the result, namespace-filtered, below organic hits.
    if req.expand and out and LINK_MAX > 0:
        existing = {r["id"] for r in out}
        for lid in out[0]["links"][:LINK_MAX]:
            if lid in existing:
                continue
            linked = _visible(conn, lid, req)
            if linked:
                linked["via"] = out[0]["id"]
                linked["final"] = 0.0  # rank strictly below organic hits
                linked["score"] = None
                out.append(linked)
                existing.add(lid)

    # reinforcement (MemoryBank/Mem0): recalled memories persist longer
    if hit_ids:
        ph = ",".join("?" * len(hit_ids))
        conn.execute(
            f"UPDATE memories SET access_count = access_count + 1, last_access = ?"
            f" WHERE id IN ({ph})", (now_iso(), *hit_ids),
        )
        conn.commit()
    return out


@app.post("/forget")
def forget(req: ForgetReq):
    conn = db()
    conn.execute("DELETE FROM memories WHERE id = ?", (req.id,))
    conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (req.id,))
    conn.commit()
    return {"ok": True}


@app.get("/export")
def export_all():
    """JSONL dump of every row (incl. superseded/expired) — backup/restore."""
    conn = db()
    cols = ("id, ts, agent, run, source, tags, text, hash, type, importance,"
            " access_count, last_access, superseded_by, expires_at, links")
    out = []
    for row in conn.execute(f"SELECT {cols} FROM memories ORDER BY id"):
        out.append(json.dumps(dict(zip(cols.split(", "), row))))
    return Response(content="\n".join(out), media_type="application/x-ndjson")


@app.post("/import")
def import_all(req: dict):
    """Restore from an /export dump: re-embeds every row, skips existing ids."""
    conn = db()
    ts = now_iso()
    added = 0
    for line in str(req.get("jsonl", "")).splitlines():
        if not line.strip():
            continue
        r = json.loads(line)
        if conn.execute("SELECT 1 FROM memories WHERE id = ?", (r["id"],)).fetchone():
            continue
        emb = model().encode([r["text"]])[0].tobytes()
        conn.execute(
            "INSERT INTO memories(id, ts, agent, run, source, tags, text, hash,"
            " type, importance, access_count, last_access, superseded_by,"
            " expires_at, links) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (r["id"], r["ts"], r["agent"], r.get("run"), r.get("source"),
             r.get("tags", "[]"), r["text"], r.get("hash") or md5(r["text"]),
             r.get("type", "fact"), r.get("importance", 0.5),
             r.get("access_count", 0), r.get("last_access"),
             r.get("superseded_by"), r.get("expires_at"), r.get("links", "[]")),
        )
        conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, r["id"]))
        added += 1
    conn.commit()
    return {"ok": True, "added": added}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ.get("FORSETI_MEMORY_PORT", "8752")),
                log_level="warning")
