"""memtree — MemTree-style hierarchical organization for forseti-memory (P14).

Paper: arXiv:2410.14052 (Rezazadeh et al., "From Isolated Conversations to
Hierarchical Schemas: Dynamic Tree Memory Representation for LLMs"). Verified
mechanics (docs/spikes.md S26): insert = traverse from the root, descend into
the best child while cosine(new, child) >= theta(d) = theta0 * exp(lambda * d
/ max_depth); reaching a leaf expands it into a parent holding {old, new};
internal-node content aggregates its children and is re-embedded. Retrieval =
COLLAPSED tree: flat cosine over all nodes (paper ablation: collapsed >=
traversal retrieval). Learned trees show branching factor ~2.1 — n-ary fanout,
no child cap (paper-faithful).

Adaptations to forseti (recorded in spikes S26):
- Memory rows ARE the leaves: memories.parent_id -> tree_nodes.id, NULL = the
  implicit per-namespace root. tree_nodes holds internal summary nodes only.
- ONE TREE PER NAMESPACE. Internal nodes aggregate descendant text, so a
  global tree would leak a private fact's wording into a shared-visible
  summary. Namespace == the memories.agent value ('shared', pi agent, or the
  crew namespace 'crew-<name>').
- Expansion runs at most ONCE per insert. The paper's unbounded re-expansion
  (descend into the just-created leaf child when sim still clears the deeper
  theta) builds single-content chains; one expansion attaches {old,new}.
- Aggregation RECOMPUTES from the current living children (incremental
  Aggregate(cv, cnew) cannot be undone by merge/forget/collapse). The LLM
  hook folds children pairwise with the paper's merge prompt; any failure
  falls back to the heuristic. Writes never fail because of aggregation.
- Internal embeddings bootstrap as the mean of the children's embeddings at
  placement time, then refresh replaces them with the summary-text embedding.
- Lock discipline (M6): structure mutations are pure SQL and run INSIDE the
  caller's write transaction; text aggregation + encode run AFTER commit
  (advisory — a crash there leaves correct structure with stale summaries).
"""
import math

import numpy as np

# Configured by memory_serve at import time (dependency injection so this
# module never imports the service).
encode_fn = None      # texts -> np.ndarray [n, DIM] float32
agg_llm_fn = None     # (texts list) -> merged summary or None (abstain/down)
expired_fn = None     # expires_at value -> bool
now_fn = None         # -> ISO timestamp string

AGG_CHILD_CHARS = 120   # heuristic: per-child head in a merged summary
AGG_MAX_CHARS = 600     # heuristic: hard cap of a node summary
DESC_SCAN_MAX = 200     # per-node BFS budget for descendant leaf scans
TOTAL_SCAN_MAX = 4000   # whole-walk budget (treats a corrupted cycle as done)

KIND_LEAF = "leaf"
KIND_NODE = "node"


def _vec(row):
    if not row or row[0] is None:
        return None
    return np.frombuffer(row[0], dtype="<f4")


def _cosine(a, b):
    if a is None or b is None:
        return -1.0
    na, nb = float(np.linalg.norm(a)), float(np.linalg.norm(b))
    if na == 0 or nb == 0:
        return -1.0
    return float(np.dot(a, b) / (na * nb))


def _mean_bytes(vecs):
    """Mean embedding serialized as little-endian float32 (sqlite_vec format)."""
    return np.mean(np.stack(vecs), axis=0).astype("<f4").tobytes()


def children(conn, ns, parent_id):
    """Living children of one tree position (parent_id None = namespace root).

    Leaves = CURRENT memory rows only: superseded/expired rows keep their
    placement but are invisible to traversal, aggregation, and descendant
    scans. An internal node with no living descendant is not a child either.
    """
    out = []
    ph = "IS NULL" if parent_id is None else "= ?"
    args = (ns,) if parent_id is None else (ns, parent_id)
    for (nid,) in conn.execute(
        f"SELECT id FROM tree_nodes WHERE namespace=? AND parent_id {ph} ORDER BY id", args
    ).fetchall():
        if count_descendants(conn, ns, nid) <= 0:
            continue
        emb = _vec(conn.execute("SELECT embedding FROM tree_vec WHERE node_id=?", (nid,)).fetchone())
        out.append({"kind": KIND_NODE, "id": nid, "emb": emb})
    for mid, text, expires in conn.execute(
        f"SELECT id, text, expires_at FROM memories WHERE agent=? AND parent_id {ph}"
        " AND superseded_by IS NULL ORDER BY id", args
    ).fetchall():
        if expired_fn and expired_fn(expires):
            continue
        emb = _vec(conn.execute("SELECT embedding FROM mem_vec WHERE mem_id=?", (mid,)).fetchone())
        out.append({"kind": KIND_LEAF, "id": mid, "emb": emb, "text": text})
    return out


