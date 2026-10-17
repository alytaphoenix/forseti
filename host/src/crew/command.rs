//! The `forseti-host crew …` command surface (Phase 18D-1): the same contract
//! the Go `forseti-crew` CLI exposes, so a run can be compared in-process.
//!
//! Output is split into stdout/stderr/exit so tests can diff it against the
//! oracle goldens (`host/tests/golden/`) without spawning a process.

use std::io::ErrorKind;

pub struct Outcome {
    pub stdout: String,
    pub stderr: String,
    pub exit: i32,
}

impl Outcome {
    fn ok(stdout: String) -> Self {
        Self {
            stdout,
            stderr: String::new(),
            exit: 0,
        }
    }

    fn invalid(err: String) -> Self {
        Self {
            stdout: String::new(),
            stderr: format!("INVALID: {err}\n"),
            exit: 1,
        }
    }

    fn read_error(file: &str, e: &std::io::Error) -> Self {
        // Go: `cannot read: open FILE: no such file or directory`. Matching the
        // common case keeps the two CLIs interchangeable in scripts.
        let detail = match e.kind() {
            ErrorKind::NotFound => "no such file or directory".to_string(),
            ErrorKind::PermissionDenied => "permission denied".to_string(),
            ErrorKind::IsADirectory => "is a directory".to_string(),
            _ => e.to_string(),
        };
        Self {
            stdout: String::new(),
            stderr: format!("cannot read: open {file}: {detail}\n"),
            exit: 1,
        }
    }
}

/// `validate -f FILE [--explain]`, identical to `forseti-crew validate`.
pub fn validate(file: &str, explain: bool) -> Outcome {
    let text = match std::fs::read_to_string(file) {
        Ok(t) => t,
        Err(e) => return Outcome::read_error(file, &e),
    };
    match crate::crew::load(&text) {
        Ok(crew) => {
            let mut out = String::new();
            if explain {
                out.push_str(&crate::crew::explain(&crew));
            }
            out.push_str(&format!("{file} valid\n"));
            Outcome::ok(out)
        }
        Err(e) => Outcome::invalid(e),
    }
}

#[cfg(test)]
mod tests {
    use super::validate;

    #[test]
    fn missing_file_message_matches_the_go_cli() {
        let out = crate::crew::validate("crew/does-not-exist.yaml", true);
        assert_eq!(out.exit, 1);
        assert!(out.stdout.is_empty());
        assert_eq!(
            out.stderr,
            "cannot read: open crew/does-not-exist.yaml: no such file or directory\n"
        );
    }

    #[test]
    fn valid_output_is_the_explain_then_the_valid_line() {
        std::env::set_current_dir(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .parent()
                .unwrap(),
        )
        .expect("chdir repo root");
        let out = validate("crew/examples/crew.yaml", true);
        assert_eq!(out.exit, 0);
        assert!(out.stderr.is_empty());
        assert!(
            out.stdout
                .starts_with("crew demo-planner-builder\nnodes:\n"),
            "{}",
            out.stdout
        );
        assert!(out.stdout.ends_with("crew/examples/crew.yaml valid\n"));

        let quiet = validate("crew/examples/crew.yaml", false);
        assert_eq!(quiet.stdout, "crew/examples/crew.yaml valid\n");
    }
}
