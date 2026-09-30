-- Forseti ttt plugin (Phase 2a) — implemented in Phase 2a of docs/implementation-plan.md.
-- Design: docs/design.md §2. manifest: plugin.ttt.json
-- Planned registers:
--   Forseti: Jump   (palette command; reads FORSETI_JUMP_FILE, opens tab, cursor + hunk selection)
--   forseti.ask     (ctrl+k a; selection/buffer -> herdr agent prompt --wait)
--   sidebar "Forseti" (set_interval poll of `herdr agent list --json`)
return {}
