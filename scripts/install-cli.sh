#!/bin/sh
# install-cli.sh — build the umbrella CLI and install it on PATH.
#
#   scripts/install-cli.sh                 -> ~/.local/bin/forseti
#   FORSETI_BIN_DIR=/usr/local/bin ...     -> another bin dir
#
# The repo path is stamped into the binary (defaultRepo), so the installed
# command finds ttt-plugin/, pi-extension/, host/ and scripts/ from anywhere.
set -eu

REPO=$(cd "$(dirname "$0")/.." && pwd)
BIN_DIR=${FORSETI_BIN_DIR:-$HOME/.local/bin}

mkdir -p "$BIN_DIR"
cd "$REPO/cli"
go build -ldflags "-X main.defaultRepo=$REPO" -o "$BIN_DIR/forseti" .
printf 'installed %s (repo %s)\n' "$BIN_DIR/forseti" "$REPO"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) printf 'note: %s is not on PATH — add it to use `forseti` directly\n' "$BIN_DIR" ;;
esac
