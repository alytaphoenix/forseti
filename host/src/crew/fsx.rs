//! Private (0600) filesystem writes for run artifacts.
//!
//! Run logs, bus files, the crew-status bridge and the memory hand-off all
//! contain prompts, model output, or node names. They are created owner-only so
//! a multi-user box does not leak a run's transcript.

use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::Path;

/// Creates/truncates `path`, mode 0600 (existing files keep their mode).
pub fn create_private(path: &Path) -> io::Result<File> {
    let mut opts = OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let file = opts.open(path)?;
    // an EXISTING file keeps its old mode with O_CREAT: force 0600 anyway so a
    // fixed-name artifact (bus/<node>.md, crew-status.json) does not stay
    // world-readable from an older release
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = file.set_permissions(std::fs::Permissions::from_mode(0o600));
    }
    Ok(file)
}

/// Writes `bytes` to `path` as a private (0600) file.
pub fn write_private(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let mut f = create_private(path)?;
    f.write_all(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn artifacts_are_owner_only() {
        let dir = std::env::temp_dir().join(format!("forseti-fsx-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("run.jsonl");
        write_private(&path, b"{}\n").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o600, "mode {mode:o}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }
}