def descendants_leaves(conn, ns, nid):
    """Living leaf ids under an internal node (bounded BFS)."""
    ids, stack, seen = [], [nid], {nid}
    scanned = 0
    while stack and len(ids) < DESC_SCAN_MAX and scanned < TOTAL_SCAN_MAX:
        cur = stack.pop()
        for k in children(conn, ns, cur):
            scanned += 1
            if k["kind"] == KIND_LEAF:
                ids.append(k["id"])
                if len(ids) >= DESC_SCAN_MAX:
                    break
            elif k["id"] not in seen:
                seen.add(k["id"])
                stack.append(k["id"])
    return ids


def count_descendants(conn, ns, nid):
    return len(descendants_leaves(conn, ns, nid))


def ancestors(conn, node_id):
    """Internal-node ids from node_id (exclusive) up toward the root."""
    out, cur, seen = [], node_id, set()
    while cur is not None and cur not in seen:
        seen.add(cur)
        row = conn.execute(
            "SELECT parent_id FROM tree_nodes WHERE id=?", (cur,)
        ).fetchone()
        if row is None:
            break
        cur = row[0]
        if cur is not None:
            out.append(cur)
    return out


def place(conn, ns, mem_id, theta0, lam, max_depth):
    """Attach a freshly inserted row into its namespace tree (Algorithm 1,
    with the one-expansion guard). PURE SQL — call inside the write txn.
    Returns internal-node ids to refresh, deepest-first (possibly []).
    """
    emb = _vec(conn.execute("SELECT embedding FROM mem_vec WHERE mem_id=?", (mem_id,)).fetchone())
    if emb is None:
        return []  # vector leg missing (import edge) — stay unplaced, retry on import replay
    parent, chain = None, []
    for d in range(max_depth + 1):
        kids = [k for k in children(conn, ns, parent) if k["id"] != mem_id]
        best, bestsim = None, -1.0
        for k in kids:
            s = _cosine(emb, k["emb"])
            if s > bestsim:
                best, bestsim = k, s
        theta = theta0 * math.exp(lam * d / max_depth)
        if best is not None and bestsim >= theta:
            if best["kind"] == KIND_NODE:
                parent = best["id"]
                chain.append(best["id"])
                continue
            # best is a leaf -> expand: new internal node takes this position,
            # old leaf + new row become its two children.
            cur = conn.execute(
                "INSERT INTO tree_nodes(namespace, parent_id, text, descendants, updated_at)"
                " VALUES (?,?,?,?,?)",
                (ns, parent, "", 2, now_fn()),
            )
            nid = cur.lastrowid
            if best["emb"] is not None:
                conn.execute("INSERT INTO tree_vec(embedding, node_id) VALUES (?,?)",
                             (_mean_bytes([emb, best["emb"]]), nid))
            conn.execute("UPDATE memories SET parent_id=? WHERE id=?", (nid, best["id"]))
            conn.execute("UPDATE memories SET parent_id=? WHERE id=?", (nid, mem_id))
            chain.append(nid)
            return chain
        break  # no child clears theta(d): attach under the current node
    conn.execute("UPDATE memories SET parent_id=? WHERE id=?", (parent, mem_id))
    return chain


def node_depth(conn, nid):
    d, cur, seen = 0, nid, {nid}
    while True:
        row = conn.execute("SELECT parent_id FROM tree_nodes WHERE id=?", (cur,)).fetchone()
        if row is None or row[0] is None:
            return d
        cur = row[0]
        d += 1
        if cur in seen or d > 64:  # corruption guard
            return d
        seen.add(cur)


def _aggregate(texts):
    """Fold child texts into one summary: LLM hook first (pairwise fold, the
    paper's merge prompt), heuristic heads on abstention."""
    if not texts:
        return ""
    if len(texts) == 1:
        return texts[0][:AGG_MAX_CHARS]
    if agg_llm_fn:
        acc = texts[0]
        out = None
        for i, t in enumerate(texts[1:], start=2):
            merged = agg_llm_fn([acc, t], i)
            if merged:
                acc = merged
                out = acc
            else:
                out = None  # one bad fold invalidates the LLM result — heuristic
                break
        if out:
            return out[:AGG_MAX_CHARS]
    heads = [t if len(t) <= AGG_CHILD_CHARS else t[:AGG_CHILD_CHARS].rstrip() + "…" for t in texts[:16]]
    s = " / ".join(heads)
    if len(texts) > 16:
        s += f" / (+{len(texts) - 16} more)"
    return s[:AGG_MAX_CHARS]


