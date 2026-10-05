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
import threading
from datetime import datetime, timezone

import sqlite_vec
from fastapi import FastAPI, HTTPException, Response
from pydantic import BaseModel, ConfigDict, Field
from sentence_transformers import SentenceTransformer
from typing import Optional

DB_PATH = os.environ.get("FORSETI_MEMORY_DB", os.path.expanduser("~/.config/forseti/memory.db"))
MODEL_NAME = os.environ.get("FORSETI_MEMORY_MODEL", "all-MiniLM-L6-v2")
DIM = 384
SCHEMA_VERSION = 4  # v4 (P12.5-C): history audit table

# S23 calibration caveat: every published constant was tuned on 1024-d
# embeddings — keep them in env config, never in code.
RRF_K = float(os.environ.get("FORSETI_MEMORY_RRF_K", "60"))
DEDUP_SIM = float(os.environ.get("FORSETI_MEMORY_DEDUP", "0.95"))  # Mem0's near-dup gate
RECENCY_DECAY = float(os.environ.get("FORSETI_MEMORY_RECENCY_DECAY", "0.995"))  # per hour
W_RECENCY = float(os.environ.get("FORSETI_MEMORY_W_RECENCY", "0.5"))
W_IMPORTANCE = float(os.environ.get("FORSETI_MEMORY_W_IMPORTANCE", "0.5"))
LINK_MAX = int(os.environ.get("FORSETI_MEMORY_LINK_MAX", "10"))  # bounded box expansion
# P12.5-B auto-links on write (A-MEM light): a new row links its top visible
# candidates in [AUTOLINK_SIM, DEDUP_SIM) — below DEDUP_SIM is the merge band,
# at-or-above never reaches here as an insert.
AUTOLINK_SIM = float(os.environ.get("FORSETI_MEMORY_AUTOLINK_SIM", "0.7"))
AUTOLINK_MAX = int(os.environ.get("FORSETI_MEMORY_AUTOLINK_MAX", "10"))
AUTOLINK_ON = os.environ.get("FORSETI_MEMORY_AUTOLINK", "1") != "0"
BOOT_ID = os.environ.get("FORSETI_MEMORY_BOOT_ID", "")  # serve-script liveness nonce (M2)

# P13 boot-time validation (agent-1 hardening): bad env constants fail FAST
# at startup instead of ZeroDivision/negative boosts on every request.
if RRF_K <= 0:
    raise SystemExit(f"FORSETI_MEMORY_RRF_K must be > 0 (got {RRF_K})")
if not 0 <= DEDUP_SIM <= 1 or not 0 <= RECENCY_DECAY <= 1:
    raise SystemExit("FORSETI_MEMORY_DEDUP / RECENCY_DECAY must be in [0,1]")
if W_RECENCY < 0 or W_IMPORTANCE < 0 or LINK_MAX < 0:
    raise SystemExit("FORSETI_MEMORY_W_RECENCY / W_IMPORTANCE / LINK_MAX must be >= 0")

VALID_TYPES = {"fact", "episode", "procedure", "preference"}

_model = None
_model_lock = threading.Lock()


def model():
    # P13-M18: thread-safe lazy init (FastAPI sync endpoints run in a
    # threadpool; two first requests used to double-load the model)
    global _model
    with _model_lock:
        if _model is None:
            _model = SentenceTransformer(MODEL_NAME)
        return _model


def now_iso():
    return datetime.now(timezone.utc).isoformat()


def fts_query(q: str) -> str:
    """FTS5-safe query: bare tokens only (MATCH syntax errors on punctuation)."""
    return " ".join(re.findall(r"\w+", q))


