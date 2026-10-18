//! The UI-only live streams (Phase 18D-6): the Rust port of the Go
//! `pumpStatus` subscription and the `watch:` pane waiters.
//!
//! Neither affects scheduling — correctness rests on `agent.prompt.wait`. They
//! exist so a run has the same visible event stream as the Go engine:
//!
//! * `pane_agent_status_changed` → `node_status` events + `live_status` on the
//!   node (and a `blocked` transition while the node is still running, matching
//!   the Go engine's badge/notification path).
//! * `watch: [{node, match}]` → `pattern_matched` events, deduped by line.
//!   `pane.wait_for_output` matches past scrollback instantly, so the dedupe is
//!   what keeps a watcher from replaying the prompt echo (S12).
//!
//! Threads own their herdr subscriptions; the driver merges the shared maps into
//! the scheduler at each flush and drains the queued events into the run log.

use std::collections::{BTreeMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::RecvTimeoutError;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use serde_json::json;

use super::herdr::Client;
use super::model::Crew;
use super::scheduler::{Event, Scheduler, Status};

/// Pane↔node mapping, the nodes currently running (node → pane), and herdr's
/// last reported agent status per node.
#[derive(Debug, Default)]
pub struct LiveMaps {
    pub pane_to_node: BTreeMap<String, String>,
    pub running: BTreeMap<String, String>,
    pub statuses: BTreeMap<String, String>,
}

/// Applies a status push. Returns the `node_status` event to emit when the pane
/// belongs to a crew node.
pub fn apply_status(maps: &mut LiveMaps, pane_id: &str, status: &str) -> Option<Event> {
    let node = maps.pane_to_node.get(pane_id)?.clone();
    maps.statuses.insert(node.clone(), status.to_string());
    let mut ev = Event::on_node("node_status", &node).with_info(status);
    ev.ts = super::scheduler::rfc3339_now();
    Some(ev)
}

/// The `pattern_matched` event for one watcher hit.
pub fn watch_event(node: &str, pattern: &str, line: &str) -> Event {
    let mut ev = Event::on_node("pattern_matched", node);
    ev.ts = super::scheduler::rfc3339_now();
    ev.line = line.to_string();
    ev.info = format!("watch {pattern}");
    ev
}

/// Merges herdr's pushes into the scheduler's node states (Go `pumpStatus`): the
/// badge status, and a `blocked` transition for a node that is still running.
pub fn apply_live(maps: &LiveMaps, sched: &mut Scheduler) {
    for (node, status) in &maps.statuses {
        let Some(st) = sched.node_mut(node) else {
            continue;
        };
        st.live_status = status.clone();
        if status == "blocked" && st.status == Status::Running {
            // a blocked agent is terminal for scheduling: it needs the human
            st.status = Status::Blocked;
            st.ended = Some(std::time::SystemTime::now());
        }
    }
}

/// One run's live streams.
pub struct Live {
    stop: Arc<AtomicBool>,
    queued: Arc<Mutex<Vec<Event>>>,
    maps: Arc<Mutex<LiveMaps>>,
    handles: Vec<JoinHandle<()>>,
}

impl Live {
    /// Subscribes to every crew pane and arms one waiter per `watch:` entry.
    pub fn start(client: Arc<Client>, crew: &Crew, pane_ids: &[String]) -> Live {
        let stop = Arc::new(AtomicBool::new(false));
        let queued: Arc<Mutex<Vec<Event>>> = Arc::new(Mutex::new(Vec::new()));
        let mut maps = LiveMaps::default();
        for (i, a) in crew.agents.iter().enumerate() {
            if let Some(pane) = pane_ids.get(i) {
                maps.pane_to_node.insert(pane.clone(), a.name.clone());
            }
        }
        let maps = Arc::new(Mutex::new(maps));
        let mut handles = Vec::new();

        // status stream (Go 6B-2/6B-3): one subscription, one entry per pane
        let subs: Vec<serde_json::Value> = pane_ids
            .iter()
            .map(|p| json!({"type": "pane.agent_status_changed", "pane_id": p}))
            .collect();
        if let Ok(mut sub) = client.subscribe(&subs, Duration::from_secs(15)) {
            let stop = Arc::clone(&stop);
            let queued = Arc::clone(&queued);
            let maps = Arc::clone(&maps);
            let client = Arc::clone(&client);
            handles.push(std::thread::spawn(move || {
                while !stop.load(Ordering::SeqCst) {
                    match sub.events().recv_timeout(Duration::from_millis(500)) {
                        Ok(ev) => {
                            let pane = ev.data.get("pane_id").and_then(|v| v.as_str());
                            let status = ev.data.get("agent_status").and_then(|v| v.as_str());
                            let (Some(pane), Some(status)) = (pane, status) else {
                                continue;
                            };
                            let event = maps
                                .lock()
                                .ok()
                                .and_then(|mut m| apply_status(&mut m, pane, status));
                            if let Some(ev) = event {
                                if let Ok(mut q) = queued.lock() {
                                    q.push(ev);
                                }
                                if status == "blocked" {
                                    // best-effort: notifications are disabled on
                                    // this setup; the ttt badge is the alert
                                    let _ = client.notification_show(
                                        "forseti-crew: agent needs you",
                                        "agent blocked — approval/question UI in its pane",
                                        "request",
                                    );
                                }
                            }
                        }
                        Err(RecvTimeoutError::Timeout) => continue,
                        Err(RecvTimeoutError::Disconnected) => break,
                    }
                }
                sub.close();
            }));
        }

        // watch waiters (6C-1): advisory pattern events
        for w in &crew.watch {
            let stop = Arc::clone(&stop);
            let queued = Arc::clone(&queued);
            let maps = Arc::clone(&maps);
            let client = Arc::clone(&client);
            let node = w.node.clone();
            let pattern = w.match_.clone();
            handles.push(std::thread::spawn(move || {
                let mut seen: HashSet<String> = HashSet::new();
                while !stop.load(Ordering::SeqCst) {
                    let pane = maps.lock().ok().and_then(|m| m.running.get(&node).cloned());
                    let Some(pane) = pane else {
                        std::thread::sleep(Duration::from_millis(500));
                        continue;
                    };
                    match client.pane_wait_for_output(
                        &pane,
                        "regex",
                        &pattern,
                        Duration::from_secs(30),
                    ) {
                        Ok(line) => {
                            if seen.insert(line.clone())
                                && let Ok(mut q) = queued.lock()
                            {
                                q.push(watch_event(&node, &pattern, &line));
                            }
                            std::thread::sleep(Duration::from_millis(250));
                        }
                        Err(_) => std::thread::sleep(Duration::from_millis(500)),
                    }
                }
            }));
        }

        Live {
            stop,
            queued,
            maps,
            handles,
        }
    }

    pub fn queued(&self) -> Arc<Mutex<Vec<Event>>> {
        Arc::clone(&self.queued)
    }

    pub fn maps(&self) -> Arc<Mutex<LiveMaps>> {
        Arc::clone(&self.maps)
    }

    /// Sets/clears the running pane for a node (the driver calls this around
    /// each node so the watchers know when to poll).
    pub fn set_running(maps: &Arc<Mutex<LiveMaps>>, node: &str, pane: Option<&str>) {
        if let Ok(mut m) = maps.lock() {
            match pane {
                Some(p) => {
                    m.running.insert(node.to_string(), p.to_string());
                }
                None => {
                    m.running.remove(node);
                }
            }
        }
    }

    pub fn stop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        for h in self.handles.drain(..) {
            let _ = h.join();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_status_push_maps_to_its_node_and_only_for_crew_panes() {
        let mut maps = LiveMaps::default();
        maps.pane_to_node.insert("w1:p1".into(), "planner".into());
        let ev = apply_status(&mut maps, "w1:p1", "working").expect("crew pane");
        assert_eq!(ev.kind, "node_status");
        assert_eq!(ev.node, "planner");
        assert_eq!(ev.info, "working");
        assert_eq!(
            maps.statuses.get("planner").map(String::as_str),
            Some("working")
        );
        assert!(
            apply_status(&mut maps, "w9:p9", "working").is_none(),
            "not a crew pane"
        );
    }

    #[test]
    fn a_watch_hit_carries_the_matched_line_and_pattern() {
        let ev = watch_event("builder", "DONE", "reply DONE");
        assert_eq!(ev.kind, "pattern_matched");
        assert_eq!(ev.node, "builder");
        assert_eq!(ev.line, "reply DONE");
        assert_eq!(ev.info, "watch DONE");
    }

    #[test]
    fn a_blocked_push_ends_a_running_node_but_not_a_settled_one() {
        let crew = crate::crew::load(
            r#"
name: live
agents:
  - {name: a, prompt: x}
"#,
        )
        .unwrap();
        let mut sched = Scheduler::new(crew, crate::crew::Options::default());
        sched.node_mut("a").unwrap().status = Status::Running;
        let mut maps = LiveMaps::default();
        maps.statuses.insert("a".into(), "blocked".into());
        apply_live(&maps, &mut sched);
        assert_eq!(sched.node("a").unwrap().status, Status::Blocked);
        assert_eq!(sched.node("a").unwrap().live_status, "blocked");

        // an already-done node must not be flipped back to blocked by a late push
        sched.node_mut("a").unwrap().status = Status::Done;
        apply_live(&maps, &mut sched);
        assert_eq!(sched.node("a").unwrap().status, Status::Done);
    }
}
