#!/usr/bin/env python3
"""crew-host-oracle.py — the Phase 18D-1 gate: the Rust crew engine must match Go.

The Go engine (`forseti-crew`) is the compatibility surface while the runner/TUI
port is staged. This script captures, from the Go binary, the exact
stdout/stderr/exit of

    forseti-crew validate -f FILE --explain

for a corpus of crews, and writes one JSON record per crew under
`host/tests/golden/`. `host/tests/crew_oracle.rs` replays the same corpus
through the Rust engine (`forseti-host crew validate`) and diffs the records, so
the Rust port can be finished stage by stage without either engine being frozen.

    scripts/crew-host-oracle.py            regenerate the goldens
    scripts/crew-host-oracle.py --check    fail if the committed goldens are stale

Corpus
  crew/examples/*.yaml          the shipped examples. A crew that needs the
                                Phase-19 layer (roles/patterns, stage 18D-2) is
                                skipped against an EXPLICIT list, so the gate
                                cannot silently narrow as examples are added.
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
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GOLDEN = ROOT / "host/tests/golden"

# Crews that need the Phase-19 layer (roles/patterns), which stage 18D-2 ports.
STAGE_18D2_SKIP = {"crew-sop.yaml"}

VALID_KINDS = ("valid", "invalid")


def relative(p: Path) -> str:
    return str(p.relative_to(ROOT))


def needs_stage_18d2(path: Path) -> bool:
    text = path.read_text()
    return bool(
        re.search(r"(?m)^(roles|patterns):", text) or re.search(r"(?m)^\s+role:", text)
    )


def corpus() -> list[tuple[str, Path]]:
    cases: list[tuple[str, Path]] = []
    skipped = set()
    for p in sorted((ROOT / "crew/examples").glob("*.yaml")):
        if needs_stage_18d2(p):
            skipped.add(p.name)
            continue
        cases.append(("examples", p))
    if skipped != STAGE_18D2_SKIP:
        sys.exit(
            "examples needing the Phase-19 layer changed: expected "
            f"{sorted(STAGE_18D2_SKIP)}, found {sorted(skipped)} — implement stage "
            "18D-2 or update STAGE_18D2_SKIP"
        )
    for kind in VALID_KINDS:
        for p in sorted((ROOT / f"host/tests/fixtures/{kind}").glob("*.yaml")):
            cases.append((kind, p))
    return cases


def capture(binary: Path, file: Path, env: dict) -> dict:
    argv = ["validate", "-f", relative(file), "--explain"]
    proc = subprocess.run(
        [str(binary), *argv], cwd=ROOT, capture_output=True, text=True, env=env
    )
    return {
        "file": relative(file),
        "argv": argv,
        "exit": proc.returncode,
        "stdout": proc.stdout,
        "stderr": proc.stderr,
    }


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