def db():
    conn = sqlite3.connect(DB_PATH, timeout=5.0)
    conn.enable_load_extension(True)
    sqlite_vec.load(conn)
    conn.enable_load_extension(False)
    # hardening (P13): WAL = concurrent readers with one writer; explicit busy
    # timeout for writer contention (probed clean at 12 concurrent writes, this
    # makes it robust by construction rather than by default luck)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA busy_timeout=5000")
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
        except sqlite3.OperationalError as e:
            # P13-M5: only duplicate-column failures may pass — a swallowed
            # "database is locked" latched user_version with a missing column
            if "duplicate column" not in str(e).lower():
                raise
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
    # v3 (P13 hardening): fire only when text is SET — every recall's access
    # bump UPDATE used to churn the FTS index (delete+insert per hit). The
    # near-dup merge UPDATE sets text, so it still fires where it must.
    conn.execute("DROP TRIGGER IF EXISTS mem_fts_au")
    conn.execute(
        "CREATE TRIGGER IF NOT EXISTS mem_fts_au AFTER UPDATE OF text ON memories BEGIN"
        " INSERT INTO mem_fts(mem_fts, rowid, text) VALUES ('delete', old.id, old.text);"
        " INSERT INTO mem_fts(rowid, text) VALUES (new.id, new.text); END"
    )
    conn.execute("INSERT INTO mem_fts(mem_fts) VALUES('rebuild')")
    # v4 (P12.5-C): append-only audit of every mutation. Closes the gap where
    # a near-dup merge destroyed the previous statement (and forget destroyed
    # everything). Mem0-style events: add|merge|supersede|delete|revive
    # (revive = M10 un-supersede after the superseding row was forgotten).
    conn.execute(
        "CREATE TABLE IF NOT EXISTS history ("
        " id INTEGER PRIMARY KEY AUTOINCREMENT,"
        " memory_id INTEGER NOT NULL,"
        " namespace TEXT NOT NULL,"
        " event TEXT NOT NULL,"
        " actor TEXT,"
        " old_text TEXT,"
        " new_text TEXT,"
        " ref INTEGER,"
        " at TEXT NOT NULL)"
    )
    conn.execute("CREATE INDEX IF NOT EXISTS idx_history_mem ON history(memory_id)")
    # backfill: every pre-v4 row gets one add event so /history is never empty
    # for existing facts (at = the row's own ts)
    if conn.execute("SELECT COUNT(*) FROM history").fetchone()[0] == 0:
        conn.execute(
            "INSERT INTO history(memory_id, namespace, event, actor, new_text, at)"
            " SELECT id, agent, 'add', agent, text, ts FROM memories"
        )
    # P13-M5: verify every expected column exists BEFORE latching the schema
    # version — a partially applied migration must re-run, not fail forever.
    have = {row[1] for row in conn.execute("PRAGMA table_info(memories)").fetchall()}
    for col in ("hash", "type", "importance", "access_count", "last_access",
                "superseded_by", "expires_at", "links"):
        if col not in have:
            raise RuntimeError(f"migration incomplete: column {col!r} missing after ALTERs")
    tables = {r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    if "history" not in tables:  # P12.5-C (same re-run-not-latch rule as M5)
        raise RuntimeError("migration incomplete: history table missing")
    conn.execute(f"PRAGMA user_version = {SCHEMA_VERSION}")


def md5(text: str) -> str:
    return hashlib.md5(text.encode()).hexdigest()


def _parse_ts(s):
    """Parse an ISO timestamp; naive values are assumed UTC (P13-M3: naive
    timestamps from imports used to crash recall with a TypeError). Raises
    ValueError on garbage — callers decide the fallback."""
    dt = datetime.fromisoformat(str(s))
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def _expired(expires_at) -> bool:
    """Expired = past its TTL. Unparseable/absent → not expired (bad data must
    not silently hide facts). P13-M2: string-compare of ISO timestamps with
    mixed formats was also fragile — parse, never compare strings."""
    if not expires_at:
        return False
    try:
        return _parse_ts(expires_at) < datetime.now(timezone.utc)
    except (ValueError, TypeError):
        return False


def _hist(conn, mem_id, namespace, event, actor=None, old=None, new=None, ref=None):
    """P12.5-C: append one audit row. Callers must already be inside their
    write transaction — history and mutation commit or roll back together."""
    conn.execute(
        "INSERT INTO history(memory_id, namespace, event, actor, old_text,"
        " new_text, ref, at) VALUES (?,?,?,?,?,?,?,?)",
        (mem_id, namespace, event, actor, old, new, ref, now_iso()),
    )


def _autolinks(conn, near, req_agent, exclude):
    """P12.5-B: ids the NEW row should link (A-MEM light, write time).
    Candidates from the caller's read-visibility set (own namespace + shared)
    with cosine in [AUTOLINK_SIM, DEDUP_SIM) — at-or-above DEDUP_SIM the write
    would have merged, never inserted. Shared writes therefore only ever link
    shared rows: a shared row linking a private fact would leak its existence
    through box expansion. Dead (superseded/expired) rows never link."""
    out = []
    for mem_id, distance in near:
        if len(out) >= AUTOLINK_MAX:
            break
        sim = 1.0 - float(distance)
        if sim < AUTOLINK_SIM or mem_id == exclude:
            continue
        r = conn.execute(
            "SELECT agent, superseded_by, expires_at FROM memories WHERE id = ?",
            (mem_id,),
        ).fetchone()
        if not r or (r[0] != req_agent and r[0] != "shared"):
            continue
        if r[1] is not None or _expired(r[2]):
            continue
        out.append(mem_id)
    return out


app = FastAPI(title="forseti-memory")


class WriteReq(BaseModel):
    # P13-M14: extra="forbid" — silently dropped fields were the exact
    # mechanism of the live links-lost incident; typos must 422, not vanish.
    model_config = ConfigDict(extra="forbid")
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
    model_config = ConfigDict(extra="forbid")  # P13-M14
    query: str = Field(min_length=1)
    agent: str = "shared"
    k: int = Field(default=5, ge=1)  # P13-M13: k=0 used to return one row anyway
    since: Optional[str] = None
    min_score: float = 0.3  # semantic pre-gate — below it, noise injection
    type: Optional[str] = None        # memory-type filter
    include_superseded: bool = False
    expand: bool = True  # bounded A-MEM box expansion of the top hit


class ForgetReq(BaseModel):
    model_config = ConfigDict(extra="forbid")  # P13-M14
    id: int
    agent: Optional[str] = None  # P13-M1: namespace guard on deletes


class ImportReq(BaseModel):
    model_config = ConfigDict(extra="forbid")  # P13-M14
    jsonl: str


@app.get("/health")
def health():
    conn = db()
    n = conn.execute("SELECT COUNT(*) FROM memories").fetchone()[0]
    ver = conn.execute("PRAGMA user_version").fetchone()[0]
    return {"status": "ok", "rows": n, "model": MODEL_NAME, "dim": DIM,
            "schema": ver, "boot_id": BOOT_ID}


@app.post("/write")
def write(req: WriteReq):
    if req.type not in VALID_TYPES:
        raise HTTPException(status_code=422, detail=f"type must be one of {sorted(VALID_TYPES)}")
    if not 0 <= req.importance <= 1:
        raise HTTPException(status_code=422, detail="importance must be in [0,1]")
    if req.expires_at is not None:  # P13-M20: garbage TTLs must 422, not silently never expire
        try:
            _parse_ts(req.expires_at)
        except (ValueError, TypeError):
            raise HTTPException(status_code=422, detail="expires_at must be an ISO timestamp")
    emb = model().encode([req.text])[0].tobytes()
    conn = db()
    ts = now_iso()
    digest = md5(req.text)

    # explicit supersedes (Zep semantics) wins over every heuristic: insert the
    # new fact as current and mark the OLD row superseded (history queryable).
    # P13-M5: the target must exist and still be CURRENT — superseding an
    # already-superseded row used to silently no-op (the declared relationship
    # was dropped); 422 with the chain hint instead (explicit beats silent).
    # P13-M9: the UPDATE's rowcount is checked — two concurrent supersedes of
    # the same target can't both win (the loser rolls back, 409).
    if req.supersedes is not None:
        target = conn.execute(
            "SELECT superseded_by, agent FROM memories WHERE id = ?", (req.supersedes,)
        ).fetchone()
        if target is None:
            raise HTTPException(status_code=422,
                                detail=f"supersedes target #{req.supersedes} not found")
        if target[1] != "shared" and target[1] != req.agent:
            raise HTTPException(status_code=422,
                                detail=f"#{req.supersedes} belongs to another namespace — only your own or shared facts can be superseded")
        if target[0] is not None:
            raise HTTPException(status_code=422,
                                detail=f"#{req.supersedes} is already superseded by #{target[0]} — supersede the current fact #{target[0]} instead")
        conn.execute("BEGIN IMMEDIATE")
        # P12.5-B: auto-links apply to every INSERT path, explicit supersedes
        # included (the pool predates the insert, so the new row can't link
        # itself).
        auto = []
        if AUTOLINK_ON:
            pool = conn.execute(
                "SELECT mem_id, distance FROM mem_vec WHERE embedding MATCH ? AND k = ?"
                " ORDER BY distance", (emb, 100),
            ).fetchall()
            auto = _autolinks(conn, pool, req.agent, exclude=None)
        # the superseded target is dead to recall anyway — don't link it
        auto = [i for i in auto if i != req.supersedes]
        links = sorted(set(req.links) | set(auto))
        cur = conn.execute(
            "INSERT INTO memories(ts, agent, run, source, tags, text, hash, type,"
            " importance, superseded_by, expires_at, links) VALUES"
            " (?,?,?,?,?,?,?,?,?,NULL,?,?)",
            (ts, req.agent, req.run, req.source, json.dumps(req.tags), req.text,
             digest, req.type, req.importance, req.expires_at, json.dumps(links)),
        )
        mem_id = cur.lastrowid
        conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
        marked = conn.execute(
            "UPDATE memories SET superseded_by = ? WHERE id = ? AND superseded_by IS NULL",
            (mem_id, req.supersedes),
        ).rowcount
        if marked != 1:  # P13-M9: lost the race — rollback, do not claim success
            conn.rollback()
            raise HTTPException(status_code=409,
                                detail=f"#{req.supersedes} was superseded concurrently — recall and supersede the current fact instead")
        # P12.5-C: audit both sides of the lineage edge
        _hist(conn, mem_id, req.agent, "add", actor=req.agent, new=req.text,
              ref=req.supersedes)
        _hist(conn, req.supersedes, target[1], "supersede", actor=req.agent,
              ref=mem_id)
        conn.commit()
        return {"id": mem_id, "ts": ts, "supersedes": req.supersedes,
                "auto_links": auto}

    # P13-M7: BEGIN IMMEDIATE around dedup-check + insert — two concurrent
    # identical writes used to race past the SELECT and insert permanent twins.
    conn.execute("BEGIN IMMEDIATE")

    # exact dedup: same text in the same namespace is the same memory.
    # P13-M2b: only CURRENT rows count — a superseded or expired twin is
    # invisible to dedup, so rewriting a dead fact inserts a fresh current
    # row (write-to-revive) instead of bumping a corpse's ts.
    # P13-M11: the bump refreshes metadata too (type/importance/tags) —
    # re-writing a fact with new metadata must not silently keep the old.
    row = conn.execute(
        "SELECT id, expires_at, tags, type, importance FROM memories WHERE agent = ? AND hash = ?"
        " AND superseded_by IS NULL",
        (req.agent, digest),
    ).fetchone()
    if row and not _expired(row[1]):
        old_tags = json.loads(row[2])
        merged_meta = sorted(set(old_tags) | set(req.tags))
        if (row[3] != req.type or row[4] != req.importance or merged_meta != old_tags):
            conn.execute(
                "UPDATE memories SET ts = ?, type = ?, importance = ?, tags = ? WHERE id = ?",
                (ts, req.type, req.importance, json.dumps(merged_meta), row[0]),
            )
        else:
            conn.execute("UPDATE memories SET ts = ? WHERE id = ?", (ts, row[0]))
        conn.commit()
        return {"id": row[0], "ts": ts, "dedup": "exact"}

    # near-dup merge (Mem0's 0.95 gate, inside the caller's namespace): update
    # the existing row instead of accreting near-identical facts.
    # P13-M2: only CURRENT rows are merge candidates — merging into an expired
    # row kept its expires_at and the refreshed fact stayed dead (hit live).
    # P13-M19: pool widened (global top-100) — a small global pool missed
    # in-namespace twins when other namespaces were denser.
    # P13-M12: the merge carries links too (the caller's links were dropped).
    near = conn.execute(
        "SELECT mem_id, distance FROM mem_vec WHERE embedding MATCH ? AND k = ?"
        " ORDER BY distance",
        (emb, 100),
    ).fetchall()
    for mem_id, distance in near:
        r = conn.execute(
            "SELECT agent, superseded_by, expires_at FROM memories WHERE id = ?", (mem_id,)
        ).fetchone()
        if not r or r[0] != req.agent or r[1] is not None or _expired(r[2]):
            continue
        if 1.0 - float(distance) >= DEDUP_SIM:
            old_row = conn.execute(
                "SELECT tags, text FROM memories WHERE id = ?", (mem_id,)
            ).fetchone()
            old_tags = json.loads(old_row[0])
            merged = sorted(set(old_tags) | set(req.tags))
            conn.execute(
                "UPDATE memories SET ts = ?, text = ?, hash = ?, tags = ?,"
                " type = ?, importance = ?, links = ? WHERE id = ?",
                (ts, req.text, digest, json.dumps(merged), req.type, req.importance,
                 json.dumps(req.links), mem_id),
            )
            conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (mem_id,))
            conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
            # P12.5-C: a merge destroys the previous statement — the audit row
            # is what keeps it queryable (this was the whole point of C).
            _hist(conn, mem_id, req.agent, "merge", actor=req.agent,
                  old=old_row[1], new=req.text)
            conn.commit()
            return {"id": mem_id, "ts": ts, "dedup": "near"}

    # P12.5-B: auto-link the top visible candidates from the M19 pool
    # (band [AUTOLINK_SIM, DEDUP_SIM) — closer would have merged). Exact-dup
    # bumps above get no auto-links: the row already exists with its links.
    auto = []
    if AUTOLINK_ON:
        auto = _autolinks(conn, near, req.agent, exclude=None)
    links = sorted(set(req.links) | set(auto))
    cur = conn.execute(
        "INSERT INTO memories(ts, agent, run, source, tags, text, hash, type,"
        " importance, superseded_by, expires_at, links)"
        " VALUES (?,?,?,?,?,?,?,?,?,NULL,?,?)",
        (ts, req.agent, req.run, req.source, json.dumps(req.tags), req.text,
         digest, req.type, req.importance, req.expires_at, json.dumps(links)),
    )
    mem_id = cur.lastrowid
    conn.execute("INSERT INTO mem_vec(embedding, mem_id) VALUES (?,?)", (emb, mem_id))
    _hist(conn, mem_id, req.agent, "add", actor=req.agent, new=req.text)  # P12.5-C
    conn.commit()
    return {"id": mem_id, "ts": ts, "auto_links": auto}


