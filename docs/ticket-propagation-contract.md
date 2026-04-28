# Ticket Propagation Contract

Single source of truth for how clients of the Panacea AI Gateway propagate per-incident identity, and how the gateway honors / promotes / persists it for query.

**Audience:** anyone writing a new originator (cursor-cli rewrite, Slackbot, web UI, evaluation harness, curl-based scripts). Also anyone tuning gateway promotion rules or building Langfuse dashboards.

**TL;DR:** The originator mints `ticket=<incident-id>` once at workflow start and propagates it via the W3C `baggage` header on every outbound MCP/LLM call. The gateway promotes `ticket` to `langfuse.tags=["ticket:<id>"]`. The Langfuse trace store is the canonical "MCP ledger" — `tags has ticket:<id>` returns every span belonging to that incident.

---

## 1. Responsibility split

Distributed tracing is a context-propagation problem, not a server-side inference problem. Following [W3C Trace Context](https://www.w3.org/TR/trace-context/), [W3C Baggage](https://www.w3.org/TR/baggage/), and [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/), responsibilities are split as follows:

| Responsibility | Owner | Mechanism |
|---|---|---|
| Mint `ticket=<id>` once at workflow start | Client / originator | Set OTel baggage in process context |
| Propagate on every outbound HTTP call | Client (auto via OTel) | `baggage:` request header, W3C-format |
| Validate, sanitize, allow-list keys | AI gateway | See §3 below |
| Promote to span attributes | AI gateway | See §3 below |
| Index + persist for query | Langfuse | Tag-indexed search on `langfuse.tags` |
| Retrieval / "ledger" UX | Langfuse UI / Public API | See §5 below |

The gateway **cannot** tag what the client does not send. Designs that try to derive `ticket` server-side from URL patterns, tool names, IP addresses, or timing are fundamentally broken — that context only exists at the originator.

---

## 2. Originator obligations (the client contract)

### 2.1 Baggage schema

Send a single W3C [`baggage`](https://www.w3.org/TR/baggage/) header on every outbound HTTP request to the gateway:

```
baggage: user=<email>,role=<role>,ticket=<incident-id>,session=<opaque-id>
```

| Key | Required | Recommended shape | Purpose |
|---|---|---|---|
| `user` | yes | RFC 5322 email (e.g. `mohit@nutanix.com`) | Promoted to Langfuse `userId`; powers per-user filtering & rate limits |
| `role` | yes | One of `engineer`, `cursor-cli-agent`, `slackbot`, `web-ui`, `eval` | Distinguishes human vs automated callers; appears as `role:<x>` tag |
| `ticket` | yes (when applicable) | `^[A-Z][A-Z0-9_]+-\d+$` (e.g. `ENG-918015`, `ONCALL-22386`, `TH-19173`) | Per-incident stitching key; appears as `ticket:<x>` tag |
| `session` | no | Opaque short ID, e.g. UUID or `<job_id>` | Filters one chat session inside one ticket |
| `log_bundle_id` | no | Numeric string | Ties the call to a Panacea log bundle |

Other arbitrary baggage keys propagate downstream unchanged but are NOT promoted to span attributes (see §3.2 for how to opt them in).

### 2.2 How to actually set baggage (don't roll your own)

Use OpenTelemetry context propagation. Set baggage **once** at the workflow boundary and let auto-instrumentation handle every outbound call.

**Python (FastAPI / asyncio originator):**

```python
from opentelemetry import baggage, context

# At workflow start (e.g. inside POST /agent/rca handler)
ctx = context.get_current()
ctx = baggage.set_baggage("user", request.user_email, ctx)
ctx = baggage.set_baggage("role", "cursor-cli-agent", ctx)
ctx = baggage.set_baggage("ticket", request.incident_id, ctx)
ctx = baggage.set_baggage("session", job_id, ctx)
token = context.attach(ctx)
try:
    # All outbound HTTP calls inside this block carry the baggage header
    # automatically when opentelemetry-instrumentation-httpx / requests /
    # aiohttp is installed.
    await do_the_work()
finally:
    context.detach(token)
```

**Go (cursor-cli rewrite, gateway sidecar):**

```go
import "go.opentelemetry.io/otel/baggage"

m1, _ := baggage.NewMember("user", userEmail)
m2, _ := baggage.NewMember("role", "cursor-cli-agent")
m3, _ := baggage.NewMember("ticket", incidentID)
b, _ := baggage.New(m1, m2, m3)
ctx = baggage.ContextWithBaggage(ctx, b)
// otelhttp.NewTransport injects baggage on every outbound request from this ctx
```

**curl (smoke tests, ad-hoc scripts):**

```bash
curl -H "baggage: user=mohit@nutanix.com,role=engineer,ticket=ENG-918015" \
     https://aigw.example/diamond/mcp -d @payload.json
```

### 2.3 Anti-patterns

Do not do these. They break correlation, leak PII, blow up cardinality, or all three:

- **Generating `ticket` server-side** from URL patterns, tool names, or timing windows. The gateway has no business context — only the originator does.
- **Stuffing free-form text** into `ticket` (titles, descriptions, log lines). Keep it to the canonical ticket ID.
- **Putting secrets** (API keys, tokens, passwords) into baggage. W3C explicitly defines baggage as user-visible metadata that propagates to downstream services unchanged.
- **Reusing `trace_id` as the workflow ID.** `trace_id` is per-call-chain (one inbound → fan-out); `ticket=` is per-business-workflow (N independent inbound calls that share a triage). The Langfuse `session` dimension stitches calls within one trace; `tags` stitches across many traces.
- **Adding `ticket` as a Prometheus label.** Cardinality explodes. The gateway intentionally does NOT emit `ticket` as a Prom label. Use trace store filters instead.
- **Hand-stitching headers per call.** Set baggage in OTel context once at workflow start; the SDK handles propagation. Manual per-call header setting always misses some edge case (background tasks, retries, fan-out).
- **Letting intermediaries override `ticket=`.** Once the originator sets it, the value should be copied through, not rewritten.

---

## 3. Gateway behavior (what the AI gateway actually does)

Source of truth: [`internal/tracing/mcp.go`](../internal/tracing/mcp.go). This section documents the live behavior; if it diverges from the code, the code wins and this doc is the bug.

### 3.1 Allow-list of promoted keys

Default: only `ticket`, `user`, `role` are promoted to span attributes. Other baggage keys propagate to downstream services on the wire but are not surfaced on the gateway span. See [`defaultBaggageSpanAttributes`](../internal/tracing/mcp.go#L68).

Operators can override the allow-list via env var:

```bash
# Promote additional keys (the gateway will surface them as `mcp.client.<key>`)
MCP_TRACE_BAGGAGE_ATTRS=ticket,user,role,session,log_bundle_id
```

Setting it to the empty string disables promotion entirely (baggage still propagates downstream).

### 3.2 Per-value validation rules

| Rule | Value | Source |
|---|---|---|
| Per-baggage-value cap | 128 bytes (UTF-8 boundary preserved, suffix `...`) | [`maxBaggageAttrValueBytes`](../internal/tracing/mcp.go#L90) |
| Empty values skipped | yes | [`StartSpanAndInjectMeta`](../internal/tracing/mcp.go#L362) |
| Unknown keys | propagated, not promoted | (see allow-list above) |
| Total `baggage` header size | ≤ 8 KB (W3C-recommended; gateway does not enforce a tighter cap) | W3C spec |
| Fail-open on missing values | yes — request is not rejected; `auth.kind` falls back to `ip` / `anonymous` | [`StartSpanAndInjectMeta`](../internal/tracing/mcp.go#L411) |

The gateway intentionally does **not** regex-validate the `ticket=` value today: clients are trusted to follow the recommended shape. If you need strict validation, set `MCP_BAGGAGE_TICKET_REGEX` (TBD env knob — see §6 for the open work).

### 3.3 Promotion to span attributes

| Baggage key | OTel span attribute | Langfuse-native dimension |
|---|---|---|
| `user` | `mcp.client.user`, `user.id` | `userId` |
| `ticket` | `mcp.client.ticket` | `tags` entry `ticket:<value>` |
| `role` | `mcp.client.role` | `tags` entry `role:<value>` |
| (other allow-listed) | `mcp.client.<key>` | not promoted |
| (gateway-added) | `mcp.backend.name` | `tags` entry `backend:<name>` |
| (always) | `auth.kind` ∈ {`baggage`, `ip`, `anonymous`} | not promoted |

This means a single Langfuse query `tags has ticket:ENG-918015` returns every span the gateway emitted while handling baggage for that ticket, regardless of which backend (`diamond`, `sourcegraph`, `nurag`, …) was hit.

### 3.4 Trace context

The gateway also extracts `traceparent` / `tracestate` from inbound HTTP and from the MCP `_meta` map, so cross-hop trace continuity is preserved. Originators that already use OTel auto-instrumentation get this for free.

---

## 4. The MCP ledger is the Langfuse trace store

There is no separate ledger to build. Langfuse already records every MCP call as a span/observation; baggage is the join key.

```
Originator  --baggage: ticket=ENG-918015-->  AI Gateway  --OTLP-->  Langfuse
                                                                       |
                              "give me everything for ENG-918015"  <---+
                                          (filter: tags has ticket:ENG-918015)
```

### 4.1 Self-serve UI query

1. Open Langfuse: `http://10.48.65.201:4000/project/cmmtgs957001tlb07m0t95u6a/traces`
2. Filter: `Tags` → `contains` → `ticket:ENG-918015`
3. Sort by `Start time` ascending → that's your ledger.

A pinned saved view exists at the project sidebar (search "Per-ticket ledger" — see §4.4 for setup if missing).

### 4.2 Public API snippet

For scripts, CI, notebooks, or dashboards that need programmatic access:

```bash
LANGFUSE_HOST=http://10.48.65.201:4000
LANGFUSE_PUB=...    # public key from project settings
LANGFUSE_SEC=...    # secret key from project settings
TICKET=ENG-918015

curl -s -u "$LANGFUSE_PUB:$LANGFUSE_SEC" \
  "$LANGFUSE_HOST/api/public/traces?tags=ticket:$TICKET&limit=200" \
  | jq '.data[] | {name, timestamp, latency, tags, metadata}'
```

For span-level (every individual MCP / LLM call, not just trace headers):

```bash
curl -s -u "$LANGFUSE_PUB:$LANGFUSE_SEC" \
  "$LANGFUSE_HOST/api/public/observations?tags=ticket:$TICKET&limit=500" \
  | jq '.data[] | {name, type, startTime, endTime, input, output}'
```

The host-VM CLI also exists at `~/aigw-otel-langfuse-backup/find-mcps-for-ticket.py` (see [telemetry-rollout-tracker.md](telemetry-rollout-tracker.md)) and the repo-shipped CLI at [`tools/ticket-propagation/panacea-ledger`](../tools/ticket-propagation/panacea-ledger). Use the latter if you want the CLI versioned with the contract.

```bash
# repo-shipped CLI: human-readable summary
export LANGFUSE_PUBLIC_KEY=pk-lf-...
export LANGFUSE_SECRET_KEY=sk-lf-...
panacea-ledger ENG-918015                # pretty-print every span
panacea-ledger ENG-918015 --count        # just the number
panacea-ledger ENG-918015 --json | jq    # full payload, pipeable
```

### 4.3 Dashboard

The AIGW Langfuse dashboard ([`cmohddu5o0009lb07k127lwo6`](http://10.48.65.201:4000/project/cmmtgs957001tlb07m0t95u6a/dashboards/cmohddu5o0009lb07k127lwo6)) groups by `langfuse.tags`. Once originators emit `ticket:<id>`, the existing widgets segment by ticket without any dashboard change. A "Tool calls per ticket (last 24h)" tile is queued — see §4.4.

### 4.4 Ledger setup runbook (manual, one-time per project)

Langfuse `Saved Views` and `Dashboard widgets` are project-scoped UI artifacts; they cannot be created via the Public API today (Langfuse OSS as of `v3.x` exposes traces / observations / metrics / sessions / scores via `/api/public/*`, but neither saved-view CRUD nor widget CRUD). The setup is therefore a **one-time manual UI step** per project, which is acceptable: once pinned, the saved view is shared with everyone in the project.

#### 4.4.1 Pin a "Per-ticket ledger" saved view

1. Open `http://10.48.65.201:4000/project/cmmtgs957001tlb07m0t95u6a/traces`.
2. In the left **Filters** rail, expand **Tags**. Wait until at least one trace has been emitted with a `ticket:<id>` tag (run a triage with `panacea-ticket set TEST-1` first if needed). Tag suggestions only appear once the project has data.
3. Pick one `ticket:*` value to seed the filter (any one is fine — engineers will retype the actual ticket later).
4. Click the **Saved Views** button in the toolbar → **Save current view as…**
5. Name it `Per-ticket ledger`, set it as the default for the project if appropriate, **Save**.
6. From now on, click `Saved Views → Per-ticket ledger`, then in the Tags chip replace the seed ticket with the one you're triaging.

#### 4.4.2 Add a "Tool calls per ticket (last 24h)" dashboard tile

1. Open the AIGW dashboard: `http://10.48.65.201:4000/project/cmmtgs957001tlb07m0t95u6a/dashboards/cmohddu5o0009lb07k127lwo6`.
2. Click **Add widget**.
3. Configure:
   - **View**: `Observations`
   - **Metric**: `count`
   - **Group by**: `tags`
   - **Filter**: `tags` `contains` `ticket:`
   - **Time range**: `Last 24 hours`
   - **Visualization**: bar chart (top 20)
4. Title: `Tool calls per ticket (last 24h)`. Save.

#### 4.4.3 Programmatic alternative (per-team scripts)

If you do not want to depend on the project-level saved view, the same query is one Public API call. Drop this snippet into your runbook / Slack bot / CI step — every engineer can run it with their own keys without touching the Langfuse UI:

```python
# tools/ticket-propagation/ledger.py — minimal reference impl
import os, sys, requests
host = os.environ.get("LANGFUSE_HOST", "http://10.48.65.201:4000")
auth = (os.environ["LANGFUSE_PUBLIC_KEY"], os.environ["LANGFUSE_SECRET_KEY"])
ticket = sys.argv[1]
r = requests.get(f"{host}/api/public/observations",
                 params={"tags": f"ticket:{ticket}", "limit": 500},
                 auth=auth, timeout=30)
r.raise_for_status()
for obs in r.json()["data"]:
    print(obs["startTime"], obs["name"], obs.get("latency", "-"), obs.get("tags", []))
```

The bash equivalent is the repo-shipped `panacea-ledger` CLI (§4.2).

---

### 4.5 Self-serve verification recipe (no Langfuse keys required)

A two-step recipe an engineer can run on their own machine, in order, to confirm the propagation chain is healthy from their Cursor IDE all the way to Langfuse.

**Step 1 — hermetic shim test (no network, no creds).** Confirms the shim correctly injects `ticket=` and preserves static baggage:

```bash
panacea-ai-gateway/tools/ticket-propagation/verify-baggage-shim
# Expected:
#   upstream saw baggage: user=mohit%40nutanix.com,role=engineer,ticket=VERIFY-1
#   ✓ shim correctly injected ticket=VERIFY-1, preserved user= and role=
#   ✓ path forwarded unchanged: /mcp/diamond
```

The script atomically swaps `~/.cursor/active-ticket`, spawns its own capture upstream, runs the shim against it, asserts the forwarded `baggage` header, and restores any prior active ticket. Safe to run during an active triage.

**Step 2 — live shim → cf-stack gateway smoke (network, no Langfuse creds).** Confirms the full data path including the running shim, your launchd / systemd unit, and the gateway:

```bash
panacea-ticket set E2E-VERIFY-1
curl -sS -i -m 8 \
  -H "content-type: application/json" \
  -H "accept: application/json, text/event-stream" \
  -H "baggage: user=$(whoami)@nutanix.com,role=engineer" \
  -X POST "http://127.0.0.1:13017/cf-stack/mcp/sourcegraph" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"verify","version":"0"}}}'
panacea-ticket unset
# Expected: HTTP/1.1 200, mcp-session-id header, JSON body with serverInfo.name=envoy-ai-gateway.
```

A 200 here means the running shim, the AI gateway, and the upstream MCP server all accepted the request including the baggage header. (Validation does not reject malformed baggage; it would only show up missing in Step 3.)

**Step 3 — Langfuse-side promotion (Langfuse keys required, recommended for the project owner).** Run after Step 2:

```bash
export LANGFUSE_PUBLIC_KEY=pk-lf-...
export LANGFUSE_SECRET_KEY=sk-lf-...
panacea-ledger E2E-VERIFY-1 --json | jq '.data | length, .data[0].tags'
# Expected: > 0 spans returned, with tags including "ticket:E2E-VERIFY-1" and "role:engineer".
```

If Step 1 + 2 pass but Step 3 returns zero, look at the gateway side: confirm `MCP_TRACE_BAGGAGE_ATTRS` is unset or includes `ticket`, and that the OTLP exporter is healthy ([`telemetry-rollout-tracker.md`](telemetry-rollout-tracker.md)).

---

## 5. Worked example — one RCA, twenty MCP calls, one filter

A triage agent receives `POST /agent/rca {incident_id: "ENG-918015"}`.

1. The handler sets baggage `user=alice@nutanix.com,role=cursor-cli-agent,ticket=ENG-918015,session=<job_id>` once at the start.
2. Over the next 5 minutes the agent makes 20 MCP calls across `diamond`, `sourcegraph`, `nurag`, `atlassian`. OTel auto-instrumentation copies the baggage header onto each outbound HTTP request automatically.
3. The gateway sees baggage on each, validates, and emits one OTLP span per call with `langfuse.tags=["ticket:ENG-918015", "role:cursor-cli-agent", "backend:<x>"]`.
4. An on-call engineer 30 minutes later runs:
   ```
   curl ... /api/public/observations?tags=ticket:ENG-918015&limit=500
   ```
   …and gets all 20 MCP calls back with timestamps, inputs, outputs, latencies. That is the MCP ledger.

No new storage. No new schema. No new service.

---

## 6. Open work / non-goals

| Item | Status | Why |
|---|---|---|
| Optional gateway env knob `MCP_BAGGAGE_TICKET_REGEX` for strict validation | TBD | Today the gateway is permissive (any non-empty value ≤ 128 bytes). A regex is straightforward to add but breaks any client that uses non-canonical IDs; defer until we have a real misuse case. |
| Pinned "Per-ticket ledger" saved view in Langfuse | Manual UI step — runbook in [§4.4.1](#441-pin-a-per-ticket-ledger-saved-view) | Saved-view CRUD is not in the Public API. One-time setup per project. |
| "Tool calls per ticket (last 24h)" dashboard tile | Manual UI step — runbook in [§4.4.2](#442-add-a-tool-calls-per-ticket-last-24h-dashboard-tile) | Widget CRUD is not in the Public API. |
| Cursor IDE per-session baggage | Shipped — see [`tools/ticket-propagation/`](../tools/ticket-propagation/) (`panacea-ticket`, `panacea-baggage-shim`, `panacea-ledger`, `verify-baggage-shim`) | Cursor's static `~/.cursor/mcp.json` cannot carry per-session ticket. Closed by a local baggage-injecting shim. |
| cursor-cli rewrite emits baggage | **Shipped — pending PR review** ([panacea-agent #161](https://github.com/nutanix-core/panacea-agent/pull/161)) | First non-IDE originator on the contract. Renders per-job `mcp.json` baggage. See §7.2. |
| Prometheus label for `ticket` | **Will not do** | Cardinality blast radius. Trace store is the right home. |

---

## 7. Conformance — the cursor-cli rewrite

> The cursor-cli rewrite ([`panacea-agent` PR #161, branch `mohit/cursor-cli-aigw-cutover`](https://github.com/nutanix-core/panacea-agent/pull/161)) is the first non-curl, non-IDE originator on this contract. It is implemented and pending review at the time of writing. This section documents the conformance check so future originators (Slack bot, web UI, eval harness) can follow the same recipe.

The Panacea AI Gateway already validates and promotes baggage to Langfuse-native dimensions (§3). To participate in the per-incident MCP ledger, an originator MUST set the W3C `baggage` header on every outbound HTTP call to the gateway. Nothing else — no Langfuse SDK, no per-call tagging, no ledger storage.

### 7.1 Required baggage on every gateway-bound request

| Key | Value | Why |
|---|---|---|
| `user` | requesting human's email (RFC 5322), URL-encoded if it contains `,` `;` `=` | Promoted to Langfuse `userId`; powers per-user filtering. |
| `role` | hard-code `cursor-cli-agent` | Distinguishes automated triage from interactive engineers. Appears as `role:cursor-cli-agent` in `langfuse.tags`. |
| `ticket` | the incident ID (`ENG-…`, `ONCALL-…`, `TH-…`, `KB-…`) supplied to the agent | The join key for the ledger. Appears as `ticket:<id>` in `langfuse.tags`. |
| `session` | the agent job ID / chat session ID (opaque, ≤128 bytes) | Splits one ticket across multiple chat sessions. Optional but recommended. |
| `log_bundle_id` (optional) | numeric Panacea bundle ID, when applicable | Ties the call chain to a specific log bundle. |

### 7.2 How cursor-cli does it (the reference implementation)

The cursor-cli rewrite renders a per-job `mcp.json` at `<job_dir>/.cursor/mcp.json` whose `mcpServers.*.headers.baggage` is the union of static template baggage (`user`, `role` defaults, etc.) and per-job dynamic baggage (`ticket=<incident_id>`, `session=<job_id>`, `log_bundle_id=<id>`). The renderer lives at [`services/cursor-cli-agent/app/services/mcp_config.py::render_for_job`](https://github.com/nutanix-core/panacea-agent/blob/mohit/cursor-cli-aigw-cutover/services/cursor-cli-agent/app/services/mcp_config.py). The merge rules are:

| Field | Source | Behavior on missing value |
|---|---|---|
| `user` | template, falling back to `STATIC_BAGGAGE_USER` env (default `cursor-cli-agent`) | always set |
| `role` | endpoint-authoritative (`rca-worker`, `chat`, `chat-stream`) | falls back to `STATIC_BAGGAGE_ROLE_DEFAULT` |
| `ticket` | `incident_id` from request | omitted (no `ticket=` emitted) |
| `session` | `session_id` from request / agent | omitted |
| `log_bundle_id` | `log_bundle_id` from request | omitted |

This matches §7.1 exactly and ensures gateway-side validation does not need to defend against empty `ticket=` values. New originators can either (a) follow the cursor-cli pattern of rendering `mcp.json` per request, or (b) use the OTel SDK to set baggage in process context and let auto-instrumentation emit the header on every outbound call (recommended for code that talks to the gateway over HTTP rather than MCP).

### 7.3 Wiring sketch (Go) — for originators using OTel directly

```go
import (
    "context"
    "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
    "go.opentelemetry.io/otel/baggage"
)

func contextWithIncident(ctx context.Context, userEmail, incidentID, sessionID string) context.Context {
    members := []baggage.Member{
        mustMember("user", userEmail),
        mustMember("role", "cursor-cli-agent"),
        mustMember("ticket", incidentID),
    }
    if sessionID != "" {
        members = append(members, mustMember("session", sessionID))
    }
    b, _ := baggage.New(members...)
    return baggage.ContextWithBaggage(ctx, b)
}

// And on the HTTP client used to talk to the gateway:
client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
// otelhttp injects `baggage:` and `traceparent:` automatically from ctx.

func mustMember(k, v string) baggage.Member {
    m, err := baggage.NewMemberRaw(k, v) // NewMemberRaw skips %-decoding; the SDK already escapes on emit
    if err != nil {
        panic(err) // contract violation: don't ship bad keys
    }
    return m
}
```

Set the baggage **once** at the boundary where you receive the request from upstream (the NKP API handler, or wherever a triage starts). All subsequent `client.Do(req.WithContext(ctx))` calls inside that workflow inherit it for free.

### 7.4 Don't do these (these are the foot-guns)

- Don't use `baggage.NewMemberRaw` with un-escaped commas / semicolons / equals signs in user-supplied values — they break the `baggage` header grammar. Use `baggage.NewMember` (which percent-encodes) for human-typed values.
- Don't set the same baggage key twice in one context — last writer wins, but it makes debugging confusing.
- Don't propagate `Authorization` headers as baggage. Baggage propagates downstream unchanged.
- Don't add `ticket=` only to "user-visible" calls — set it once at the workflow boundary so background tasks, retries, and fan-out all carry it.
- Don't reuse `trace_id` as the ticket. `trace_id` is per-call-chain; one triage = one `ticket=` × N independent traces.

### 7.5 Verification

Run §4.5 Step 1 (`verify-baggage-shim`) against your local shim if you also use Cursor IDE for development. For server-side originators, add a unit test that asserts your outbound HTTP requests carry `baggage:` containing all required keys for a given workflow — the cursor-cli rewrite covers this in [`services/cursor-cli-agent/tests/test_mcp_config.py`](https://github.com/nutanix-core/panacea-agent/blob/mohit/cursor-cli-aigw-cutover/services/cursor-cli-agent/tests/test_mcp_config.py). The gateway side is covered by [`internal/tracing/mcp_test.go::TestTracer_BaggagePromotesToLangfuseNativeAttrs`](../internal/tracing/mcp_test.go).

### 7.6 Known unrelated rough edges (track outside this contract)

- The legacy in-process Langfuse hook in `services/cursor-cli-agent/hooks/` writes via a different path than the gateway-promoted spans. Once the rewrite is in production end-to-end, the hook becomes redundant and can be removed by the rewrite owners. Not blocking the contract.
- `services/cursor-cli-agent/docker-compose.yml` references `LANGFUSE_HOST=…:3002` while production Langfuse listens on `:4000`. Owned by the rewrite; pick `:4000` and drop `:3002`.

---

## 8. References

- [W3C Trace Context](https://www.w3.org/TR/trace-context/)
- [W3C Baggage](https://www.w3.org/TR/baggage/)
- [OpenTelemetry semantic conventions for AI/LLM](https://opentelemetry.io/docs/specs/semconv/gen-ai/)
- Gateway implementation: [`internal/tracing/mcp.go`](../internal/tracing/mcp.go)
- Gateway tests: [`internal/tracing/mcp_test.go`](../internal/tracing/mcp_test.go)
- Telemetry rollout history: [`docs/telemetry-rollout-tracker.md`](telemetry-rollout-tracker.md)
- Native-attribute promotion PR: [PR #10](https://github.com/nutanix-core/panacea-ai-gateway/pull/10)
