#!/bin/sh
# Runs govulncheck on the Go toolchain in PATH, once for each GOOS the release
# ships (.goreleaser.yaml, "goos"). govulncheck only analyzes the files built
# for the target GOOS, and the standard library has per-OS code, so a scan for
# the host alone can miss a finding in the darwin or windows binaries (#133).
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

status=0
for goos in linux darwin windows; do
	echo "== govulncheck ${version}, GOOS=${goos}, $(go version)"
	GOOS="$goos" "$bin/govulncheck" ./... || status=$?
done
exit "$status"