def _visible(conn, mem_id, req, since_dt=None):
    """Namespace + filter predicates for one candidate row (returns row or None)."""
    r = conn.execute(
        "SELECT ts, agent, run, source, tags, text, type, importance,"
        " superseded_by, expires_at, links FROM memories WHERE id = ?", (mem_id,)
    ).fetchone()
    if not r:
        return None
    ts, agent, run, source, tags, text, mtype, importance, sup, expires, links = r
    try:
        tags = json.loads(tags)
    except (ValueError, TypeError):
        tags = []  # bad data must not 500 the recall (P13-M15)
    try:
        links = json.loads(links)
        if not isinstance(links, list):
            links = []
    except (ValueError, TypeError):
        links = []
    if agent != "shared" and agent != req.agent:  # namespace isolation
        return None
    if since_dt is not None:
        try:
            if _parse_ts(ts) < since_dt:
                return None
        except (ValueError, TypeError):
            pass  # unparseable row ts → keep it visible (bad data must not hide facts)
    if req.type and mtype != req.type:
        return None
    if sup is not None and not req.include_superseded:
        return None
    if _expired(expires):
        return None
    return {"id": mem_id, "ts": ts, "agent": agent, "run": run,
            "source": source, "tags": tags, "text": text,
            "type": mtype, "importance": importance, "superseded_by": sup,
            "links": links}


