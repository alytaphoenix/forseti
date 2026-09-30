// Forseti pi extension (Phase 2b) — implemented in Phase 2b of docs/implementation-plan.md.
// Design: docs/design.md §3. Dev-load: pi --extension ./pi-extension/index.ts
// Planned registers:
//   /ttt jump <path> [line] [end_line]  — write FORSETI_JUMP_FILE + POST /exec "Forseti: Jump"
//   /ttt follow on|off                  — tool_result hook -> jump push (default off)
//   /ttt diff                           — open working-tree diff in ttt
//   /herd list|agents                   — read-only herdr pass-through (HERDR_ENV=1)
export default function (_pi: unknown) {
  // intentionally inert until Phase 2b
}