def refresh(conn, ns, ids):
    """Recompute summary text + embedding for internal nodes, deepest-first.
    Runs AFTER the write txn commits (encode/network stay off the lock).
    """
    uniq = sorted(set(i for i in ids if i is not None), key=lambda i: -node_depth(conn, i))
    for nid in uniq:
        row = conn.execute("SELECT id FROM tree_nodes WHERE id=?", (nid,)).fetchone()
        if row is None:
            continue  # collapsed away between txn and refresh — nothing to do
        kids = children(conn, ns, nid)
        texts = []
        for k in kids:
            if k["kind"] == KIND_LEAF:
                texts.append(k["text"])
            else:
                t = conn.execute("SELECT text FROM tree_nodes WHERE id=?", (k["id"],)).fetchone()
                if t and t[0]:
                    texts.append(t[0])
        summary = _aggregate(texts)
        n = count_descendants(conn, ns, nid)
        conn.execute("UPDATE tree_nodes SET text=?, descendants=?, updated_at=? WHERE id=?",
                     (summary, n, now_fn(), nid))
        if summary and encode_fn:
            try:
                v = encode_fn([summary])[0].astype("<f4").tobytes()
                conn.execute("DELETE FROM tree_vec WHERE node_id=?", (nid,))
                conn.execute("INSERT INTO tree_vec(embedding, node_id) VALUES (?,?)", (v, nid))
            except Exception:
                pass  # summary lives, embedding keeps its mean-bootstrap value


def collapse_up(conn, ns, nid):
    """After a leaf left the tree: internal nodes with <2 living children are
    pruned (one child is promoted into the pruned node's place). Returns the
    surviving ancestors that should be refreshed (innermost first, [] at root
    or when everything up to the root dissolved).
    """
    touched = []
    cur = nid
    seen = set()
    while cur is not None and cur not in seen:
        seen.add(cur)
        row = conn.execute("SELECT parent_id FROM tree_nodes WHERE id=?", (cur,)).fetchone()
        if row is None:
            break
        parent = row[0]
        kids = children(conn, ns, cur)
        if len(kids) >= 2:
            touched = [cur] + ancestors(conn, cur)  # cur lost a descendant — stale too
            return touched
        # prune cur; promote the single remaining child (if any) into its place
        if len(kids) == 1:
            k = kids[0]
            if k["kind"] == KIND_LEAF:
                conn.execute("UPDATE memories SET parent_id=? WHERE id=?", (parent, k["id"]))
            else:
                conn.execute("UPDATE tree_nodes SET parent_id=? WHERE id=?", (parent, k["id"]))
        conn.execute("DELETE FROM tree_vec WHERE node_id=?", (cur,))
        conn.execute("DELETE FROM tree_nodes WHERE id=?", (cur,))
        cur = parent
    return touched


def tree_dump(conn, ns):
    """Nested structure view for /tree (id -N marks internal nodes)."""
    def walk(parent):
        out = []
        for k in children(conn, ns, parent):
            if k["kind"] == KIND_LEAF:
                out.append({"id": k["id"], "kind": "leaf"})
            else:
                t = conn.execute("SELECT text FROM tree_nodes WHERE id=?", (k["id"],)).fetchone()
                out.append({
                    "id": -k["id"], "kind": "node",
                    "text": ((t[0] if t and t[0] else "")[:200] or None),
                    "descendants": count_descendants(conn, ns, k["id"]),
                    "children": walk(k["id"]),
                })
        return out
    return walk(None)


def build_backfill(conn, theta0, lam, max_depth):
    """Schema-v5 backfill: replay every existing row (per namespace, ts order)
    through placement, then refresh all internal nodes bottom-up. Reuses the
    embeddings already in mem_vec — never re-encodes memory texts. Returns
    {namespace: rows placed}."""
    stats = {}
    namespaces = [r[0] for r in conn.execute(
        "SELECT DISTINCT agent FROM memories ORDER BY agent").fetchall()]
    for ns in namespaces:
        ids = [r[0] for r in conn.execute(
            "SELECT id FROM memories WHERE agent=? ORDER BY ts, id", (ns,)).fetchall()]
        touched = []
        for mid in ids:
            touched.extend(place(conn, ns, mid, theta0, lam, max_depth))
        stats[ns] = len(ids)
        refresh(conn, ns, _all_nodes(conn, ns))
    return stats


def _all_nodes(conn, ns):
    return [r[0] for r in conn.execute(
        "SELECT id FROM tree_nodes WHERE namespace=?", (ns,)).fetchall()]


def tree_stats(conn):
    stats = {}
    for ns, in conn.execute(
        "SELECT DISTINCT namespace FROM tree_nodes ORDER BY namespace").fetchall():
        n = conn.execute("SELECT COUNT(*) FROM tree_nodes WHERE namespace=?", (ns,)).fetchone()[0]
        deepest, stack, seen = 0, [(nid, 1) for nid in _all_nodes(conn, ns)], set()
        while stack:
            nid, d = stack.pop()
            deepest = max(deepest, d)
            for k in children(conn, ns, nid):
                if k["kind"] == KIND_NODE and k["id"] not in seen:
                    seen.add(k["id"])
                    stack.append((k["id"], d + 1))
        stats[ns] = {"internal": n, "max_depth": deepest}
    return stats
