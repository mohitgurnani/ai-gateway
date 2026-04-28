#!/usr/bin/env bash
# Reverts what flip-to-collector.sh did: restores the start-*-otel.sh scripts
# from .pre-collector-cutover.bak, putting OTEL_EXPORTER_OTLP_ENDPOINT back
# to the direct Langfuse URL.

set -euo pipefail

SCRIPT_DIR="${HOME}/aigw-otel-langfuse-backup"

for f in "${SCRIPT_DIR}/start-gateway-otel.sh" "${SCRIPT_DIR}/start-eval-gateway-otel.sh"; do
  bak="$f.pre-collector-cutover.bak"
  if [ ! -f "$bak" ]; then
    echo "ERROR: backup $bak not found; cannot revert."
    exit 1
  fi
  cp "$bak" "$f"
  echo "reverted $f"
done

echo
echo "Now recreate the gateways with the reverted scripts:"
echo "  bash ${SCRIPT_DIR}/start-gateway-otel.sh"
echo "  bash ${SCRIPT_DIR}/start-eval-gateway-otel.sh"
