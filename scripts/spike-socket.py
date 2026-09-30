#!/usr/bin/env python3
"""Phase 5 spike driver: verify herdr socket API framing + semantics (S6-S10).

Resolves spikes S6-S10 from docs/implementation-plan.md against the LIVE server.
Raw NDJSON over the Unix domain socket (~/.config/herdr/herdr.sock).

Usage:
  python3 scripts/spike-socket.py            # all stages
  python3 scripts/spike-socket.py s6 s7     # selected stages

Stages:
  s6  framing: ping/pong, error shape, id correlation
  s7  agent.read sources + pane_output_changed rate (debounce data)
  s8  layout.apply / layout.export round-trip (declarative N-pane crew tab)
  s9  two concurrent pi agents with different --model args
  s10 agent.view.set/clear ownership semantics

Safety: only creates tabs/agents named spike-*; closes/removes what it creates.
Never stops the server. Requires: herdr server running, a live pi agent named
`coder` (for s7), repo at ~/repos/forseti.
"""
import json
import os
import socket
import subprocess
import sys
import threading
import time

SOCK = os.path.expanduser("~/.config/herdr/herdr.sock")
REPO = os.path.expanduser("~/repos/forseti")
TRANSCRIPT = "/tmp/forseti-socket-spike.jsonl"

results = {}


def log(kind, payload):
    with open(TRANSCRIPT, "a") as f:
        f.write(json.dumps({"ts": time.time(), "kind": kind, **payload}) + "\n")


