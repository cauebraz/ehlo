#!/usr/bin/env bash
# Usage: deploy/scripts/helm.sh path | deploy/scripts/helm.sh <helm args>
# Runs Helm v4.3.2. A helm on PATH with that version is used as is;
# otherwise the release archive is downloaded once into the cache, checked
# against the sha256 below, and unpacked.
set -euo pipefail

version=v4.3.2
sum_for() {
    case "$1" in
        darwin-amd64) echo 9e0068e9ae18c441d27388190ee7f53ae65eba44bfe910b40bdac44c4b51fa71 ;;
        darwin-arm64) echo 6f3421de3a8cc2ac578c8cfb33b8edde290dde2871bc1bab12e24f2e5caeaa90 ;;
        linux-amd64) echo d1c0278d46beab79582d7e50f5b3dc206944ba8e577cde618513fd07daa53983 ;;
        linux-arm64) echo df31cb61680ef728f82c770e5280dd565617bee6b039d37e85c677288be4bbb0 ;;
    esac
}

resolve() {
    if command -v helm >/dev/null && [[ "$(helm version --template '{{.Version}}' 2>/dev/null)" == "$version" ]]; then
        command -v helm
        return
    fi
    local os arch platform sum dir archive
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)
    case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
    platform="$os-$arch"
    sum=$(sum_for "$platform")
    [[ -n "$sum" ]] || { echo "no pinned helm for $platform" >&2; exit 1; }
    dir="${XDG_CACHE_HOME:-$HOME/.cache}/ehlo/helm-$version"
    if [[ ! -x "$dir/helm" ]]; then
        mkdir -p "$dir"
        archive="$dir/helm.tar.gz"
        curl -fsSL -o "$archive" "https://get.helm.sh/helm-$version-$platform.tar.gz"
        if command -v sha256sum >/dev/null; then
            echo "$sum  $archive" | sha256sum -c --quiet - >&2
        else
            echo "$sum  $archive" | shasum -a 256 -c --quiet - >&2
        fi
        tar -xzf "$archive" -C "$dir" --strip-components 1 "$platform/helm"
        rm "$archive"
    fi
    echo "$dir/helm"
}

if [[ "${1:-}" == path ]]; then
    resolve
else
    exec "$(resolve)" "$@"
fi
