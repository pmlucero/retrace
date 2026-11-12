#!/bin/sh
# Runs govulncheck on the Go toolchain in PATH, once for each GOOS/GOARCH the
# release ships, read from the `goos:` and `goarch:` lists in .goreleaser.yaml
# so a new target cannot go unscanned. govulncheck only analyzes the files built
# for the target platform and cgo setting, and the standard library has
# per-platform and cgo-only code, so each scan uses the release's build
# configuration (CGO_ENABLED=0, ADR 0001) (#133).
# Used by .github/workflows/govulncheck.yml and release.yml, and locally:
#
#   sh .github/scripts/govulncheck.sh
set -eu

# .goreleaser.yaml and ./... are relative to the repository root.
cd "$(dirname "$0")/../.."

# At least 14 days old (CONTRIBUTING.md, "Dependencies"). Dependabot does not
# see a version named in a script, so update it by hand.
version=v1.9.0

# Built once for the host: `GOOS=windows go run` would build govulncheck itself
# for Windows. `go build` does not take a path@version, so install it into a
# temporary GOBIN.
bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
GOBIN="$bin" GOOS= GOARCH= go install "golang.org/x/vuln/cmd/govulncheck@${version}"

# The one-line `key: [a, b]` list from .goreleaser.yaml, space-separated.
list() {
	sed -n "s/^[[:space:]]*$1:[[:space:]]*\\[\\(.*\\)\\][[:space:]]*\$/\\1/p" .goreleaser.yaml | tr ',' ' '
}
goos="$(list goos)"
goarch="$(list goarch)"
if [ -z "$goos" ] || [ -z "$goarch" ]; then
	echo "govulncheck.sh: no goos: [...] or goarch: [...] list in .goreleaser.yaml" >&2
	exit 1
fi

status=0
for os in $goos; do
	for arch in $goarch; do
		echo "== govulncheck ${version}, ${os}/${arch}, CGO_ENABLED=0, $(go version)"
		GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "$bin/govulncheck" ./... || status=$?
	done
done
exit "$status"
