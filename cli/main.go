// forseti — one command to bring up, inspect, and configure the herdr + ttt +
// pi workspace (the forseti IDE).
//
// It wraps the pieces that already exist (the herdr plugin's bring-up action,
// the ttt plugin + pi extension assets, the memory/laya services, and the crew
// engines) behind a single, predictable surface. It never starts a TUI from a
// non-TTY context and never edits plugin permission grants.
package main

import (
	"fmt"
	"os"
	"strings"
)

// Version is the CLI version (kept in step with the repo release).
const Version = "0.10.0"

// defaultRepo is stamped at build time:
//
//	go build -ldflags "-X main.defaultRepo=$PWD" -o ~/.local/bin/forseti .
var defaultRepo string

func main() {
	if len(os.Args) < 2 {
		cmdStatus(nil)
		return
	}
	switch os.Args[1] {
	case "start", "up":
		cmdStart(os.Args[2:])
	case "down":
		cmdDown(os.Args[2:])
	case "status", "st":
		cmdStatus(os.Args[2:])
	case "doctor", "doc":
		cmdDoctor(os.Args[2:])
	case "config", "cfg":
		cmdConfig(os.Args[2:])
	case "open":
		cmdOpen(os.Args[2:])
	case "run":
		cmdCrew("run", os.Args[2:])
	case "watch":
		cmdCrew("watch", os.Args[2:])
	case "validate", "check":
		cmdCrew("validate", os.Args[2:])
	case "flow", "fl":
		if len(os.Args) < 3 {
			cmdFlow("help", nil)
			return
		}
		cmdFlow(os.Args[2], os.Args[3:])
	case "version", "-v", "--version":
		cfg := loadConfig()
		fmt.Printf("forseti %s\nrepo: %s\nconfig: %s\n", Version, repoOrUnknown(cfg), configPath())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "forseti: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `forseti — bring up, inspect, and configure herdr + ttt + pi

usage:
  forseti                         status (the default)
  forseti start [dir] [flags]     bring the whole IDE up (services, herdr, one tab)
  forseti down                    stop the forseti-managed services
  forseti status [--json]         one-screen environment status
  forseti doctor [--json]         preflight checks with fixes (exit 1 on failure)
  forseti open <path[:line[:col]]>  jump the running ttt to a file
  forseti run [crew args...]      crew run (engine/host from config)
  forseti watch [args...]         tail a crew run log
  forseti validate [args...]      validate a crew file
  forseti flow <sub>              validate|explain a per-project workflow
  forseti config <sub>            list|get|set|path|edit|sync|init
  forseti version                 version, repo, config path

forseti up is an alias of forseti start.

start flags:
  --dir DIR        target directory (default: the current directory)
  --label TEXT     tab label (default: herdr.editor_label)
  --no-sync        skip the ttt-plugin / pi-extension / sol-pi sync
  --no-services    do not start memory/laya
  --no-open        do not open (or revive) the IDE tab
  --attach         if herdr is not running, exec herdr (attached) first
  --no-attach      if herdr is not running, fail instead of starting it headless
  --replace        create a fresh IDE tab instead of reviving the owned one
  --clean          also prune dead forseti tabs in other workspaces
  --prune-dead     also close tabs whose saved directory is gone
  --yes            do not ask before closing stale tabs
  --dry-run        print what would happen
  --json           machine-readable summary

config keys:
  repo  herdr.bin  herdr.agent  herdr.editor_label  ttt.bin  ttt.port
  pi.bin  services.memory  services.laya
  crew.engine  crew.host  crew.model  crew.default
  sol_pi.enabled  sol_pi.install  sol_pi.package
  sol_pi.action_fusion  sol_pi.observation_pack  sol_pi.online_context_compact
  sol_pi.evidence_preserving_reducer  sol_pi.reducer_provider
  sol_pi.reducer_model  sol_pi.cache_write_read_ratio

config edit targets:
  forseti  pi-models  pi-settings  pi-mcp  ttt-settings  ttt-plugins
  ttt-registry  crew  tools  sol-pi
`)
}

// repoOrUnknown never fails a version/status print.
func repoOrUnknown(cfg Config) string {
	if r, err := findRepo(cfg); err == nil {
		return r
	}
	return "(not found — set FORSETI_REPO or `forseti config set repo DIR`)"
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "forseti: "+format+"\n", args...)
	os.Exit(1)
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "forseti: "+format+"\n", args...)
}

func trim(s string) string { return strings.TrimSpace(s) }
