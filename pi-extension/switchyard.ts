// Forseti pi extension — switchyard routing headers (Phase 18A)
//
// Sends switchyard's explicit correlation headers so the router can attribute
// traffic per session/agent. Facts verified against switchyard 0.3.0 source
// (docs/spikes.md S28):
//   - switchyard-protocol/src/metadata.rs HEADER_CONFIG: the explicit
//     `x-switchyard-*` headers are FIRST in each precedence chain; pi's native
//     `x-session-id` is only a later fallback, so we send the explicit override.
//   - switchyard-server/src/routing_log.rs also reads the legacy
//     `proxy_x_session_id` (0.2.0 name) — we send both for compatibility.
//   - `x-switchyard-origin` labels the client in the routing log.
//   - responses carry `x-model-router-selected-model` (the target that served).
//
// Hook contract (extensions/types.d.ts:680): `before_provider_headers` handlers
// MUTATE `event.headers` in place (Record<string, string | null>; null deletes).
//
// Dev:  pi -a -e ./pi-extension/switchyard.ts --model switchyard/<route> -p "..."
// Install: cp pi-extension/switchyard.ts ~/.pi/agent/extensions/

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { randomUUID } from "node:crypto";

// Only decorate traffic that goes through the crew's switchyard provider.
const PROVIDER = process.env.FORSETI_SWITCHYARD_PROVIDER ?? "switchyard";

type LastRoute = { status: number; served?: string; at: number };
let lastRoute: LastRoute | undefined;
let injected = 0;

function headerCI(headers: Record<string, string>, name: string): string | undefined {
	const want = name.toLowerCase();
	for (const [k, v] of Object.entries(headers)) {
		if (k.toLowerCase() === want) return v;
	}
	return undefined;
}

export default function forsetiSwitchyard(pi: ExtensionAPI): void {
	pi.on("before_provider_headers", (event, ctx) => {
		const model = ctx.model;
		if (!model || model.provider !== PROVIDER) return;

		let sessionId: string | undefined;
		try {
			sessionId = ctx.sessionManager.getSessionId();
		} catch {
			// a session id is desirable, never fatal
		}
		if (sessionId) {
			event.headers["x-switchyard-session-id"] = sessionId; // precedence 1 (S28)
			event.headers["proxy_x_session_id"] = sessionId; // 0.2.0 legacy fallback
		}

		// Optional per-agent identity (e.g. a crew node name), set by whoever
		// launched pi. Per-pane env is unavailable over the herdr socket (S26),
		// so this stays opt-in via the environment.
		const agentId = process.env.FORSETI_SWITCHYARD_AGENT_ID;
		if (agentId) event.headers["x-switchyard-agent-id"] = agentId;

		event.headers["x-switchyard-origin"] = process.env.FORSETI_SWITCHYARD_ORIGIN ?? "pi";
		event.headers["x-switchyard-request-id"] = randomUUID(); // log correlation
		injected++;
	});

	pi.on("after_provider_response", (event, ctx) => {
		const model = ctx.model;
		if (!model || model.provider !== PROVIDER) return;
		lastRoute = {
			status: event.status,
			served: headerCI(event.headers, "x-model-router-selected-model"),
			at: Date.now(),
		};
	});

	pi.registerCommand("switchyard", {
		description: "Show forseti switchyard routing header state",
		handler: async (_args, ctx) => {
			const lines = [
				`provider gate: ${PROVIDER}`,
				`headers sent : x-switchyard-session-id, proxy_x_session_id, x-switchyard-origin, x-switchyard-request-id`,
				`requests     : ${injected}`,
			];
			if (lastRoute) {
				const ago = Math.round((Date.now() - lastRoute.at) / 1000);
				lines.push(`last status  : ${lastRoute.status} (${ago}s ago)`);
				lines.push(`served by    : ${lastRoute.served ?? "unknown"}`);
			} else {
				lines.push("last status  : no switchyard request yet");
			}
			ctx.ui.notify(lines.join("\n"), "info");
		},
	});
}
