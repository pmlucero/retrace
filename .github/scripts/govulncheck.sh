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

# The version is declared by the `tool` directive in tools/go.mod, a separate
# module so the binary's go.mod does not gain the dependency. Dependabot updates
# it with the 14-day cooldown (CONTRIBUTING.md, "Dependencies").
#
# Built once for the host: `GOOS=windows go tool govulncheck` would build
# govulncheck itself for Windows. `go tool -n` builds it and prints its path.
govulncheck="$(GOOS= GOARCH= go tool -modfile=tools/go.mod -n govulncheck)"
version="$(go list -modfile=tools/go.mod -m -f '{{.Version}}' golang.org/x/vuln)"

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
		GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "$govulncheck" ./... || status=$?
	done
done
exit "$status"
