# Ticket-propagation tools for Cursor IDE

Three tiny user-local tools that close the per-incident telemetry gap for Cursor IDE traffic to the Panacea AI Gateway and let engineers self-serve the per-incident "MCP ledger":

| Tool | What it does |
|---|---|
| `panacea-ticket` | Shell helper that writes the active triage ticket to `~/.cursor/active-ticket`. |
| `panacea-baggage-shim` | Local Python reverse proxy that reads the active ticket and rewrites the W3C `baggage` header on outbound MCP calls before forwarding to the AI gateway. |
| `panacea-ledger` | Bash CLI that wraps the Langfuse Public API to print every MCP/LLM call tagged with `ticket:<ID>`. The "ledger" any engineer can run during/after a triage. |

Why a shim? Cursor IDE's `~/.cursor/mcp.json` is loaded once at startup and cannot carry per-session ticket=. The shim is a standard sidecar interception point — same shape as Datadog tag injectors, OTel Collector `transform` processors, and Envoy sidecars. See [`panacea-ai-gateway/docs/ticket-propagation-contract.md`](../../docs/ticket-propagation-contract.md) for the full contract this implements.

## Install

```bash
cd panacea-ai-gateway/tools/ticket-propagation
mkdir -p ~/.local/bin
ln -sfn "$PWD/panacea-ticket"        ~/.local/bin/panacea-ticket
ln -sfn "$PWD/panacea-baggage-shim"  ~/.local/bin/panacea-baggage-shim
ln -sfn "$PWD/panacea-ledger"        ~/.local/bin/panacea-ledger
chmod +x panacea-ticket panacea-baggage-shim panacea-ledger
```

Make sure `~/.local/bin` is on your `PATH`.

For the shim to start automatically at login, install the OS-specific unit:

- macOS: `cp com.panacea.baggage-shim.plist ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/com.panacea.baggage-shim.plist`
- Linux (systemd-user): `mkdir -p ~/.config/systemd/user && cp panacea-baggage-shim.service ~/.config/systemd/user/ && systemctl --user enable --now panacea-baggage-shim`

## Use

```bash
panacea-ticket set ENG-918015        # before triaging
panacea-ticket show                  # confirm: prints "ENG-918015"
# ... do RCA work in Cursor IDE; every MCP call gets ticket=ENG-918015 baggage ...
panacea-ticket unset                 # when done
```

Wire `~/.cursor/mcp.json` so gateway-routed MCP entries point at the shim port (default `13017`) instead of the gateway directly. The default OS units (`com.panacea.baggage-shim.plist`, `panacea-baggage-shim.service`) ship two path-prefix routes:

- `/cf-stack/*` → `http://10.113.24.33:6980` (cf-stack gateway, port 6980)
- `/cf-stack-eval/*` → `http://10.113.24.33:6981` (cf-stack-eval gateway, port 6981)

The first path segment is consumed by the shim and the remainder forwarded. So `gw-sourcegraph` becomes `http://127.0.0.1:13017/cf-stack/mcp/sourcegraph` (originally `http://10.113.24.33:6980/mcp/sourcegraph`); `gw-eval-sourcegraph` becomes `http://127.0.0.1:13017/cf-stack-eval/mcp/sourcegraph`. Override via the `PANACEA_AIGW_URL` (default upstream) and `PANACEA_AIGW_ROUTES` (`prefix=url,...`) env vars in the launchd / systemd unit. Static `user=` / `role=` baggage in `mcp.json` is preserved; the shim only adds/refreshes `ticket=`.

## Verify

After setting an active ticket and making an MCP call, query Langfuse via the shipped CLI:

```bash
export LANGFUSE_PUBLIC_KEY=pk-lf-...
export LANGFUSE_SECRET_KEY=sk-lf-...
panacea-ledger ENG-918015              # human-readable ledger
panacea-ledger ENG-918015 --count      # just the count
panacea-ledger ENG-918015 --json | jq  # full payload, pipeable
```

If you prefer a one-liner, the same query is just a Public API call:

```bash
curl -s -u "$LANGFUSE_PUBLIC_KEY:$LANGFUSE_SECRET_KEY" \
  "http://10.48.65.201:4000/api/public/observations?tags=ticket:ENG-918015&limit=200" \
  | jq '.data[] | {name, startTime, latency, tags}'
```

The full end-to-end recipe and the manual UI runbook for the pinned saved view + dashboard tile live in [`panacea-ai-gateway/docs/ticket-propagation-contract.md`](../../docs/ticket-propagation-contract.md) (§4).

## Files

- `panacea-ticket` — bash helper (set/unset/show)
- `panacea-baggage-shim` — Python 3 reverse proxy (no third-party deps; uses `http.server` and `urllib`)
- `panacea-ledger` — bash CLI for querying the per-ticket ledger from Langfuse
- `com.panacea.baggage-shim.plist` — macOS launchd unit
- `panacea-baggage-shim.service` — Linux systemd-user unit
