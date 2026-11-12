#!/bin/bash
# Build, validate and launch an Antithesis run of the snapd-incus workload.
# Usage: scripts/launch.sh <max-compute-hours> [description]
# Check the task's compute budget before each launch.
set -eu
export SNOUTY_CONTAINER_ENGINE=docker
HOURS=${1:?max compute hours}; DESC=${2:-snapd in incus}
cd "$(dirname "$0")/../config"
export COMPOSE_FILE=docker-compose.yaml:docker-compose.local.yaml
docker compose build
docker compose down -v >/dev/null 2>&1 || true
unset COMPOSE_FILE
snouty validate . --timeout 15m
SNOUTY_UNSTABLE_FEATURES=teams snouty launch --launcher basic_test --config . \
  --test-name snapd-incus \
  --description "$DESC" \
  --fault-tolerance high_availability \
  --fault-exclusions '^driver$' \
  --max-compute-hours "$HOURS" \
  --setup-timeout 20m \
  --param attrs.plugin_version=0.0.9 \
  --param attrs.workload=snapd-incus \
  --param attrs.snapd_commit="$(git rev-parse --short HEAD)"
# Keep the build cache from filling the disk between launches.
docker builder prune -f --filter until=1h >/dev/null 2>&1 || true
