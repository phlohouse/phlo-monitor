#!/usr/bin/env bash
# Build release assets locally or in CI. This script never publishes anything.
set -euo pipefail

if [[ $# -ne 2 || ! $1 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo 'usage: bash scripts/package-release.sh vMAJOR.MINOR.PATCH NEW_OUTPUT_DIRECTORY' >&2
	exit 1
fi
version=$1
out=$2
# A new directory prevents accidental reuse of stale or already packaged files.
mkdir -p "$(dirname "$out")"
mkdir "$out"
for arch in amd64 arm64; do
	binary="$out/phlo-monitor_${version}_linux_${arch}"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -p 1 -trimpath -buildvcs=false \
		-ldflags "-s -w -X main.version=$version" -o "$binary" ./cmd/phlo-monitor
	metadata=$(go version -m "$binary")
	for setting in CGO_ENABLED=0 GOOS=linux GOARCH=$arch; do
		printf '%s\n' "$metadata" | grep -F "$setting" >/dev/null
	done
	if [ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" = "linux/$arch" ]; then
		test "$("$binary" --version)" = "phlo-monitor $version linux/$arch"
	fi
done
(
	cd "$out"
	sha256sum "phlo-monitor_${version}_linux_amd64" "phlo-monitor_${version}_linux_arm64" \
		> "phlo-monitor_${version}_SHA256SUMS"
	sha256sum --strict --check "phlo-monitor_${version}_SHA256SUMS"
)