@app.post("/recall")
def recall(req: RecallReq):
    if req.type is not None and req.type not in VALID_TYPES:
        raise HTTPException(status_code=422, detail=f"type must be one of {sorted(VALID_TYPES)}")
    since_dt = None
    if req.since:
        try:
            since_dt = _parse_ts(req.since)
        except (ValueError, TypeError):
            raise HTTPException(status_code=422, detail="since must be an ISO timestamp")
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
    # semantically-dead candidate. P13-M4: only FTS-ONLY ids pass the gate —
    # a candidate present in BOTH legs must clear min_score on the vector leg
    # (the old union let a 0.05-cosine lexical twin take full dual-leg credit).
    v_ids = {mem_id for mem_id, _ in v_leg}
    gate = {mem_id for mem_id, sim in v_leg if sim >= req.min_score}
    gate |= {mem_id for mem_id, _ in f_leg if mem_id not in v_ids}

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
    # P12.5-D: boost, THEN rank-and-truncate. The old loop emitted in RRF order
    # and truncated at k before ranking on the boosted score — importance and
    # recency could never reorder anything (their whole purpose), and a
    # boost-strong candidate at RRF rank > k never surfaced at all. The boost
    # term is computed for every visible candidate — cheap; the expensive
    # encode already happened.
    cands = []
    for mem_id, score in rrf.items():
        row = _visible(conn, mem_id, req, since_dt)
        if not row:
            continue
        last = row["ts"]  # never accessed → creation time
        conn_row = conn.execute(
            "SELECT last_access FROM memories WHERE id = ?", (mem_id,)
        ).fetchone()
        if conn_row and conn_row[0]:
            last = conn_row[0]
        try:
            hours = max(0.0, (now - _parse_ts(last)).total_seconds() / 3600.0)
        except (ValueError, TypeError):
            hours = 24 * 365.0  # unparseable timestamp → fully decayed, never 500
        recency = RECENCY_DECAY ** hours
        final = (score / max_rrf) + W_RECENCY * recency + W_IMPORTANCE * row["importance"]
        cands.append((final, mem_id, row))
    cands.sort(key=lambda t: (-t[0], t[1]))
    out = []
    hit_ids = []
    for final, mem_id, row in cands[:req.k]:
        r = dict(row)
        # score = cosine similarity (unchanged v1 meaning); FTS-only hits carry
        # lexical presence but no cosine → null
        r["score"] = round(sim_of[mem_id], 4) if mem_id in sim_of else None
        r["final"] = round(final, 4)
        out.append(r)
        hit_ids.append(mem_id)

    # bounded link expansion (A-MEM box propagation, user decision: default ON):
    # the top hit's links join the result, namespace-filtered, below organic hits.
    # P13-M15: links from a hand-edited import can be any JSON — guard the shape
    # (a dict/string used to TypeError the whole recall).
    if req.expand and out and LINK_MAX > 0:
        top_links = out[0].get("links")
        if isinstance(top_links, list):
            existing = {r["id"] for r in out}
            for lid in top_links[:LINK_MAX]:
                if not isinstance(lid, int) or lid in existing:
                    continue
                linked = _visible(conn, lid, req, since_dt)
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
    """Hard-delete one row. P13-M1: namespace-guarded — a caller may forget
    only its own rows or shared rows (another agent's private row 404s, same
    isolation as recall). P13-M10: forgetting a superseding row un-supersedes
    its predecessor instead of orphaning the lineage."""
    conn = db()
    owner = conn.execute("SELECT agent, text FROM memories WHERE id = ?", (req.id,)).fetchone()
    if owner is None:
        raise HTTPException(status_code=404, detail=f"#{req.id} not found")
    caller = req.agent or "shared"
    if owner[0] != "shared" and owner[0] != caller:
        raise HTTPException(status_code=404, detail=f"#{req.id} not found")  # not yours → invisible
    children = conn.execute(
        "SELECT id, agent FROM memories WHERE superseded_by = ?", (req.id,)
    ).fetchall()
    conn.execute("DELETE FROM memories WHERE id = ?", (req.id,))
    conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (req.id,))
    conn.execute("UPDATE memories SET superseded_by = NULL WHERE superseded_by = ?", (req.id,))
    # P12.5-C: audit the destruction (the text dies with the row — the audit
    # row is the only survivor) and the M10 un-supersedes (revive events).
    _hist(conn, req.id, owner[0], "delete", actor=caller, old=owner[1])
    for cid, cns in children:
        _hist(conn, cid, cns, "revive", actor=caller, ref=req.id)
    conn.commit()
    return {"ok": True}


