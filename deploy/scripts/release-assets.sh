#!/usr/bin/env bash
# Usage: deploy/scripts/release-assets.sh VERSION IMAGE_DIGEST OUT_DIR
# Writes the release files that GoReleaser does not build into OUT_DIR:
#   ehlo-chart-VERSION.tgz                    Helm chart, image pinned to IMAGE_DIGEST
#   ehlo-monitoring-bundle_VERSION.tar.gz     ehlo-monitoring render -format all, default config
#   ehlo-dashboards-classic_VERSION.tar.gz    classic dashboards from scripts/export-classic.sh
# Tarballs are reproducible for a given commit: sorted names, the commit
# time as mtime, owner 0, gzip without a name or time.
set -euo pipefail
cd "$(dirname "$0")/../.."

[[ $# -eq 3 ]] || { echo "usage: $0 VERSION IMAGE_DIGEST OUT_DIR" >&2; exit 2; }
version=$1 digest=$2 out=$3
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "version $version is not semver without v" >&2; exit 2; }
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "image digest $digest is not sha256:<64 hex>" >&2; exit 2; }

mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
epoch=$(git log -1 --format=%ct)

tar=tar
if command -v gtar >/dev/null; then tar=gtar; fi
"$tar" --version | grep -q 'GNU tar' || { echo "GNU tar is required (gtar on macOS)" >&2; exit 1; }

tarball() {
    local dir=$1 name=$2
    "$tar" --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -C "$work" -cf - "$dir" | gzip -n > "$out/$name"
    echo "$out/$name"
}

echo "== chart"
deploy/scripts/chart-files.sh -check
cp -R deploy/helm/ehlo "$work/chart"
sed -i.bak 's|^  digest: ""$|  digest: "'"$digest"'"|' "$work/chart/values.yaml" && rm "$work/chart/values.yaml.bak"
grep -q "^  digest: \"$digest\"$" "$work/chart/values.yaml" || { echo "image.digest not set in values.yaml" >&2; exit 1; }
deploy/scripts/helm.sh lint "$work/chart" --strict
deploy/scripts/helm.sh package "$work/chart" --version "$version" --app-version "$version" -d "$work"
mv "$work/ehlo-$version.tgz" "$out/ehlo-chart-$version.tgz"
deploy/scripts/helm.sh template ehlo "$out/ehlo-chart-$version.tgz" | grep -q "image: ghcr.io/cauebraz/ehlo:$version@$digest" \
    || { echo "packaged chart does not reference ghcr.io/cauebraz/ehlo:$version@$digest" >&2; exit 1; }

echo "== monitoring bundle"
go run ./cmd/ehlo-monitoring render -format all -out "$work/ehlo-monitoring-$version"
cp LICENSE "$work/ehlo-monitoring-$version/"
tarball "ehlo-monitoring-$version" "ehlo-monitoring-bundle_$version.tar.gz"

echo "== classic dashboards"
scripts/export-classic.sh -in "$work/ehlo-monitoring-$version/dashboards" -out "$work/ehlo-dashboards-classic-$version"
tarball "ehlo-dashboards-classic-$version" "ehlo-dashboards-classic_$version.tar.gz"

ls -l "$out"
