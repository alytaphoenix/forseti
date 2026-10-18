#!/usr/bin/env python3
"""crew-host-oracle.py — the Phase 18D gate: the Rust crew engine must match Go.

The Go engine (`forseti-crew`) is the compatibility surface while the runner/TUI
port is staged. This script captures, from the Go binary, the exact
stdout/stderr/exit of

    forseti-crew validate -f FILE --explain
    forseti-crew prompts  -f FILE            (the resolved prompt templates)

for a corpus of crews, and writes one JSON record per crew under
`host/tests/golden/`. `host/tests/crew_oracle.rs` replays the same corpus
through the Rust engine (`forseti-host crew validate`) and diffs the records, so
the Rust port can be finished stage by stage without either engine being frozen.
Each record carries a nested `prompts` capture too: `--explain` pins the graph,
`prompts` pins the synthesized prompt text the runner renders (a pattern/role
text drift is otherwise invisible until a live run — docs/spikes.md S35).

    scripts/crew-host-oracle.py            regenerate the goldens
    scripts/crew-host-oracle.py --check    fail if the committed goldens are stale

Corpus
  crew/examples/*.yaml          the shipped examples (all of them; 18D-2 landed
                                the Phase-19 layer, so `crew-sop.yaml` is in).
  host/tests/fixtures/valid/    valid crews that add coverage the examples lack
  host/tests/fixtures/invalid/  one crew per validation rule

The Go `validate` shell-outs to the router for a routes dry-run; we point
`FORSETI_CREW_ROUTER_BIN` at a stub that exits 0 so the captured output is
deterministic and independent of whichever router build happens to be present.
(The Rust engine has no route dry-run yet — that is a later 18D stage.)
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GOLDEN = ROOT / "host/tests/golden"

VALID_KINDS = ("valid", "invalid")


def relative(p: Path) -> str:
    return str(p.relative_to(ROOT))


def corpus() -> list[tuple[str, Path]]:
    cases: list[tuple[str, Path]] = []
    for p in sorted((ROOT / "crew/examples").glob("*.yaml")):
        cases.append(("examples", p))
    for kind in VALID_KINDS:
        for p in sorted((ROOT / f"host/tests/fixtures/{kind}").glob("*.yaml")):
            cases.append((kind, p))
    return cases


def run(binary: Path, argv: list[str], env: dict) -> dict:
    proc = subprocess.run(
        [str(binary), *argv], cwd=ROOT, capture_output=True, text=True, env=env
    )
    return {
        "argv": argv,
        "exit": proc.returncode,
        "stdout": proc.stdout,
        "stderr": proc.stderr,
    }


def capture(binary: Path, file: Path, env: dict) -> dict:
    # `validate --explain` pins the graph; `prompts` pins the prompt templates,
    # so a role/pattern text drift cannot hide behind an unchanged graph.
    rec = {"file": relative(file)}
    rec.update(run(binary, ["validate", "-f", relative(file), "--explain"], env))
    rec["prompts"] = run(binary, ["prompts", "-f", relative(file)], env)
    return rec


def render(record: dict) -> str:
    return json.dumps(record, indent=2, sort_keys=True) + "\n"


def dest_for(root: Path, kind: str, path: Path) -> Path:
    return root / kind / (path.stem + ".json")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--check",
        action="store_true",
        help="fail if the committed goldens differ from a fresh capture",
    )
    args = ap.parse_args()

    # The oracle is the Go engine; build it.
    binary = ROOT / "crew/bin/forseti-crew"
    subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/forseti-crew"],
        cwd=ROOT / "crew",
        check=True,
    )

    tmp = Path(tempfile.mkdtemp(prefix="forseti-oracle-"))
    try:
        stub = tmp / "router-stub"
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(0o755)
        env = dict(os.environ)
        env["FORSETI_CREW_ROUTER_BIN"] = str(stub)
        for k in ("FORSETI_CREW_ROUTER", "FORSETI_LIVE_ROUTER"):
            env.pop(k, None)

        out_root = (tmp / "golden") if args.check else GOLDEN
        out_root.mkdir(parents=True, exist_ok=True)

        cases = corpus()
        records = []
        for kind, path in cases:
            rec = capture(binary, path, env)
            dest = dest_for(out_root, kind, path)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(render(rec))
            records.append((kind, path, rec))

        if not args.check:
            print(f"wrote {len(records)} goldens under {relative(GOLDEN)}/")
            return 0

        bad = 0
        expected = set()
        for kind, path, rec in records:
            committed = dest_for(GOLDEN, kind, path)
            expected.add(committed)
            if not committed.exists():
                print(f"MISSING    {relative(committed)}")
                bad += 1
                continue
            if committed.read_text() != render(rec):
                print(f"STALE      {relative(committed)}")
                print("  committed:", repr(committed.read_text()[:500]))
                print("  fresh    :", repr(render(rec)[:500]))
                bad += 1
        for p in sorted(GOLDEN.rglob("*.json")):
            if p not in expected:
                print(f"UNEXPECTED {relative(p)}")
                bad += 1
        if bad:
            print(f"{bad} golden(s) stale or unexpected — run scripts/crew-host-oracle.py")
            return 1
        print(f"goldens current ({len(records)} records)")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
