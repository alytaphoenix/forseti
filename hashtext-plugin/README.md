# Forseti hashtext plugin

Agent-side component: a **fingerprinted context cache** for pi sessions. The
general conversation stays hot; bulky material goes cold and is reconstructed
only when the task needs it.

```
/hash list                          show the cold index
/hash put <file> [name]             store a file as a cold block
/hash expand <ref>                  mark a block resident (content returns)
/hash evict <ref>                   drop it from the hot set (marker remains)
/hash stats                         store size / compression
/hash clear                         evict everything in this session
```

Model tools: `hashtext_put`, `hashtext_list`, `hashtext_expand`,
`hashtext_evict`.

## How it works

```
┌ system prompt ─────────────────────────────────────────────┐
│ <hashtext>                                                 │
│ Cold context cache — 3 blocks …                            │
│ a3f2c1b9d4e0 | ci log | failing build output | 41KB→7.9KB  │  always hot,
│ …                                                          │  index only
└────────────────────────────────────────────────────────────┘
conversation transcript
    user: …                     [hashtext a3f2c1b9d4e0 resident]   ← marker stub
    assistant/tool results …
                              │ before every LLM call
                              ▼  (pi `context` event)
    user: …                     [hashtext a3f2c1b9d4e0 "ci log" — expanded]
                                …decompressed contents…
                                [/hashtext a3f2c1b9d4e0]
```

- **Identity is content.** The block ref is the sha256 prefix of the plaintext;
  identical text dedups, changed text gets a new ref. Nothing to invalidate.
- **The transcript never carries the bulk.** Expanding leaves a one-line marker;
  the `context` handler substitutes decompressed text before each model call.
  Evicting stops the substitution — the marker stays as a cheap stub.
- **Agent-declared, not heuristic.** No embeddings, no silent retrieval: the
  model sees the index and decides what to expand.
- **Budget guard.** Expansions are injected in residency order while they fit
  under 90% of the context window; blocks that do not fit are withheld with a
  visible note instead of overflowing the request.
- **Residency survives reload.** Stored per session under
  `<store>/resident/<session>.json`.

The extension never writes its own cache format — the store is the `hashtext`
Python CLI (sibling repo: content-addressed zstd blocks). It keeps an in-memory
decompression cache, safe because a block's identity is its content hash.

## Install

```sh
# 1 · the store CLI (once)
cd ~/repos/hashtext && uv tool install .   # or: pipx install .

# 2 · the extension (user-level, every pi session)
cp hashtext-plugin/index.ts ~/.pi/agent/extensions/hashtext.ts
# or as a package:
pi install /path/to/forseti/hashtext-plugin
```

Environment: `HASHTEXT_DIR` (store location, default `~/.hashtext`),
`HASHTEXT_BIN` (CLI path; falls back to `hashtext` on PATH, then
`~/.local/bin/hashtext`, then Homebrew prefixes).

## Notes

- A missing CLI never breaks a turn: tools report the install hint once, and
  the index section is simply omitted.
- Expanded content is re-sent each model call while resident — the saving is
  that it is not *retained* across turns, and that cold blocks cost one index
  line each.
- Residency state resets with `/hash clear` or a new session.
- Extension state resets on `/reload`; residency is reloaded from disk.
- Read-only with respect to your files: it only reads files you pass to
  `/hash put`.
