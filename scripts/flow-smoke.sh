#!/bin/sh
# flow-smoke — end-to-end gate for the flow language (F0 + F1).
#
#   scripts/flow-smoke.sh
#
# Builds forseti-flow if needed, then exercises validation, evidence evaluation,
# the cache, waivers, git hooks and the init drafts in a throwaway repo.
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
bin="${FORSETI_FLOW_BIN:-$repo/crew/bin/forseti-flow}"
if [ ! -x "$bin" ]; then
	(cd "$repo/crew" && go build -o bin/forseti-flow ./cmd/forseti-flow)
fi
[ -x "$bin" ] || { echo "flow-smoke: missing $bin" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/forseti-flow-smoke.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM
cd "$tmp"

fail() { echo "flow-smoke: FAIL: $*" >&2; exit 1; }
ok() { echo "flow-smoke: ok: $*"; }

git init -q
git config user.email smoke@example.com
git config user.name Smoke

mkdir -p .forseti
cat > .forseti/flow.yaml <<'YAML'
version: 1
name: smoke
mode: repo
evidence:
  proof:
    kind: shell
    run: "test -f EVIDENCE.txt"
    within: 30m
  docs:
    kind: diff
    changed: ["docs/**"]
  release_ok:
    kind: human
    prompt: "Ship this?"
on:
  pre_tool:
    deny:
      - match: "bash-re:^rm\\s+-rf"
        reason: "no recursive deletes"
  post_turn: {require: [docs], severity: warn}
  pre_commit: {require: [proof], severity: block}
  pre_push: {require: [release_ok], severity: block}
  commit_msg: {pattern: '^ok: .+'}
waivers: {ledger: .forseti/waivers.jsonl, ttl: 1h, never: [proof]}
YAML

"$bin" validate >/dev/null || fail "validate"
"$bin" explain | grep -q "flow smoke" || fail "explain"
"$bin" install-hooks --print | grep -q ">>> forseti flow" || fail "install-hooks --print"
"$bin" install-hooks >/dev/null || fail "install-hooks"
"$bin" ci | grep -q "forseti-flow" || fail "ci snippet"
"$bin" simulate | grep -q "^stages:" || fail "simulate"
if "$bin" check --stage pre_push >/dev/null 2>&1; then fail "human gate passed without --assume"; fi
"$bin" check --stage pre_push --assume release_ok >/dev/null || fail "human --assume"
ok "validate/explain/install-hooks/ci/simulate/human"

printf 'ok: init\n' > EVIDENCE.txt
git add .forseti EVIDENCE.txt
git commit -q -m 'ok: init' || fail "initial commit blocked"

# Waivers are scoped to the evidence/stage; proof is in never: here, so the
# waiver must be refused while the hook still blocks.
if "$bin" waive --stage pre_commit --evidence proof --reason nope >/dev/null 2>&1; then
	fail "waiver of never: evidence was accepted"
fi
rm EVIDENCE.txt
printf 'change\n' > code.txt
git add -A
if git commit -q -m 'ok: change' >/dev/null 2>&1; then fail "commit without proof passed"; fi
ok "hook blocks and never: waiver is refused"

# Cache: the evidence blob is fresh and reused on the second run.
"$bin" check --stage pre_commit >/dev/null 2>&1 || true
"$bin" check --stage pre_commit | grep -q "(cache)" || fail "expected a cached result"
"$bin" eval --evidence proof --json | grep -q '"status": "fail"' || fail "eval"
ok "cache reuse/eval"

# A non-never evidence can be waived.
sed -i.bak 's/, never: \[proof\]//' .forseti/flow.yaml && rm -f .forseti/flow.yaml.bak
"$bin" waive --stage pre_commit --evidence proof --reason "smoke waiver" >/dev/null || fail "waive"
"$bin" check --stage pre_commit | grep -q "WAIVED" || fail "waiver not applied"
git commit -q -m 'ok: change' || fail "commit with waiver blocked"
ok "waive unblocks the hook"

# Commit message gate.
git commit -q --allow-empty -m 'nope' >/dev/null 2>&1 && fail "bad commit message passed"
git commit -q --allow-empty -m 'ok: empty' || fail "good commit message blocked"
ok "commit-msg gate"

# post_turn is advisory.
"$bin" check --stage post_turn | grep -q "warning" || fail "post_turn warning missing"
ok "post_turn warning"

# Drafts validate in every preset.
for preset in repo review-loop docs-required release; do
	"$bin" init --print --preset "$preset" > "flow-$preset.yaml" || fail "init $preset"
	"$bin" validate -f "flow-$preset.yaml" >/dev/null || fail "draft $preset invalid"
done
ok "init presets validate"

echo "flow-smoke: PASS"
