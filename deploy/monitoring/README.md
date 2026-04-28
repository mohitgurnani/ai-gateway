# AI Gateway monitoring stack

Observability sidecar for the AI gateway VM at `10.113.24.33`. Originally
landed as Phase 2 of the Sohham PR follow-up; phases P3 / P3d / P4 / P5
extended it into the per-ticket-grouped, alertable, container-aware stack
documented in `panacea-ai-gateway/docs/telemetry-rollout-tracker.md`.

## What this stack does today

- **OTel Collector (contrib)** receives OTLP traces and metrics from the two
  AI gateway containers (post-P3d cutover). It tags every span with a
  `collector.host` resource attribute, batches, and forwards traces to
  Langfuse (`http://10.48.65.201:4000`) and metrics to Prometheus via
  `prometheusremotewrite`.
- **Prometheus** scrapes:
  - Both gateway admin endpoints (`/metrics` on `:6067` and `:6068`)
  - Blackbox HTTP probes against the core MCP set
  - Otel-collector self-metrics (`:8888`)
  - Alertmanager self-metrics (`:9093`) - so we can see notify dispatch
  - cAdvisor (`:8085` / container CPU / RAM / FDs / network / FS)
  - node-exporter (`:9100` / host CPU / RAM / load / disk)
  - process-exporter (`:9256` / per-process FD count by group)
- **Alertmanager** receives 8 alert rules (3 groups: `aigw-platform`,
  `mcp-health`, `traffic-anomalies`) and pages `#panacea-mcp-monitoring`
  via a native Slack `slack_configs` integration. The Incoming Webhook URL
  is read from a secret file (`alertmanager/secrets/slack_webhook_url`)
  bind-mounted into the container - never embedded in compose env or YAML.
- **Grafana** is provisioned with two dashboards:
  - **AI Gateway Overview** (gateway up, MCP probe status / ratio /
    duration, GenAI / collector panels)
  - **MCP & Gateway Resource Usage** (host overview, sortable per-container
    CPU/RAM/FD/network table, per-MCP timeseries for CPU/RAM/FDs/network,
    tool-call rate / p95 latency / error rate per backend, top tool methods
    table, process-group FD counts) - this dashboard mirrors the layout of
    `~eng/panacea-ai-analysis/mcp_usage_dashboard.html`.
- **Blackbox-exporter** runs HTTP `/health` (and `/healthz` where needed)
  probes against the core MCP set.
- **cAdvisor / node-exporter / process-exporter** (P5) feed the new
  resource-usage dashboard. cAdvisor exposes per-container metrics
  including `container_file_descriptors`; process-exporter exposes
  `namedprocess_namegroup_open_filedesc` per process group so per-MCP FD
  counts are available even for containers that run multiple Python
  processes.

## Why ports 4327/4328 instead of 4317/4318

The host already runs a SigNoz collector that binds `4317`/`4318`. To stay
out of its way, the AI gateway monitoring collector exposes OTLP on
`4327`/`4328`. Internal-to-container ports remain `4317`/`4318` so any
default-config OTLP client speaks unchanged once it crosses the docker NAT.

## Files

```
docker-compose.yml
.env.example                       # template; copy to .env on host
otel-collector/config.yaml
prometheus/prometheus.yml
prometheus/rules/aigw-alerts.yml
alertmanager/alertmanager.yml
alertmanager/secrets/.gitignore    # secret file is host-only
blackbox/blackbox.yml
process-exporter/process-exporter.yml
grafana/provisioning/datasources/prometheus.yml
grafana/provisioning/dashboards/dashboards.yml
grafana/dashboards/aigw-overview.json    # ships in PR mohit/aigw-monitoring-dashboards
grafana/dashboards/mcp-usage.json        # ships in PR mohit/aigw-monitoring-dashboards
cutover/flip-to-collector.sh
cutover/flip-back-to-langfuse.sh
```

## Running

