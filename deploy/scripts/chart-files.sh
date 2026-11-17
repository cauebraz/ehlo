#!/usr/bin/env bash
# Usage: deploy/scripts/chart-files.sh [-check]
# Copies the monitoring files that ehlo-monitoring renders with the default
# config (prometheus/rules, grafana/dashboards) into deploy/helm/ehlo/files,
# so the packaged chart carries them. -check fails when the copies differ.
set -euo pipefail
cd "$(dirname "$0")/../.."

files=deploy/helm/ehlo/files
want=$(mktemp -d)
trap 'rm -rf "$want"' EXIT
mkdir -p "$want/rules" "$want/dashboards"
cp prometheus/rules/ehlo-recording.yml prometheus/rules/ehlo-alerts.yml "$want/rules/"
cp grafana/dashboards/*.json "$want/dashboards/"

if [[ "${1:-}" == -check ]]; then
    diff -r "$want" "$files" || { echo "$files is stale; run just dashboard"; exit 1; }
    exit 0
fi
rm -rf "$files"
mkdir -p "$files"
cp -R "$want/rules" "$want/dashboards" "$files/"
echo "updated $files"
