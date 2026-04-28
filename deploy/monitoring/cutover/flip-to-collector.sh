#!/usr/bin/env bash
# Phase 3 redeploy_gateways: cutover step.
#
# After Phase 3's mohit/otel-langfuse-native-attrs PR merges, has been image-
# built, and you have just recreated both gateways via the unmodified
# start-*-otel.sh scripts, run THIS script to flip the OTLP target from
# "Langfuse direct" to "local otel-collector (Phase 2 stack) -> Langfuse".
#
# It is fully reversible via flip-back-to-langfuse.sh.
#
# Required state before running:
#   * ~/aigw-monitoring-stack/ stack is up (docker compose ps shows healthy).
#   * Phase 3 PR's smoke test succeeded (Langfuse UI shows user.id and tags
#     populated on the latest traces).

set -euo pipefail

STACK_DIR="${HOME}/aigw-monitoring-stack"
SCRIPT_DIR="${HOME}/aigw-otel-langfuse-backup"
COLLECTOR_HOST="10.113.24.33"
COLLECTOR_PORT="4328"  # OTLP HTTP

if ! docker compose -f "${STACK_DIR}/docker-compose.yml" ps --status running --quiet otel-collector | grep -q .; then
  echo "ERROR: otel-collector container is not running. Bring up Phase 2 stack first."
  exit 1
fi

if ! curl -sSf "http://${COLLECTOR_HOST}:9091/api/v1/query?query=up%7Bjob%3D%22otel-collector-self%22%7D" \
     | grep -q '"value":\[.*,"1"\]'; then
  echo "ERROR: otel-collector self-scrape isn't reporting up=1 in Prometheus. Aborting."
  exit 1
fi

ENDPOINT="http://${COLLECTOR_HOST}:${COLLECTOR_PORT}"

for f in "${SCRIPT_DIR}/start-gateway-otel.sh" "${SCRIPT_DIR}/start-eval-gateway-otel.sh"; do
  if [ ! -f "$f" ]; then
    echo "ERROR: $f not found."
    exit 1
  fi
  cp "$f" "$f.pre-collector-cutover.bak"
  sed -i.tmp \
    -e "s#OTEL_EXPORTER_OTLP_ENDPOINT=http://10.48.65.201:4000/api/public/otel#OTEL_EXPORTER_OTLP_ENDPOINT=${ENDPOINT}#" \
    -e "/OTEL_EXPORTER_OTLP_HEADERS=/d" \
    -e "s#OTEL_METRICS_EXPORTER=none#OTEL_METRICS_EXPORTER=otlp#" \
    "$f"
  rm -f "$f.tmp"
  echo "patched $f (backup at $f.pre-collector-cutover.bak)"
done

echo
echo "Now recreate the gateways with the patched scripts:"
echo "  bash ${SCRIPT_DIR}/start-gateway-otel.sh"
echo "  bash ${SCRIPT_DIR}/start-eval-gateway-otel.sh"
echo
echo "Then verify:"
echo "  * Langfuse UI shows new traces (same shape as before)."
echo "  * Grafana 'Collector Span Throughput' panel populates."
echo "  * Grafana 'GenAI Request Rate (post-cutover)' panel starts to populate."