```bash
cd ~/aigw-monitoring-stack
cp .env.example .env             # first time only; fill in real values
mkdir -p alertmanager/secrets
echo "https://hooks.slack.com/services/XXX/YYY/ZZZ" \
    > alertmanager/secrets/slack_webhook_url
chmod 644 alertmanager/secrets/slack_webhook_url
docker compose up -d
docker compose ps
```

Endpoints exposed on the host:

| Service             | URL                                            |
| ------------------- | ---------------------------------------------- |
| Grafana             | http://10.113.24.33:3030  (admin / from .env)  |
| Prometheus          | http://10.113.24.33:9091                       |
| Alertmanager        | http://10.113.24.33:9095                       |
| Blackbox-exporter   | http://10.113.24.33:9115                       |
| cAdvisor            | http://10.113.24.33:8085                       |
| node-exporter       | http://10.113.24.33:9100                       |
| process-exporter    | http://10.113.24.33:9256                       |
| OTLP gRPC receiver  | 10.113.24.33:4327                              |
| OTLP HTTP receiver  | http://10.113.24.33:4328                       |

Anonymous viewers can read Grafana without logging in.

## Validation queries

```bash
# Prometheus is ready
curl -fsS http://10.113.24.33:9091/-/ready

# Both gateways are scraped successfully
curl -fsS 'http://10.113.24.33:9091/api/v1/query?query=up{job=~"aigw-.*"}' | jq

# MCP probes are reporting
curl -fsS 'http://10.113.24.33:9091/api/v1/query?query=probe_success' | jq

# Container-level CPU usage by MCP (P5)
curl -fsS 'http://10.113.24.33:9091/api/v1/query?query=sum%20by%20(name)%20(rate(container_cpu_usage_seconds_total%7Bname%3D~%22panacea-.*%22%7D%5B1m%5D))%20*%20100' | jq

# Per-container FD counts (P5)
curl -fsS 'http://10.113.24.33:9091/api/v1/query?query=container_file_descriptors%7Bname%3D~%22panacea-.*%22%7D' | jq

# Alertmanager dispatch counters (P4)
curl -fsS 'http://10.113.24.33:9091/api/v1/query?query=alertmanager_notifications_total%7Bintegration%3D%22slack%22%7D' | jq

# Grafana datasource health
curl -fsS http://10.113.24.33:3030/api/health
```

## Slack delivery

`alertmanager.yml` uses **native** `slack_configs` (not a custom relay).
The Incoming Webhook URL is read from a file:

```
alertmanager/secrets/slack_webhook_url    chmod 644
```

referenced from `alertmanager.yml` via `global.slack_api_url_file`. This
keeps the URL out of docker-compose env, out of source control, and out of
process listings. Templates render the page subject + body in the same
"WARNING: glean is down" format the legacy mcp-health-monitor used.

The earlier `slack-relay` Flask sidecar has been removed. There is no
Slack bot token in this stack.

## P3d cutover (already done; here for the record)

```bash
~/aigw-monitoring-stack/cutover/flip-to-collector.sh
bash ~/aigw-otel-langfuse-backup/start-gateway-otel.sh
bash ~/aigw-otel-langfuse-backup/start-eval-gateway-otel.sh
```

The script edits `~/aigw-otel-langfuse-backup/start-*-otel.sh` to point
`OTEL_EXPORTER_OTLP_ENDPOINT` at `http://10.113.24.33:4328`, drop the
`OTEL_EXPORTER_OTLP_HEADERS` (the auth header now lives on the collector
exporter), and switch `OTEL_METRICS_EXPORTER` from `none` to `otlp`. It
refuses to run if the collector is not healthy and saves a
`.pre-collector-cutover.bak` next to each script so the change is
reversible via `flip-back-to-langfuse.sh`.

## Resource footprint

Combined RSS observed in production (P5): ~1.0-1.3 GB. cAdvisor is the
single largest consumer. Disk usage grows with Prometheus retention
(`14d` default).
