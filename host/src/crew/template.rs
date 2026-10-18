//! The prompt-template layer (Phase 18D-5): the Go `text/template` subset the
//! crew engine uses, ported with the SAME parse-time failures the Go runner
//! produces (S32).
//!
//! Crew prompts are Go templates rendered at dispatch against a map of upstream
//! node outputs plus three runner functions. Go's `text/template` is a full
//! language; crews only use this surface:
//!
//! * `{{ .name }}` — field access into the upstream map. An absent key renders
//!   `<no value>`, exactly like Go's `missingkey=default` (the runner binds
//!   every *incoming* edge unconditionally, so a miss means the prompt named a
//!   node it does not edge from — P13 BUG-06 / S32).
//! * `{{ node "x" }}`, `{{ artifact "t" }}`, `{{ memory "q" }}` — a function
//!   with one string argument. The argument may contain `-`, which field access
//!   cannot express.
//! * `{{- … -}}` — trim markers.
//!
//! Go resolves a dot-less action as a **function** (not a variable), so
//! `{{ draft }}` fails with `function "draft" not defined`, and a hyphenated
//! field like `{{ .ap-critic-1 }}` fails with `bad character U+002D`. Both
//! messages are reproduced verbatim, including the `template: <name>:<line>:`
//! prefix, because they are the failure mode a crew author actually hits.
//!
//! Anything outside the subset is a clear error naming the action; there is no
//! silent fallback.

use std::collections::HashMap;

/// The runner-side function set (`node` / `artifact` / `memory`).
///
/// `is_known` runs at parse time (Go resolves function names then); `call` runs
/// at render time. Splitting them keeps the parse-time error identical without
/// giving the parser mutable access to the runner.
pub trait Funcs {
    /// Whether `name` is a registered template function.
    fn is_known(&self, name: &str) -> bool;

    /// Evaluates `name(arg)`.
    fn call(&mut self, name: &str, arg: &str) -> String;
}

/// A parsed template: literal text, a field reference, or a function call.
///
/// Public because the runner resolves calls in a separate pass (see
/// [`parse`] / [`render_parts`]): `{{ memory }}` and `{{ artifact }}` need
/// mutable state, and doing them in a second pass keeps the borrows sequential.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Part {
    Text(String),
    Field {
        name: String,
        line: usize,
    },
    Call {
        name: String,
        arg: String,
        line: usize,
    },
}

/// Parses and renders `text` in one step (Go parses per dispatch, so there is
/// no value in caching the parse tree here).
pub fn render(
    name: &str,
    text: &str,
    data: &HashMap<String, String>,
    funcs: &mut dyn Funcs,
) -> Result<String, String> {
    let parts = parse(name, text, &mut |n| funcs.is_known(n))?;
    Ok(render_parts(&parts, data, &mut |n, a| funcs.call(n, a)))
}

/// Parses `text` into parts. `known` is the parse-time function-name check
/// (Go resolves function names while parsing, so an unknown one is an error
/// even in a branch that would never run).
pub fn parse(
    name: &str,
    text: &str,
    known: &mut dyn FnMut(&str) -> bool,
) -> Result<Vec<Part>, String> {
    parse_with(name, text, known)
}

/// Renders parsed parts with pre-resolved call values, one per part
/// (`None` for non-call parts). The runner uses this: each call OCCURRENCE is
/// evaluated independently, exactly like Go's `Execute` (two `{{ memory "q" }}`
/// in one prompt are two recalls and two events).
pub fn render_resolved(
    parts: &[Part],
    data: &HashMap<String, String>,
    values: &[Option<String>],
) -> String {
    debug_assert_eq!(parts.len(), values.len(), "one value slot per part");
    let mut out = String::new();
    for (part, value) in parts.iter().zip(values) {
        match part {
            Part::Text(t) => out.push_str(t),
            Part::Field { name: field, .. } => match data.get(field) {
                Some(v) => out.push_str(v),
                None => out.push_str("<no value>"),
            },
            Part::Call { .. } => {
                if let Some(v) = value {
                    out.push_str(v);
                }
            }
        }
    }
    out
}

/// Renders parsed parts, resolving each call through `call` (the convenience
/// path used by [`render`] and the tests).
pub fn render_parts(
    parts: &[Part],
    data: &HashMap<String, String>,
    call: &mut dyn FnMut(&str, &str) -> String,
) -> String {
    let mut out = String::new();
    for part in parts {
        match part {
            Part::Text(t) => out.push_str(t),
            Part::Field { name: field, .. } => match data.get(field) {
                Some(v) => out.push_str(v),
                // Go's missingkey=default.
                None => out.push_str("<no value>"),
            },
            Part::Call { name, arg, .. } => out.push_str(&call(name, arg)),
        }
    }
    out
}

fn err(name: &str, line: usize, detail: impl std::fmt::Display) -> String {
    format!("template: {name}:{line}: {detail}")
}