class ClearReq(BaseModel):
    model_config = ConfigDict(extra="forbid")
    agent: str = Field(min_length=1, max_length=32)
    confirm: bool = False


@app.post("/clear")
def clear(req: ClearReq):
    """P12.5-A: namespace cleanup (eval gates, agent resets). `shared` is
    everyone's view → confirm:true required. Rows hard-delete like /forget
    (audited per row); rows whose superseded_by pointed at a cleared row are
    REVIVED — a clear must never strand private facts behind a deleted
    superseder (same rule as M10, batched)."""
    if req.agent == "shared" and not req.confirm:
        raise HTTPException(status_code=422,
                            detail="clearing the SHARED namespace affects every agent — resend with confirm: true")
    conn = db()
    conn.execute("BEGIN IMMEDIATE")
    rows = conn.execute("SELECT id, text FROM memories WHERE agent = ?", (req.agent,)).fetchall()
    ids = [r[0] for r in rows]
    for mem_id, text in rows:
        conn.execute("DELETE FROM memories WHERE id = ?", (mem_id,))
        conn.execute("DELETE FROM mem_vec WHERE mem_id = ?", (mem_id,))
        _hist(conn, mem_id, req.agent, "delete", actor=req.agent, old=text)
    if ids:
        ph = ",".join("?" * len(ids))
        revived = conn.execute(
            f"SELECT id, agent FROM memories WHERE superseded_by IN ({ph})", ids
        ).fetchall()
        conn.execute(
            f"UPDATE memories SET superseded_by = NULL WHERE superseded_by IN ({ph})", ids
        )
        for cid, cns in revived:
            _hist(conn, cid, cns, "revive", actor=req.agent)
    conn.commit()
    return {"ok": True, "cleared": len(ids)}


