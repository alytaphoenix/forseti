//! Verdict parsing (Phase 19B / 18D-2): the Rust port of `bus.go`'s
//! `ParseVerdict`. The typed-bus helpers (`WritesType`, `Producers`,
//! `deriveSubscriptions`) already live on `model` because the base graph needs
//! them.
//!
//! `when: verdict:<label>` fires only when the producing node's captured output
//! carries that verdict. The LAST verdict line wins: inspector/prompt macros
//! print the pane transcript, which echoes the prompt (naming the expected
//! verdicts) before the model's own answer.

use regex::Regex;
use std::sync::OnceLock;

fn verdict_line() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r"(?im)^[ \t>*-]*VERDICT[ \t]*:[ \t]*([A-Za-z0-9][A-Za-z0-9_-]*)").unwrap()
    })
}

fn bare_verdict() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"(?im)^[ \t>*-]*(LGTM|LBTM)[ \t]*$").unwrap())
}

/// Extracts the verdict label a node's output carries, lowercased. The last
/// verdict line wins; a bare LGTM/LBTM (MetaGPT's convention) is a fallback.
pub fn parse_verdict(output: &str) -> Option<String> {
    if let Some(m) = verdict_line().captures_iter(output).last() {
        return Some(m[1].to_lowercase());
    }
    if let Some(m) = bare_verdict().captures_iter(output).last() {
        return Some(m[1].to_lowercase());
    }
    None
}

#[cfg(test)]
mod tests {
    use super::parse_verdict;

    /// The Go engine's `TestParseVerdict` table, ported case-for-case.
    #[test]
    fn parse_verdict_mirrors_the_go_table() {
        let cases: &[(&str, &str, Option<&str>)] = &[
            ("plain", "looks fine\n\nVERDICT: LGTM", Some("lgtm")),
            ("bare LBTM", "the loop guard is wrong\nLBTM", Some("lbtm")),
            ("pass/fail", "all green\nVERDICT: PASS", Some("pass")),
            ("case folded", "VERDICT: Revise", Some("revise")),
            ("markdown quoted", "> VERDICT: ACCEPT", Some("accept")),
            ("bullet", "- VERDICT: FAIL", Some("fail")),
            (
                "last one wins (prompt echo first)",
                "End with `VERDICT: ACCEPT` or `VERDICT: REVISE`.\n...analysis...\nVERDICT: REVISE",
                Some("revise"),
            ),
            ("none", "just prose with no decision", None),
            (
                "word in prose is not a verdict",
                "we should remember the LGTM convention",
                None,
            ),
            (
                "inline mention is not a verdict line",
                "the token LGTM appears here mid-sentence",
                None,
            ),
            (
                "bare token with indentation",
                "analysis\n   LGTM",
                Some("lgtm"),
            ),
        ];
        for (name, out, want) in cases {
            assert_eq!(parse_verdict(out).as_deref(), *want, "case {name}: {out:?}");
        }
    }
}
