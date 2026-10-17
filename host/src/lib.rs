// Forseti host — the Rust side of forseti (Phase 18D).
//
// Phase 18A made this crate an in-process switchyard router (see `src/main.rs`);
// Phase 18D grows it into the crew **engine** (herdr client, scheduler, runner,
// TUI). While the port is staged the Go `forseti-crew` stays the compatibility
// surface: `scripts/crew-host-oracle.py` pins this crate's crew behaviour to the
// Go engine's by way of the checked-in goldens under `host/tests/golden/`.
#![allow(dead_code)] // later 18D stages consume the whole crew model.

pub mod crew;
