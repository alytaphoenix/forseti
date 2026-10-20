#!/usr/bin/env python3
# crew-tui-drive-roles.py — P19 dev helper: drives the forseti-crew TUI's new
# role and pattern forms (plus the agent form's role field) in a REAL herdr pane
# of the sandbox session. Bubble Tea screens can't be unit-tested headlessly, so
# this is the live verification path for TUI work (same shape as
# crew-tui-drive.py, which drives the P11 phase form).
#
# Flow: bootstrap the sandbox session when absent → open the TUI on a seed crew
# → b (build) → l (role form) → g (pattern form) → a (agent + role field)
# → s (save) → validate the written file → quit → close the tab.
#
# The seed crew carries a `models:` map so the declared/pattern-synthesized
# roles' tiers resolve. Assertions ride on the SAVED FILE, not the status line:
# the TUI clips it at the left pane's width (P13 lesson).
import json, socket, os, time, sys, subprocess, tempfile, shutil

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.environ.get("FORSETI_CREW_BIN", os.path.join(REPO, "crew", "bin", "forseti-crew"))
DEF_SOCK = os.path.expanduser("~/.config/herdr/herdr.sock")
SOCK = os.path.expanduser("~/.config/herdr/sessions/sandbox/herdr.sock")


def call(method, params, timeout=15, sock=SOCK):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(sock)
    frame = json.dumps({"id": f"drv-{method}", "method": method, "params": params})
    s.sendall(frame.encode() + b"\n")  # NDJSON: newline-terminated frames (S6)
    buf = b""
    try:
        while True:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
    except socket.timeout:
        pass
    s.close()
    lines = buf.decode().strip().splitlines()
    if not lines:
        raise RuntimeError(f"{method}: empty reply (raw {buf!r})")
    return json.loads(lines[0])


def ok(r, what):
    if "error" in r:
        raise RuntimeError(f"{what}: {r['error']}")
    return r.get("result", {})


# Everything the drive creates lives in the SANDBOX session and a temp dir;
# cleanup() runs on failure too, so a crashed run can never leave a tab behind
# (and never, ever touch the live default session).
CLEANUP = {"tab": None, "work": None}


def cleanup():
    if CLEANUP.get("tab"):
        try:
            call("tab.close", {"tab_id": CLEANUP["tab"]}, 10, SOCK)
        except Exception:
            pass
        CLEANUP["tab"] = None
    if CLEANUP.get("work"):
        shutil.rmtree(CLEANUP["work"], ignore_errors=True)
        CLEANUP["work"] = None


def send_text(pane, text):
    ok(call("pane.send_text", {"pane_id": pane, "text": text}, 10), "send_text")


def key(pane, k):
    ok(call("pane.send_keys", {"pane_id": pane, "keys": [k]}, 10), "send_keys")


def wait_for(pane, substr, timeout=30):
    r = call("pane.wait_for_output", {"pane_id": pane, "source": "recent",
        "strip_ansi": True, "match": {"type": "substring", "value": substr},
        "timeout_ms": timeout * 1000}, timeout + 10)
    if "error" in r:
        raise RuntimeError(f"wait_for({substr!r}): {r['error']}")


def bootstrap_sandbox():
    """Spawn the named session the way the runner does: a temp pane of the
    default session running `herdr --session sandbox` (a named server needs a
    TTY + no inherited HERDR_* nesting markers). The server outlives the pane."""
    if os.path.exists(SOCK):
        return
    snap = ok(call("session.snapshot", {}, 15, DEF_SOCK), "snapshot")
    ws = snap["snapshot"]["focused_workspace_id"]
    cmd = ("env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_SOCKET_PATH -u HERDR_SESSION "
           "-u HERDR_WORKSPACE_ID -u HERDR_TAB_ID herdr --session sandbox")
    r = ok(call("layout.apply", {"workspace_id": ws, "tab_label": "forseti-sandbox-boot",
        "focus": False, "root": {"type": "pane", "label": "boot", "cwd": REPO,
        "command": ["sh", "-c", cmd]}}, 30, DEF_SOCK), "bootstrap layout")
    tab = r["layout"]["tab_id"]
    deadline = time.time() + 45
    while time.time() < deadline:
        if os.path.exists(SOCK):
            time.sleep(2)  # the socket can appear before panes are creatable
            break
        time.sleep(0.25)
    else:
        raise RuntimeError("sandbox socket never appeared within 45s")
    call("tab.close", {"tab_id": tab}, 10, DEF_SOCK)
    print("sandbox session bootstrapped")


