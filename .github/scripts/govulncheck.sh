#!/bin/sh
# Runs govulncheck on the Go toolchain in PATH, once for each GOOS the release
# ships, read from the `goos:` list in .goreleaser.yaml so a new target cannot
# go unscanned. govulncheck only analyzes the files built for the target GOOS
# and cgo setting, and the standard library has per-OS and cgo-only code, so
# each scan uses the release's build configuration (CGO_ENABLED=0, ADR 0001)
# (#133).
# Used by .github/workflows/govulncheck.yml and release.yml, and locally:
#
#   sh .github/scripts/govulncheck.sh
set -eu

# At least 14 days old (CONTRIBUTING.md, "Dependencies"). Dependabot does not
# see a version named in a script, so update it by hand.
version=v1.9.0

# Built once for the host: `GOOS=windows go run` would build govulncheck itself
# for Windows. `go build` does not take a path@version, so install it into a
# temporary GOBIN.
bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
GOBIN="$bin" GOOS= GOARCH= go install "golang.org/x/vuln/cmd/govulncheck@${version}"

goos="$(sed -n 's/^[[:space:]]*goos:[[:space:]]*\[\(.*\)\][[:space:]]*$/\1/p' .goreleaser.yaml | tr ',' ' ')"
if [ -z "$goos" ]; then
	echo "govulncheck.sh: no goos: [...] list in .goreleaser.yaml" >&2
	exit 1
fi

status=0
for os in $goos; do
	echo "== govulncheck ${version}, GOOS=${os}, CGO_ENABLED=0, $(go version)"
	GOOS="$os" CGO_ENABLED=0 "$bin/govulncheck" ./... || status=$?
done
exit "$status"
