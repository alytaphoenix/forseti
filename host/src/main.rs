// Forseti host — Phase 18A
//
// Embeds the switchyard routing server *in this process* instead of spawning
// `switchyard-server` as a child. Verified against switchyard-server 0.3.0:
// the crate ships a library (`[lib] name = "switchyard_server"`) exposing
//   - config::load_server_state(path) -> ServerState
//   - ServerState::{from_runner, with_routing_log, models, caller_auth_kind}
//   - BoundServer::bind(state, ServerRunOptions) -> BoundServer
//        .local_addr()            (OS-assigned port when addr port == 0)
#![allow(unused)] // 18A scaffold: TUI/runner port lands on top of this.
//     .serve(shutdown)         (async, drains on shutdown)
//   ServerRunOptions { addr, backlog, dry_run, shutdown_timeout, tls }
//
// We do NOT reimplement routing: the algorithm config is the same TOML the
// standalone server takes, and we hand pi the loopback port we bound.

use std::net::SocketAddr;
use anyhow::Result;
use clap::Parser;
use switchyard_server::config::load_server_state;
use switchyard_server::{BoundServer, ServerRunOptions};

#[derive(Parser, Debug)]
#[command(
    name = "forseti-host",
    version,
    about = "Forseti host — runs the switchyard router in-process"
)]
struct Args {
    /// Routes TOML (the same schema `switchyard-server --config` accepts).
    #[arg(long)]
    config: std::path::PathBuf,

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

fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| anyhow::anyhow!("tokio runtime: {e}"))?;
    rt.block_on(run(args))
}

async fn run(args: Args) -> anyhow::Result<()> {
    let state = load_server_state(&args.config)
        .map_err(|e| anyhow::anyhow!("load {}: {e}", args.config.display()))?;

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

    let bound = BoundServer::bind(state, opts)
        .map_err(|e| anyhow::anyhow!("bind {}: {e}", args.addr))?;
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
