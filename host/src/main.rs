// Forseti host — Phase 18A (in-process router) + Phase 18D (crew engine)
//
// Phase 18A: embeds the switchyard routing server *in this process* instead of
// spawning `switchyard-server` as a child. Verified against switchyard-server
// 0.3.0: the crate ships a library exposing
//   - config::load_server_state(path) -> ServerState
//   - ServerState::{from_runner, with_routing_log, models, caller_auth_kind}
//   - BoundServer::bind(state, ServerRunOptions) -> BoundServer
//        .local_addr()            (OS-assigned port when addr port == 0)
//        .serve(shutdown)         (async, drains on shutdown)
//   - ServerRunOptions { addr, backlog, dry_run, shutdown_timeout, tls }
// We do NOT reimplement routing: the algorithm config is the same TOML the
// standalone server takes, and we hand pi the loopback port we bound.
//
// Phase 18D: the `crew` subcommand validates crew.yaml with the Rust crew model
// (`forseti_host::crew`), pinned to the Go engine by the oracle goldens. The
// router flags keep working unchanged, so the Go crew runner keeps spawning
// this binary as its router.
#![allow(unused)] // the router path keeps its own scaffolding.

use anyhow::Result;
use clap::{Parser, Subcommand};
use std::net::SocketAddr;
use switchyard_server::config::load_server_state;
use switchyard_server::{BoundServer, ServerRunOptions};

#[derive(Parser, Debug)]
#[command(
    name = "forseti-host",
    version,
    about = "Forseti host — switchyard router in-process, plus the crew engine (Phase 18D)"
)]
struct Args {
    #[command(subcommand)]
    cmd: Option<Cmd>,

    /// Routes TOML (the same schema `switchyard-server --config` accepts).
    #[arg(long)]
    config: Option<std::path::PathBuf>,

    /// Bind address. Port 0 = let the OS choose (we print the real port).
    #[arg(long, default_value = "127.0.0.1:0")]
    addr: std::net::SocketAddr,

    /// Durable per-request routing records (NDJSON), for per-node attribution.
    #[arg(long)]
    routing_log: Option<std::path::PathBuf>,

    /// Validate the config and report the served model ids; bind nothing.
    #[arg(long)]
    dry_run: bool,
}

#[derive(clap::Subcommand, Debug)]
enum Cmd {
    /// Crew commands (Phase 18D). `validate` is the Go-compatible schema gate.
    Crew {
        #[command(subcommand)]
        cmd: CrewCmd,
    },
}

#[derive(clap::Subcommand, Debug)]
enum CrewCmd {
    /// Validate a crew file (the base graph; roles/patterns land in 18D-2).
    Validate {
        /// Crew file.
        #[arg(short = 'f', default_value = "crew.yaml")]
        file: String,

        /// Print the resolved graph (models, nodes, edges, checks, memory).
        #[arg(long)]
        explain: bool,
    },

    /// Run a crew on herdr (Phase 18D-5b: herdr backend only).
    Run {
        /// Crew file.
        #[arg(short = 'f', default_value = "crew.yaml")]
        file: String,

        /// Named herdr session ("" = the default session).
        #[arg(long, default_value = "")]
        session: String,

        /// Crew tab label.
        #[arg(long, default_value = "forseti-crew")]
        tab: String,

        /// Per-node settle timeout, minutes.
        #[arg(long, default_value_t = 10)]
        timeout: u64,

        /// Leave the crew tab open after the run.
        #[arg(long)]
        keep_tab: bool,

        /// herdr workspace to stage the tab in ("" = the focused one).
        #[arg(long, default_value = "")]
        workspace: String,

        /// Substitute the model of every direct-model node (outage override).
        #[arg(long, default_value = "")]
        model: String,

        /// Pane cwd (default: the current directory).
        #[arg(long, default_value = "")]
        cwd: String,

        /// Run inside a disposable worktree on this branch.
        #[arg(long, default_value = "")]
        worktree: String,

        /// Keep the worktree (and its branch) after the run.
        #[arg(long)]
        keep_worktree: bool,

        /// End the run on a lazygit review pane of the worktree.
        #[arg(long)]
        review: bool,

        /// Where agents run: herdr (default) or ttt.
        #[arg(long, default_value = "")]
        host: String,

        /// Stream every run event as a JSON line on stdout.
        #[arg(long)]
        headless: bool,
    },
}

fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    if let Some(Cmd::Crew { cmd }) = &args.cmd {
        match cmd {
            CrewCmd::Validate { file, explain } => {
                let out = forseti_host::crew::validate(file, *explain);
                use std::io::Write;
                print!("{}", out.stdout);
                eprint!("{}", out.stderr);
                let _ = std::io::stdout().flush();
                let _ = std::io::stderr().flush();
                std::process::exit(out.exit);
            }
            CrewCmd::Run {
                file,
                session,
                tab,
                timeout,
                keep_tab,
                workspace,
                model,
                cwd,
                worktree,
                keep_worktree,
                review,
                host,
                headless,
            } => {
                if *timeout < 1 {
                    eprintln!("run: --timeout must be >= 1 (minutes)");
                    std::process::exit(2);
                }
                let text = match std::fs::read_to_string(file) {
                    Ok(t) => t,
                    Err(e) => {
                        let detail = match e.kind() {
                            std::io::ErrorKind::NotFound => "no such file or directory".to_string(),
                            _ => e.to_string(),
                        };
                        eprintln!("cannot read: open {file}: {detail}");
                        std::process::exit(1);
                    }
                };
                let crew = match forseti_host::crew::load(&text) {
                    Ok(c) => c,
                    Err(e) => {
                        eprintln!("INVALID: {e}");
                        std::process::exit(1);
                    }
                };
                let cwd = if cwd.is_empty() {
                    std::env::current_dir()
                        .map(|p| p.to_string_lossy().into_owned())
                        .unwrap_or_default()
                } else {
                    cwd.clone()
                };
                let opts = forseti_host::crew::Options {
                    cwd,
                    tab_label: tab.clone(),
                    node_timeout: std::time::Duration::from_secs(timeout * 60),
                    keep_tab: *keep_tab,
                    session: session.clone(),
                    workspace_id: workspace.clone(),
                    model_override: model.clone(),
                    headless: *headless,
                    worktree_branch: worktree.clone(),
                    keep_worktree: *keep_worktree,
                    review: *review,
                    host: host.clone(),
                };
                let mut run = forseti_host::crew::Run::new(crew, opts);
                match run.run() {
                    Ok(out) => {
                        if !*headless {
                            println!("{}", out.summary);
                        }
                        eprintln!("run log: {}", out.log_path.display());
                        if out.failed || out.blocked || out.checks_failed {
                            std::process::exit(1);
                        }
                        std::process::exit(0);
                    }
                    Err(e) => {
                        eprintln!("run error: {e}");
                        std::process::exit(1);
                    }
                }
            }
        }
    }

    let Some(config) = args.config.clone() else {
        eprintln!("forseti-host: --config is required (or use `crew validate -f FILE`)");
        std::process::exit(2);
    };

    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| anyhow::anyhow!("tokio runtime: {e}"))?;
    rt.block_on(run(&args, &config))
}

async fn run(args: &Args, config: &std::path::Path) -> anyhow::Result<()> {
    let state =
        load_server_state(config).map_err(|e| anyhow::anyhow!("load {}: {e}", config.display()))?;

    let state = match &args.routing_log {
        Some(path) => state
            .with_routing_log(path)
            .map_err(|e| anyhow::anyhow!("routing log {}: {e}", path.display()))?,
        None => state,
    };

    let models: Vec<String> = state.models().map(str::to_owned).collect();

    if args.dry_run {
        println!("ok models={}", models.join(","));
        return Ok(());
    }

    let opts = ServerRunOptions {
        addr: args.addr,
        backlog: 1024,
        dry_run: false,
        shutdown_timeout: std::time::Duration::from_secs(10),
        tls: None,
    };

    let bound =
        BoundServer::bind(state, opts).map_err(|e| anyhow::anyhow!("bind {}: {e}", args.addr))?;
    let addr = bound.local_addr();

    // Machine-readable handshake: the supervisor (crew runner) reads this line
    // to learn the OS-assigned port, then points pi at it.
    println!("FORSETI_HOST_ADDR={addr}");
    println!("FORSETI_HOST_MODELS={}", models.join(","));
    println!("FORSETI_HOST_READY=1");
    use std::io::Write;
    let _ = std::io::stdout().flush();

    bound
        .serve(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await
        .map_err(|e| anyhow::anyhow!("serve: {e}"))?;
    Ok(())
}
