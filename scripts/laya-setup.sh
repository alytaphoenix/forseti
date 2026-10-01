#!/bin/sh
# laya-setup: create the pinned laya runtime (venv) + warm the checkpoint.
# Repo rule: pip only inside a venv. Idempotent — safe to re-run.
#   scripts/laya-setup.sh
# After setup: scripts/laya-serve.sh start
set -eu

VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
LAYA_VERSION="${FORSETI_LAYA_VERSION:-0.3.22}"

if [ ! -x "$VENV/bin/python" ]; then
  echo "laya-setup: creating venv at $VENV…"
  python3 -m venv "$VENV"
fi

echo "laya-setup: installing laya==$LAYA_VERSION (+serve)…"
"$VENV/bin/pip" install -q "laya==$LAYA_VERSION" "laya[serve]==$LAYA_VERSION"

echo "laya-setup: warming the english checkpoint (first call downloads + loads; ~20 s cold)…"
"$VENV/bin/python" - << 'EOF'
import warnings, time
from laya import Router
t0 = time.time()
r = Router().predict(
    {"text": "warmup"},
    {"w": {"type": "noul", "instructions": "Is this text the word warmup?"}},
)
print(f"laya-setup: warm ({time.time()-t0:.1f}s incl. load); sample P(true)={r['answers']['w']['noul']:.3f}")
EOF

echo "laya-setup: done — start the endpoint with: scripts/laya-serve.sh start"