fn parse_with(
    name: &str,
    text: &str,
    known: &mut dyn FnMut(&str) -> bool,
) -> Result<Vec<Part>, String> {
    let mut parts: Vec<Part> = Vec::new();
    let mut line = 1usize;
    let mut rest = text;
    let mut trim_next = false;

    while !rest.is_empty() {
        let Some(start) = rest.find("{{") else {
            push_text(&mut parts, rest, &mut trim_next);
            break;
        };
        let (before, after) = rest.split_at(start);
        push_text(&mut parts, before, &mut trim_next);
        line += before.matches('\n').count();
        let action_line = line;

        let mut body_at = 2; // past "{{"
        let mut trim_left = false;
        if after[body_at..].starts_with('-') {
            trim_left = true;
            body_at += 1;
        }
        let Some(close) = after[body_at..].find("}}") else {
            return Err(err(name, action_line, "unclosed action"));
        };
        let body_end = body_at + close;
        let mut body = &after[body_at..body_end];
        let mut trim_right = false;
        if let Some(b) = body.strip_suffix('-') {
            trim_right = true;
            body = b;
        }
        line += after[..body_end].matches('\n').count();
        rest = &after[body_end + 2..];

        if trim_left && let Some(Part::Text(t)) = parts.last_mut() {
            t.truncate(t.trim_end().len());
        }
        push_action(name, body, action_line, known, &mut parts)?;
        trim_next = trim_right;
    }
    Ok(parts)
}

fn push_text(parts: &mut Vec<Part>, text: &str, trim_next: &mut bool) {
    let text = if *trim_next { text.trim_start() } else { text };
    *trim_next = false;
    if text.is_empty() {
        return;
    }
    if let Some(Part::Text(prev)) = parts.last_mut() {
        prev.push_str(text);
    } else {
        parts.push(Part::Text(text.to_string()));
    }
}

fn push_action(
    name: &str,
    body: &str,
    line: usize,
    known: &mut dyn FnMut(&str) -> bool,
    parts: &mut Vec<Part>,
) -> Result<(), String> {
    let body = body.trim();
    if body.is_empty() {
        return Err(err(name, line, "missing value for command"));
    }
    if let Some(field) = body.strip_prefix('.') {
        if field.is_empty() {
            return Err(err(name, line, "missing value for command"));
        }
        for (i, ch) in field.chars().enumerate() {
            let ident = ch == '_' || ch.is_alphanumeric();
            if !ident || (i == 0 && ch.is_numeric()) {
                // Go: `bad character U+002D` for `{{ .ap-critic-1 }}`.
                return Err(err(
                    name,
                    line,
                    format!("bad character U+{:04X}", ch as u32),
                ));
            }
        }
        parts.push(Part::Field {
            name: field.to_string(),
            line,
        });
        return Ok(());
    }
    let Some((ident, arg)) = parse_call(body) else {
        return Err(err(name, line, "unsupported template action"));
    };
    if !known(ident) {
        return Err(err(name, line, format!("function \"{ident}\" not defined")));
    }
    let Some(arg) = arg else {
        return Err(err(
            name,
            line,
            format!("wrong number of args for {ident}: want 1 got 0"),
        ));
    };
    parts.push(Part::Call {
        name: ident.to_string(),
        arg,
        line,
    });
    Ok(())
}

/// `ident` or `ident "string literal"`.
fn parse_call(body: &str) -> Option<(&str, Option<String>)> {
    let mut end = 0;
    for (i, ch) in body.char_indices() {
        if i == 0 {
            if !(ch.is_alphabetic() || ch == '_') {
                return None;
            }
        } else if !(ch.is_alphanumeric() || ch == '_') {
            break;
        }
        end = i + ch.len_utf8();
    }
    let ident = &body[..end];
    let rest = body[end..].trim();
    if rest.is_empty() {
        return Some((ident, None));
    }
    let s = unquote(rest)?;
    Some((ident, Some(s)))
}

