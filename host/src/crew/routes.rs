//! switchyard route typing (Phase 18B/18C), ported for validation.
//!
//! forseti derives the typed keys for the tier shapes it understands and passes
//! `options:` through verbatim, so the router (this same crate, `main.rs`) stays
//! the authority on semantics.

use super::model::{Route, Value};
use std::collections::BTreeMap;

/// Mirrors switchyard 0.3.0's `AlgorithmSpec` variants.
pub const KNOWN_ROUTE_TYPES: [&str; 10] = [
    "stage_router",
    "auto",
    "plan_execute",
    "llm_classifier",
    "passthrough",
    "random",
    "noop",
    "composite",
    "advisor",
    "prefill_router",
];

pub fn known_route_type(t: &str) -> bool {
    KNOWN_ROUTE_TYPES.contains(&t)
}

/// The (capable key, efficient key) a tiered route type derives.
pub fn route_tier_keys(t: &str) -> Option<(&'static str, &'static str)> {
    match t {
        "stage_router" | "auto" | "plan_execute" => Some(("capable_target", "efficient_target")),
        "llm_classifier" => Some(("strong_target", "weak_target")),
        _ => None,
    }
}

/// These take no typed keys from forseti — their whole route table comes from
/// `options`.
pub fn option_driven(t: &str) -> bool {
    matches!(t, "composite" | "advisor" | "prefill_router")
}

pub fn valid_picker(p: &str) -> bool {
    matches!(p, "efficient_first" | "capable_first")
}

impl Route {
    /// The route type, defaulted to stage_router.
    pub fn effective_type(&self) -> &str {
        if self.type_.is_empty() {
            "stage_router"
        } else {
            &self.type_
        }
    }

    /// The switchyard target names forseti generates for this route.
    pub fn target_names(&self) -> (String, String) {
        (format!("{}_eff", self.id), format!("{}_cap", self.id))
    }

    /// The target name generated for a route's dedicated judge model.
    pub fn judge_target(&self) -> String {
        format!("{}_judge", self.id)
    }

    /// The switchyard route table forseti emits: the typed keys of the route's
    /// type, plus `options` merged in. A collision is an error — forseti's typed
    /// keys are authoritative.
    pub fn route_keys(&self) -> Result<BTreeMap<String, Value>, String> {
        let t = self.effective_type();
        if !known_route_type(t) {
            return Err(format!(
                "route {:?}: unknown switchyard type {:?} (known: {})",
                self.id,
                t,
                KNOWN_ROUTE_TYPES.join(", ")
            ));
        }
        let (eff, cap) = self.target_names();
        let mut keys: BTreeMap<String, Value> = BTreeMap::new();
        if let Some((cap_key, eff_key)) = route_tier_keys(t) {
            keys.insert(cap_key.to_string(), v(cap.clone()));
            keys.insert(eff_key.to_string(), v(eff.clone()));
        }
        match t {
            "stage_router" => {
                let picker = if self.picker.is_empty() {
                    "efficient_first"
                } else {
                    &self.picker
                };
                let conf = if self.confidence == 0.0 {
                    0.5
                } else {
                    self.confidence
                };
                keys.insert("picker".to_string(), v(picker));
                keys.insert("confidence_threshold".to_string(), v(conf));
            }
            "passthrough" => {
                keys.insert("target".to_string(), v(cap.clone()));
            }
            "random" => {
                keys.insert("targets".to_string(), v(vec![cap.clone(), eff.clone()]));
            }
            _ => {}
        }
        if t == "llm_classifier" {
            // a dedicated judge model makes llm_classifier ergonomic.
            if !self.judge.is_empty() && !self.options.contains_key("classifier_target") {
                keys.insert("classifier_target".to_string(), v(self.judge_target()));
            }
            if !self.options.contains_key("base_threshold") {
                keys.insert("base_threshold".to_string(), v(0.5));
            }
        }
        for (k, val) in &self.options {
            if k == "id" || k == "type" {
                return Err(format!(
                    "route {:?}: options.{} is set by forseti",
                    self.id, k
                ));
            }
            if keys.contains_key(k) {
                return Err(format!(
                    "route {:?}: options.{} duplicates the {} field forseti derives",
                    self.id, k, t
                ));
            }
            keys.insert(k.clone(), val.clone());
        }
        Ok(keys)
    }
}

fn v<T: serde::Serialize>(x: T) -> Value {
    serde_norway::to_value(x).unwrap_or(Value::Null)
}