@app.get("/namespaces")
def namespaces():
    """Namespace overview for ops (which agents/probes hold rows)."""
    conn = db()
    return {"namespaces": [
        {"agent": a, "rows": n} for a, n in conn.execute(
            "SELECT agent, COUNT(*) FROM memories GROUP BY agent ORDER BY COUNT(*) DESC"
        ).fetchall()
    ]}


@app.get("/history")
def history_of(memory_id: int, agent: str = "shared"):
    """P12.5-C: audit trail of one memory, chronological. Namespace-guarded
    like recall (another agent's private row → 404). The memory itself may be
    long deleted — that is the point."""
    conn = db()
    ns = conn.execute(
        "SELECT namespace FROM history WHERE memory_id = ? ORDER BY id DESC LIMIT 1",
        (memory_id,)).fetchone()
    if ns is None:
        raise HTTPException(status_code=404, detail=f"no history for #{memory_id}")
    if ns[0] != "shared" and ns[0] != agent:
        raise HTTPException(status_code=404, detail=f"no history for #{memory_id}")  # invisible
    out = []
    for hid, event, actor, old, new, ref, at in conn.execute(
        "SELECT id, event, actor, old_text, new_text, ref, at FROM history"
        " WHERE memory_id = ? ORDER BY id", (memory_id,)
    ).fetchall():
        out.append({"id": hid, "event": event, "actor": actor, "old_text": old,
                    "new_text": new, "ref": ref, "at": at})
    return out