/// A double-quoted Go string literal with the escapes crews can write.
fn unquote(s: &str) -> Option<String> {
    let inner = s.strip_prefix('"')?.strip_suffix('"')?;
    let mut out = String::with_capacity(inner.len());
    let mut chars = inner.chars();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        match chars.next()? {
            'n' => out.push('\n'),
            't' => out.push('\t'),
            'r' => out.push('\r'),
            '"' => out.push('"'),
            '\\' => out.push('\\'),
            other => out.push(other),
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Stub {
        names: Vec<String>,
    }

    impl Stub {
        fn new(names: &[&str]) -> Self {
            Stub {
                names: names.iter().map(|n| n.to_string()).collect(),
            }
        }
    }

    impl Funcs for Stub {
        fn is_known(&self, name: &str) -> bool {
            matches!(name, "node" | "artifact" | "memory")
        }

        fn call(&mut self, name: &str, arg: &str) -> String {
            match name {
                "node" => {
                    if self.names.iter().any(|n| n == arg) {
                        format!("OUT:{arg}")
                    } else {
                        // the Go guard flags a nonexistent node name
                        format!("UNKNOWN:{arg}")
                    }
                }
                "artifact" => format!("ART:{arg}"),
                "memory" => format!("MEM:{arg}"),
                _ => format!("UNKNOWN-FUNC:{name}"),
            }
        }
    }

    fn data() -> HashMap<String, String> {
        let mut d = HashMap::new();
        d.insert("planner".to_string(), "the plan".to_string());
        d.insert("draft".to_string(), String::new());
        d
    }

    fn ok(text: &str) -> String {
        render("t", text, &data(), &mut Stub::new(&["draft"])).expect("renders")
    }

    #[test]
    fn field_access_renders_the_upstream_text() {
        assert_eq!(ok("Plan:\n{{ .planner }}"), "Plan:\nthe plan");
        // no spaces around the dots is the form every example uses
        assert_eq!(ok("{{.planner}}!"), "the plan!");
        // an empty upstream renders empty, not "<no value>" (P13 BUG-06)
        assert_eq!(ok("[{{ .draft }}]"), "[]");
    }

    #[test]
    fn a_missing_key_renders_go_s_no_value() {
        // the runner binds every INCOMING edge, so a miss is an authoring bug
        // and must surface exactly as it does in Go (missingkey=default)
        assert_eq!(ok("[{{ .reviewer }}]"), "[<no value>]");
    }

    #[test]
    fn function_calls_take_hyphenated_names() {
        let mut stub = Stub::new(&["ap-critic-1"]);
        let out = render(
            "t",
            "{{ node \"ap-critic-1\" }}|{{ artifact \"plan\" }}|{{ memory \"q\" }}",
            &data(),
            &mut stub,
        )
        .expect("renders");
        assert_eq!(out, "OUT:ap-critic-1|ART:plan|MEM:q");
        assert_eq!(ok("{{node \"draft\"}}"), "OUT:draft");
    }

    #[test]
    fn a_bare_name_is_a_function_reference() {
        // S32 fact 1: verbatim Go text
        let e = render(
            "rev-reviewer",
            "a\nb\nc\nd\n{{ draft }}",
            &data(),
            &mut Stub::new(&[]),
        )
        .unwrap_err();
        assert_eq!(
            e,
            "template: rev-reviewer:5: function \"draft\" not defined"
        );
    }

    #[test]
    fn a_hyphenated_field_is_a_parse_error() {
        // S32 fact 2: verbatim Go text (identifier-safe names only)
        let e = render(
            "ap-critic-1",
            "{{ .ap-critic-1 }}",
            &data(),
            &mut Stub::new(&[]),
        )
        .unwrap_err();
        assert_eq!(e, "template: ap-critic-1:1: bad character U+002D");
    }

    #[test]
    fn trim_markers_strip_surrounding_whitespace() {
        assert_eq!(ok("a\n   {{- .planner -}}   \nb"), "athe planb");
    }

    #[test]
    fn unclosed_actions_and_unknown_forms_are_errors() {
        assert_eq!(
            render("t", "{{ .planner", &data(), &mut Stub::new(&[])).unwrap_err(),
            "template: t:1: unclosed action"
        );
        assert!(
            render("t", "{{ 3 + 4 }}", &data(), &mut Stub::new(&[]))
                .unwrap_err()
                .contains("unsupported template action")
        );
    }

    #[test]
    fn string_arguments_decode_escapes() {
        assert_eq!(ok(r#"{{ artifact "a\"b" }}"#), "ART:a\"b");
        assert_eq!(ok(r#"{{ memory "line\nnext" }}"#), "MEM:line\nnext");
    }

    #[test]
    fn every_example_prompt_parses_and_renders() {
        // The S32 guard, Rust side: every prompt of every EXAMPLE crew (and
        // every phase's instructions — they render through the same template)
        // must parse and render. A bare or hyphenated-broken reference fails
        // here instead of dying one node into a live run.
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../crew/examples");
        let mut checked = 0;
        let mut files = 0;
        for entry in std::fs::read_dir(&dir).expect("crew/examples") {
            let path = entry.expect("dir entry").path();
            if path.extension().and_then(|e| e.to_str()) != Some("yaml") {
                continue;
            }
            let text = std::fs::read_to_string(&path).expect("read crew");
            let Ok(crew) = crate::crew::load(&text) else {
                continue; // the oracle owns validation
            };
            files += 1;
            let names: Vec<&str> = crew.agents.iter().map(|a| a.name.as_str()).collect();
            let mut stub = Stub::new(&names);
            let mut data = HashMap::new();
            for n in &names {
                data.insert(n.to_string(), format!("OUT:{n}"));
            }
            for a in &crew.agents {
                render(&a.name, &a.prompt, &data, &mut stub)
                    .unwrap_or_else(|e| panic!("{}: node {}: {e}", path.display(), a.name));
                checked += 1;
            }
            for p in &crew.phases {
                render(&p.name, &p.instructions, &data, &mut stub)
                    .unwrap_or_else(|e| panic!("{}: phase {}: {e}", path.display(), p.name));
                checked += 1;
            }
        }
        assert!(files >= 8, "expected the example crews, saw {files}");
        assert!(checked >= 20, "expected many prompts, checked {checked}");
    }
}