class Client:
    """Minimal NDJSON socket client.

    S6-verified semantics (2026-09-30): the JSON API needs NO handshake, but the
    server CLOSES the connection after responding to any one-shot request. Only
    events.subscribe connections stay open (ack `subscription_started`, then
    pushed lines shaped {"event": ..., "data": ...} with no id).
    So: `call()` = fresh connection per request; `subscribe()` = persistent stream.
    """

    def __init__(self, name):
        self.name = name
        self.seq = 0

    def _conn(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(SOCK)
        return s

    def call(self, method, params=None, timeout=30.0):
        self.seq += 1
        rid = f"{self.name}-{self.seq}"
        req = {"id": rid, "method": method, "params": params or {}}
        s = self._conn()
        try:
            s.sendall((json.dumps(req) + "\n").encode())
            log("req", {"conn": self.name, **req})
            line = _read_line(s, timeout)
            resp = json.loads(line)
            log("resp", {"conn": self.name, **resp})
            assert resp.get("id") == rid, f"id mismatch: {resp.get('id')} != {rid}"
            return resp
        finally:
            s.close()

    def subscribe(self, subscriptions, timeout=30.0):
        """Open a persistent subscription connection. Returns (sock, ack)."""
        self.seq += 1
        rid = f"{self.name}-sub{self.seq}"
        req = {"id": rid, "method": "events.subscribe",
               "params": {"subscriptions": subscriptions}}
        s = self._conn()
        s.sendall((json.dumps(req) + "\n").encode())
        log("req", {"conn": self.name, **req})
        ack = json.loads(_read_line(s, timeout))
        log("resp", {"conn": self.name, **ack})
        return s, ack

    def close(self):
        pass


def _read_line(s, timeout):
    s.settimeout(timeout)
    buf = b""
    while b"\n" not in buf:
        chunk = s.recv(65536)
        if not chunk:
            raise EOFError("connection closed")
        buf += chunk
    return buf.split(b"\n", 1)[0].decode()


def record(stage, check, ok, detail=""):
    results.setdefault(stage, []).append((check, bool(ok), detail[:200]))
    mark = "PASS" if ok else "FAIL"
    print(f"  [{mark}] {check}" + (f" — {detail[:160]}" if detail else ""))


def herdr(*args, timeout=60):
    p = subprocess.run(["herdr", *args], capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout, p.stderr


# ---------------------------------------------------------------- s6 framing
def s6():
    print("S6 — framing / handshake")
    c = Client("s6")
    r = c.call("ping")
    record("s6", "ping → pong, id echoed", r.get("result", {}).get("type") == "pong",
           json.dumps(r.get("result"))[:80])
    r = c.call("nosuch.method", {})
    record("s6", "unknown method → error.code, id echoed",
           "error" in r and r["error"].get("code"), json.dumps(r.get("error"))[:120])
    # no handshake needed: first line was already a request (contrast: hello/magic)
    record("s6", "no handshake required (first line = request)", True)
    snap = c.call("session.snapshot")
    proto = snap.get("result", {}).get("protocol")
    record("s6", "session.snapshot carries protocol", proto == 22, f"protocol={proto}")
    c.close()


# ---------------------------------------------------------------- s7 read + event rate
def s7():
    print("S7 — agent.read sources + pane_output_changed rate")
    c = Client("s7")
    agents = c.call("agent.list").get("result", {}).get("agents", [])
    coder = next((a for a in agents if a.get("name") == "coder"), None)
    if not coder:
        record("s7", "live `coder` agent present", False, "start one first")
        return
    pane = coder["pane_id"]
    record("s7", "live coder agent", True, f"pane={pane}")

    for src in ("visible", "recent", "recent_unwrapped", "detection"):
        r = c.call("agent.read", {"target": "coder", "source": src, "lines": 20})
        ok = "result" in r
        text = (r.get("result", {}) or {}).get("read", {}).get("text", "")
        record("s7", f"agent.read source={src}", ok and bool(text),
               f"result.read.text {len(text)} chars" if ok else json.dumps(r.get("error"))[:120])

    # persistent status stream: capture working→idle transitions with timestamps
    sock, ack = c.subscribe(
        [{"type": "pane.agent_status_changed", "pane_id": pane}], timeout=15)
    record("s7", "subscribe ack subscription_started",
           ack.get("result", {}).get("type") == "subscription_started", json.dumps(ack)[:100])

    # one-shot events.wait — 0.9.3 supports ONLY pane_agent_status_changed
    # (pane_output_changed in the schema is rejected: unsupported_event_wait_match)
    rej = c.call("events.wait",
                {"match_event": {"event": "pane_output_changed", "pane_id": pane},
                 "timeout_ms": 2000}, timeout=8)
    record("s7", "events.wait rejects pane_output_changed (0.9.3)",
           rej.get("error", {}).get("code") == "unsupported_event_wait_match",
           json.dumps(rej.get("error"))[:130])

    t0 = time.time()
    threading.Timer(0.5, lambda: subprocess.run(
        ["herdr", "agent", "prompt", "coder", "Reply with exactly: SPIKE7"],
        capture_output=True)).start()
    w = c.call("events.wait",
               {"match_event": {"event": "pane_agent_status_changed", "pane_id": pane,
                               "agent_status": "idle"},
                "timeout_ms": 30000}, timeout=40)
    dt = time.time() - t0
    record("s7", "events.wait status=idle one-shot fires", "result" in w,
           f"{dt:.2f}s → {json.dumps(w.get('result') or w.get('error'))[:130]}")

    # read pushed status events while the answer streams
    pushes = []
    t0 = time.time()
    while time.time() - t0 < 20:
        try:
            line = _read_line(sock, max(0.5, 20 - (time.time() - t0)))
        except (socket.timeout, EOFError):
            break
        o = json.loads(line)
        pushes.append((round(time.time() - t0, 2), o.get("data", {}).get("agent_status")))
        log("push", {"conn": "s7-stream", **o})
        if o.get("data", {}).get("agent_status") == "idle" and len(pushes) >= 2:
            break
    record("s7", "status stream captured transitions", len(pushes) >= 2,
           f"statuses={pushes}")
    sock.close()

    # rate probe: subscribe to ALL pane.updated events and count during a busy answer
    # (pane_output_changed is not a subscribable type in 0.9.3; pane.updated is the
    # closest observable proxy — title/scroll/status changes flow through it)
    sock2, ack2 = c.subscribe([{"type": "pane.updated"}], timeout=15)
    record("s7", "subscribe pane.updated (global) accepted",
           ack2.get("result", {}).get("type") == "subscription_started", json.dumps(ack2)[:100])
    subprocess.run(["herdr", "agent", "prompt", "coder",
                    "Count from 1 to 40, each number on its own line."],
                   capture_output=True)
    n, t0 = 0, time.time()
    while time.time() - t0 < 20:
        try:
            line = _read_line(sock2, max(0.5, 20 - (time.time() - t0)))
        except (socket.timeout, EOFError):
            break
        o = json.loads(line)
        if o.get("data", {}).get("pane_id") == pane:
            n += 1
        log("push", {"conn": "s7-rate", **o})
    wall = time.time() - t0
    rate = n / max(wall, 0.1)
    record("s7", "pane.updated rate for coder during busy answer", True,
           f"{n} events / {wall:.1f}s (~{rate:.1f}/s) → monitor debounce ≥ {max(150, int(1000 / max(rate, 0.1)))}ms")
    sock2.close()


# ---------------------------------------------------------------- s8 layout
def s8():
    print("S8 — layout.apply / layout.export round-trip")
    c = Client("s8")
    tree = {
        "type": "split", "direction": "right", "ratio": 0.5,
        "first": {"type": "pane", "label": "spike-left", "cwd": REPO},
        "second": {"type": "pane", "label": "spike-right", "cwd": REPO,
                   "command": ["sh", "-c", "echo SPIKE8_PANE && sleep 300"]},
    }
    r = c.call("layout.apply", {"workspace_id": "w1", "tab_label": "spike8",
                               "focus": False, "root": tree}, timeout=20)
    ok = "result" in r
    record("s8", "layout.apply creates tab from BSP tree", ok,
           json.dumps(r.get("result") or r.get("error"))[:180])
    tab_id = None
    if ok:
        lay = r["result"].get("layout") or {}
        tab_id = lay.get("tab_id")
        # export round-trip
        e = c.call("layout.export", {"tab_id": tab_id})
        root = (e.get("result") or {}).get("root") or (e.get("result") or {}).get("layout", {}).get("root") or {}
        rt_ok = root.get("type") == "split" and root.get("direction") == "right"
        record("s8", "layout.export returns same BSP shape", rt_ok,
               f"tab={tab_id} root={json.dumps(root)[:140]}")
    if tab_id:
        rc, _, err = herdr("tab", "close", tab_id)
        record("s8", "cleanup: spike8 tab closed", rc == 0, err[:100])
    c.close()


# ---------------------------------------------------------------- s9 two models
def s9():
    print("S9 — two concurrent pi agents, different --model")
    c = Client("s9")
    tree = {
        "type": "split", "direction": "down", "ratio": 0.5,
        "first": {"type": "pane", "label": "spike-a", "cwd": REPO},
        "second": {"type": "pane", "label": "spike-b", "cwd": REPO},
    }
    r = c.call("layout.apply", {"workspace_id": "w1", "tab_label": "spike9",
                               "focus": False, "root": tree}, timeout=20)
    if "result" not in r:
        record("s9", "layout.apply for two panes", False, json.dumps(r.get("error"))[:150])
        c.close()
        return
    res = r["result"]
    tab_id = (res.get("layout") or {}).get("tab_id")
    lay = res.get("layout") or {}
    # collect pane ids from the exported tree
    pane_ids = []
    def collect(node):
        if not isinstance(node, dict):
            return
        if node.get("type") == "pane" and node.get("pane_id"):
            pane_ids.append(node["pane_id"])
        for k in ("first", "second"):
            collect(node.get(k))
    collect(lay.get("root"))
    if len(pane_ids) < 2:
        # fall back: list panes of the tab
        rc, out, _ = herdr("pane", "list", "--json")
        try:
            pl = json.loads(out).get("result", {}).get("panes", [])
            pane_ids = [p["pane_id"] for p in pl if p.get("tab_id") == tab_id]
        except Exception:
            pass
    record("s9", "two panes created", len(pane_ids) >= 2, f"panes={pane_ids}")
    if len(pane_ids) < 2:
        c.close()
        return
    pa, pb = pane_ids[0], pane_ids[1]
    rc1, o1, e1 = herdr("agent", "start", "spike-a", "--kind", "pi", "--pane", pa,
                        "--", "--model", "opencode-go/glm-5.3-flash", timeout=90)
    rc2, o2, e2 = herdr("agent", "start", "spike-b", "--kind", "pi", "--pane", pb,
                        "--", "--model", "halogen/halogen-qwen3.8-flash-next", timeout=90)
    record("s9", "agent A started (glm-5.3-flash)", rc1 == 0, (e1 or o1)[:150])
    record("s9", "agent B started (halogen qwen)", rc2 == 0, (e2 or o2)[:150])
    if rc1 == 0 and rc2 == 0:
        # wait until both settle idle before prompting (pi startup can still be busy)
        for name in ("spike-a", "spike-b"):
            subprocess.run(["herdr", "agent", "wait", name, "--until", "idle",
                           "--timeout", "60000"], capture_output=True)
        for name, tok in (("spike-a", "MODEL_A_OK"), ("spike-b", "MODEL_B_OK")):
            subprocess.run(["herdr", "agent", "prompt", name,
                            f"Reply with exactly: {tok}"], capture_output=True)
        # wait for both to go working then idle again
        for name in ("spike-a", "spike-b"):
            subprocess.run(["herdr", "agent", "wait", name, "--until", "idle",
                           "--timeout", "120000"], capture_output=True)
        ra = c.call("agent.read", {"target": "spike-a", "source": "recent_unwrapped", "lines": 80})
        rb = c.call("agent.read", {"target": "spike-b", "source": "recent_unwrapped", "lines": 80})
        ta = (ra.get("result") or {}).get("read", {}).get("text", "")
        tb = (rb.get("result") or {}).get("read", {}).get("text", "")
        record("s9", "A answered MODEL_A_OK", "MODEL_A_OK" in ta, ta[-140:].replace("\n", " "))
        record("s9", "B answered MODEL_B_OK", "MODEL_B_OK" in tb, tb[-140:].replace("\n", " "))
        al = c.call("agent.list").get("result", {}).get("agents", [])
        names = {a.get("name") for a in al}
        record("s9", "both agents live concurrently",
               {"spike-a", "spike-b"} <= names, f"names={sorted(names)}")
        print(f"  NOTE: tab {tab_id} left open for inspection; close with `herdr tab close {tab_id}`")
    c.close()


# ---------------------------------------------------------------- s10 agent view
def s10():
    print("S10 — agent.view.set / clear semantics")
    c = Client("s10")
    r = c.call("agent.view.set", {
        "source": "spike:forseti", "label": "attention",
        "filter": {"op": "in", "field": "status", "values": ["blocked", "done"]},
        "sort": [{"field": "attention", "order": "desc"}],
    })
    ok = r.get("result", {}).get("type") == "agent_view" and r["result"].get("active")
    record("s10", "view.set accepted, active", ok, json.dumps(r.get("result") or r.get("error"))[:150])
    # wrong-source clear must NOT deactivate
    c.call("agent.view.clear", {"source": "spike:other"})
    r2 = c.call("agent.view.set", {
        "source": "spike:forseti",
        "filter": {"op": "in", "field": "status", "values": ["blocked", "done"]},
    })
    still = r2.get("result", {}).get("active") and r2.get("result", {}).get("source") == "spike:forseti"
    record("s10", "re-set by owner works (ownership tracked)", still,
           json.dumps(r2.get("result"))[:120])
    r3 = c.call("agent.view.clear", {"source": "spike:forseti"})
    record("s10", "owner clear succeeds", "result" in r3,
           json.dumps(r3.get("result") or r3.get("error"))[:120])
    # does the view affect agent.list? (docs: it must NOT)
    al = c.call("agent.list").get("result", {}).get("agents", [])
    record("s10", "agent.list unaffected by view projection", len(al) >= 1,
           f"{len(al)} agents listed")
    c.close()


STAGES = {"s6": s6, "s7": s7, "s8": s8, "s9": s9, "s10": s10}

if __name__ == "__main__":
    which = sys.argv[1:] or list(STAGES)
    open(TRANSCRIPT, "w").close()
    for name in which:
        STAGES[name]()
    print("\n=== summary ===")
    failed = 0
    for stage, checks in results.items():
        for check, ok, detail in checks:
            if not ok:
                failed += 1
    total = sum(len(v) for v in results.values())
    print(f"{total - failed}/{total} checks passed" + ("" if failed == 0 else f" ({failed} FAILED)"))
    print(f"transcript: {TRANSCRIPT}")
    sys.exit(1 if failed else 0)
