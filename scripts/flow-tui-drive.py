#!/usr/bin/env python3
# flow-tui-drive.py — PTY drive test for the flow builder TUI (F5).
#
# Opens `forseti-flow edit` in a real herdr pane of the sandbox session,
# navigates overview → stages → overview, validates, saves, and quits. Bubble
# Tea screens cannot be unit-tested headlessly; this is the integration probe.
#
# Requires: the sandbox herdr session (crew-smoke.sh bootstraps it) and the
# forseti-flow binary.
import json, os, socket, subprocess, sys, tempfile, time, shutil

SOCK = os.path.expanduser("~/.config/herdr/sessions/sandbox/herdr.sock")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.environ.get("FORSETI_FLOW_BIN", os.path.join(REPO, "crew", "bin", "forseti-flow"))


def call(method, params, timeout=15):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(SOCK)
    s.sendall((json.dumps({"id": f"drv-{method}", "method": method, "params": params}) + "\n").encode())
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
        raise RuntimeError(f"{method}: empty reply")
    r = json.loads(lines[0])
    if "error" in r:
        raise RuntimeError(f"{method}: {r['error']}")
    return r.get("result", {})


def send_text(pane, text):
    call("pane.send_text", {"pane_id": pane, "text": text}, 10)


def key(pane, k):
    call("pane.send_keys", {"pane_id": pane, "keys": [k]}, 10)


def wait_for(pane, text, timeout=20):
    r = call("pane.wait_for_output", {"pane_id": pane, "source": "recent", "strip_ansi": True,
        "match": {"type": "substring", "value": text}, "timeout_ms": timeout * 1000}, timeout + 10)
    if "error" in r:
        raise RuntimeError(f"wait_for({text!r}): {r['error']}")


def main():
    work = tempfile.mkdtemp(prefix="flow-tui-")
    flow_path = os.path.join(work, "flow.yaml")
    with open(flow_path, "w") as f:
        f.write("""version: 1
name: tui-drive
mode: repo
evidence:
  x: {kind: shell, run: "true"}
on:
  pre_commit: {require: [x]}
""")
    snap = call("session.snapshot", {}, 15)
    ws = snap["snapshot"]["focused_workspace_id"]
    label = "flow-tui-" + str(int(time.time()))
    r = call("layout.apply", {"workspace_id": ws, "tab_label": label, "focus": False,
        "root": {"type": "pane", "label": "flow-tui", "cwd": work}}, 45)

    def walk(node):
        return node["pane_id"] if node.get("type") == "pane" else walk(node.get("first") or node.get("second"))

    pane, tab = walk(r["layout"]["root"]), r["layout"]["tab_id"]
    send_text(pane, f"{BIN} edit -f {flow_path}")
    key(pane, "enter")
    wait_for(pane, "1 overview", 30)
    print("builder up")

    send_text(pane, "3")
    wait_for(pane, "stages:", 15)
    send_text(pane, "esc")
    wait_for(pane, "1 overview", 15)
    print("navigation ok")

    send_text(pane, "v")
    wait_for(pane, "valid", 15)
    send_text(pane, "s")
    wait_for(pane, "saved", 15)
    print("validate + save ok")

    key(pane, "esc")
    time.sleep(1)
    call("tab.close", {"tab_id": tab}, 10)

    v = subprocess.run([BIN, "validate", "-f", flow_path], capture_output=True, text=True)
    if v.returncode != 0:
        raise RuntimeError(v.stdout + v.stderr)
    print("PASS")
    shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print("FAIL:", e)
        sys.exit(1)