@app.get("/export")
def export_all():
    """JSONL dump of every row (incl. superseded/expired) — backup/restore.
    First line is a _meta header (embedding model + dim) so a restore can
    refuse mismatched vector spaces (P13-M16)."""
    conn = db()
    cols = ("id, ts, agent, run, source, tags, text, hash, type, importance,"
            " access_count, last_access, superseded_by, expires_at, links")
    out = [json.dumps({"_meta": {"model": MODEL_NAME, "dim": DIM}})]
    for row in conn.execute(f"SELECT {cols} FROM memories ORDER BY id"):
        out.append(json.dumps(dict(zip(cols.split(", "), row))))
    return Response(content="\n".join(out), media_type="application/x-ndjson")


@app.post("/import")
def import_all(req: ImportReq):
    """Restore from an /export dump: re-embeds every row, skips existing ids.
    P13-M1: validate + parse EVERY line first, then insert in ONE transaction —
    a malformed row used to 500 mid-import (no rollback, partial state).
    P13-M6: all texts are batch-embedded BEFORE the transaction (one writer
    under WAL — the lock is never held across model.encode time).
    P13-M16: a _meta header records the dump's embedding model; a mismatched
    restore 422s instead of silently mixing vector spaces.
    P13-M8: passthrough fields are shape-checked in the same 422 pass."""
    rows = []
    for i, line in enumerate(req.jsonl.splitlines(), 1):
        if not line.strip():
            continue
        try:
            r = json.loads(line)
            if "_meta" in r:  # export header (P13-M16)
                m = r["_meta"]
                if isinstance(m, dict) and m.get("model") and m["model"] != MODEL_NAME:
                    raise ValueError(
                        f"dump was embedded with {m['model']!r}, service runs {MODEL_NAME!r} — "
                        f"set FORSETI_MEMORY_MODEL={m['model']} to restore it faithfully")
                continue
            for k in ("id", "ts", "agent", "text"):
                if k not in r:
                    raise ValueError(f"missing {k}")
            if type(r["id"]) is not int or not isinstance(r["text"], str):
                raise ValueError("id must be int (not bool), text must be str")
            for k, kinds in (("tags", (list, str)), ("links", (list, str)),
                             ("run", (str, type(None))), ("source", (str, type(None))),
                             ("expires_at", (str, type(None))),
                             ("last_access", (str, type(None))),
                             ("type", (str,)), ("hash", (str, type(None)))):
                v = r.get(k)
                if v is not None and not isinstance(v, kinds):
                    raise ValueError(f"{k} must be {' or '.join(k.__name__ for k in kinds)}, got {type(v).__name__}")
            if isinstance(r.get("links"), list) and not all(type(x) is int for x in r["links"]):
                raise ValueError("links must be a list of ints")
            if isinstance(r.get("tags"), list) and not all(isinstance(x, str) for x in r["tags"]):
                raise ValueError("tags must be a list of strings")
            for k in ("importance",):
                if not isinstance(r.get(k, 0.5), (int, float)):
                    raise ValueError(f"{k} must be a number")
            if not isinstance(r.get("superseded_by", None), (int, type(None))):
                raise ValueError("superseded_by must be int or null")
            rows.append(r)
        except (ValueError, TypeError) as e:
            raise HTTPException(status_code=422,
                                detail=f"line {i}: {e} — nothing imported (all-or-nothing)")
    # batch-embed outside the write transaction (P13-M6)
    embs = {r["id"]: model().encode([r["text"]])[0].tobytes() for r in rows}
    conn = db()
    added = 0
    try:
        conn.execute("BEGIN")
        for r in rows:
            if conn.execute("SELECT 1 FROM memories WHERE id = ?", (r["id"],)).fetchone():
                continue
            emb = embs[r["id"]]  # pre-embedded (P13-M6: no encode under the lock)
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
    except Exception:
        conn.rollback()
        raise
    return {"ok": True, "added": added}


if __name__ == "__main__":
    import uvicorn
    # P13-M17: fail FAST at boot on a dimension mismatch (FORSETI_MEMORY_MODEL
    # with another dim used to leave /health green while every write 500s).
    # P13-M18: warm the model before serving (thread-safe lazy init stays).
    m = model()
    dim = m.get_sentence_embedding_dimension()
    if dim != DIM:
        raise SystemExit(f"{MODEL_NAME} embeds {dim}-d, schema expects {DIM}-d "
                         f"(update DIM or pick a {DIM}-d model)")
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ.get("FORSETI_MEMORY_PORT", "8752")),
                log_level="warning")