def main():
    bootstrap_sandbox()
    WORK = tempfile.mkdtemp(prefix="p19-drive-")
    CLEANUP["work"] = WORK
    crew_yaml = f"""name: p19-tui-roles
models:
  efficient: opencode-go/beam-free
  capable: opencode-go/longcat-2.5-preview-free
agents:
  - {{name: designer, model: opencode-go/beam-free, prompt: "Write the design."}}
"""
    with open(f"{WORK}/crew.yaml", "w") as f:
        f.write(crew_yaml)

    snap = ok(call("session.snapshot", {}, 15, SOCK), "snapshot")
    ws = snap["snapshot"]["focused_workspace_id"]
    label = "p19-tui-roles-" + str(int(time.time()))
    r = ok(call("layout.apply", {"workspace_id": ws, "tab_label": label,
        "focus": False, "root": {"type": "pane", "label": "tui", "cwd": WORK}},
        45, SOCK), "layout")

    def walk(node):
        if node.get("type") == "pane":
            return node["pane_id"]
        return walk(node["first"]) if "first" in node else walk(node["second"])
    pane, tab = walk(r["layout"]["root"]), r["layout"]["tab_id"]
    CLEANUP["tab"] = tab
    print(f"pane {pane} tab {tab}")

    send_text(pane, f"{BIN} run -f {WORK}/crew.yaml")
    key(pane, "enter")
    # readiness = the TUI HEADER, never a string the shell also echoes: the
    # command line above contains the crew file's path, so a crew-name wait
    # matched the ECHO and the next keystroke arrived as type-ahead — before
    # the TUI had installed its input reader, which drops it (S33e)
    wait_for(pane, "CREW: p19-tui-roles [view]", 30)
    print("tui up")

    send_text(pane, "b")
    wait_for(pane, "add agent", 15)
    time.sleep(0.5)  # let the build panel settle before the first form key

    # P19: role form (name/profile/goal/constraints/tier/instruction/writes)
    send_text(pane, "l")
    wait_for(pane, "add role — field 1/7", 15)
    for v in ["architect", "Architect", "decide the shape of the change",
              "no implementation", "capable", "Write the design and name what you rejected.",
              "design"]:
        send_text(pane, v)
        key(pane, "enter")
    wait_for(pane, "role architect declared", 15)
    print("role declared live")

    # P19: pattern form (id/kind/subject/criteria/then)
    send_text(pane, "g")
    wait_for(pane, "add pattern — field 1/5", 15)
    for v in ["rev", "reviewer_loop", "designer", "every claim cites a file", ""]:
        if v:
            send_text(pane, v)
        key(pane, "enter")
    wait_for(pane, "pattern rev (reviewer_loop)", 15)
    print("pattern declared live")

    # P19: the agent form's role field rides along
    send_text(pane, "a")
    wait_for(pane, "add agent — field 1/5", 15)
    for v in ["impl", "opencode-go/beam-free", "implement the design", "", "architect"]:
        if v:
            send_text(pane, v)
        key(pane, "enter")
    wait_for(pane, "agent impl added", 15)
    print("agent with role added live")

    def saved_has(*substrs):
        try:
            with open(f"{WORK}/crew.yaml") as fh:
                body = fh.read()
            return all(s in body for s in substrs)
        except OSError:
            return False

    send_text(pane, "s")
    deadline = time.time() + 20
    while time.time() < deadline and not saved_has("roles:", "patterns:"):
        time.sleep(0.5)
    else:
        if not saved_has("roles:", "patterns:"):
            raise RuntimeError("save never wrote roles:/patterns: into crew.yaml")

    with open(f"{WORK}/crew.yaml") as f:
        saved = f.read()
    for want in ["architect", "reviewer_loop", "every claim cites a file", "role: architect"]:
        assert want in saved, f"{want!r} missing from saved crew:\n{saved}"
    assert "rev-reviewer" not in saved, f"save wrote a synthesized node:\n{saved}"
    v = subprocess.run([BIN, "validate", "-f", f"{WORK}/crew.yaml"], capture_output=True, text=True)
    assert v.returncode == 0, v.stdout + v.stderr
    print("saved + validate OK")

    send_text(pane, "q")
    send_text(pane, "q")
    time.sleep(1)
    cleanup()
    print("saved file:")
    print(saved)
    print("PASS")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print("FAIL:", e)
        cleanup()
        sys.exit(1)
