#!/bin/sh
# Third-party license notices for the release archives (ADR 0012, #134). The
# BSD-3-Clause and MIT licenses of the modules linked into retrace require
# their notices to travel with the binaries.
#
#   sh .github/scripts/notices.sh         writes THIRD_PARTY_NOTICES.txt
#   sh .github/scripts/notices.sh check   checks it covers every module linked
#                                         into the binaries in dist/
#
# The file is generated, not committed: GoReleaser runs this script as a
# `before` hook (.goreleaser.yaml), so it always matches the module graph being
# built. It holds the Go standard library's license and every LICENSE*,
# COPYING*, NOTICE* and PATENTS file of each module linked into ./cmd/retrace
# for any GOOS/GOARCH the release ships (the module set differs per platform,
# e.g. go-isatty), read from the module cache. Only the Go toolchain is used.
# `check` runs in release.yml after the build: in the snapshot job on PRs, and
# in the release job before the release is published.
set -eu

# Byte order for `sort` and for the shell's glob expansion: the file goes into
# every archive, and archives must be the same bytes on any machine.
export LC_ALL=C

# .goreleaser.yaml and ./cmd/retrace are relative to the repository root.
cd "$(dirname "$0")/../.."

out=THIRD_PARTY_NOTICES.txt

if [ "${1:-}" = check ]; then
	status=0
	found=0
	for bin in dist/retrace_*/retrace dist/retrace_*/retrace.exe; do
		[ -f "$bin" ] || continue
		found=1
		# The `dep <path> <version> <sum>` lines of the embedded build info. A
		# failure inside `for ... in $(...)` would pass unseen, so capture first
		# and fail closed on a binary without them.
		if ! info="$(go version -m "$bin")"; then
			echo "notices.sh: cannot read the build info of $bin" >&2
			exit 1
		fi
		deps="$(printf '%s\n' "$info" | awk '$1 == "dep" { print $2 "@" $3 }')"
		if [ -z "$deps" ]; then
			echo "notices.sh: no dep lines in the build info of $bin" >&2
			exit 1
		fi
		for mod in $deps; do
			if ! grep -Fqx "${mod%@*} ${mod#*@}" "$out"; then
				echo "notices.sh: $out misses ${mod%@*} ${mod#*@}, linked into $bin" >&2
				status=1
			fi
		done
	done
	if [ "$found" = 0 ]; then
		echo "notices.sh: no binaries under dist/; run GoReleaser first" >&2
		exit 1
	fi
	exit "$status"
fi

# The one-line `key: [a, b]` list from .goreleaser.yaml, space-separated
# (the same parsing as govulncheck.sh).
list() {
	sed -n "s/^[[:space:]]*$1:[[:space:]]*\\[\\(.*\\)\\][[:space:]]*\$/\\1/p" .goreleaser.yaml | tr ',' ' '
}
goos="$(list goos)"
goarch="$(list goarch)"
if [ -z "$goos" ] || [ -z "$goarch" ]; then
	echo "notices.sh: no goos: [...] or goarch: [...] list in .goreleaser.yaml" >&2
	exit 1
fi

# "<path> <version> <dir>" of every non-main module in the build, per platform,
# with the release's build configuration (CGO_ENABLED=0, ADR 0001). <dir> is
# where the module's files are, a replacement's if go.mod replaces it.
mods="$(mktemp)"
trap 'rm -f "$mods" "$mods.raw" "$out.tmp"' EXIT
for os in $goos; do
	for arch in $goarch; do
		# Not in a pipeline, so `set -e` stops on a failing `go list`.
		GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go list -deps \
			-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' \
			./cmd/retrace >>"$mods.raw"
	done
done
sort -u "$mods.raw" >"$mods"

rule='================================================================================'

# section <title> <dir>: every license file in <dir>, under a <title> header.
# A NOTICE or PATENTS file alone is not a license, so it does not count.
section() {
	licensed=no
	for f in "$2"/LICENSE* "$2"/LICENCE* "$2"/COPYING*; do
		[ -f "$f" ] && licensed=yes
	done
	if [ "$licensed" = no ]; then
		echo "notices.sh: no LICENSE or COPYING file for $1 in $2" >&2
		exit 1
	fi
	printf '%s\n%s\n%s\n' "$rule" "$1" "$rule"
	for f in "$2"/LICENSE* "$2"/LICENCE* "$2"/COPYING* "$2"/NOTICE* "$2"/PATENTS; do
		[ -f "$f" ] || continue
		printf '\n--- %s ---\n\n' "${f##*/}"
		cat "$f"
		printf '\n'
	done
	printf '\n'
}

{
	echo "Third-party notices for retrace"
	echo
	echo "retrace is licensed under the Apache License 2.0 (LICENSE). Its binaries"
	echo "also contain the Go standard library and the Go modules below, each"
	echo "distributed under the license reproduced in its section."
	echo
	section "Go standard library $(go env GOVERSION)" "$(go env GOROOT)"
	while read -r path version dir; do
		# A replace with a local directory has no version, which shifts the
		# fields; there is none today, and it must not pass unnoticed.
		if [ -z "$dir" ]; then
			echo "notices.sh: unexpected module line \"$path $version\" (a local replace?)" >&2
			exit 1
		fi
		section "$path $version" "$dir"
	done <"$mods"
} >"$out.tmp"
mv "$out.tmp" "$out"
echo "notices.sh: wrote $out ($(wc -l <"$mods" | tr -d ' ') modules)"
