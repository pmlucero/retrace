#!/usr/bin/env python3
"""Summarize Go test coverage for a pull request, as Markdown.

Reads a Go cover profile and a unified diff of the change, and reports:
  * Statement coverage per package, and in total.
  * Coverage of the statements on lines the change adds or modifies, with
    the uncovered ranges listed, so a reviewer can see new code that no test
    executes (docs/review-guidelines.md, Tests).

There is no threshold: the report informs the review, it never fails CI.

Usage: coverage_report.py <cover profile> <unified diff> [repo-root]
"""

import re
import sys
from collections import defaultdict
from pathlib import Path

BLOCK_RE = re.compile(r"^(.+):(\d+)\.\d+,(\d+)\.\d+ (\d+) (\d+)$")
HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")


def module_path(root):
    for line in (root / "go.mod").read_text().splitlines():
        if line.startswith("module "):
            return line.split()[1]
    sys.exit("go.mod has no module line")


def read_profile(path, module):
    """Return {(file, start, end): [statements, covered]} with repo-relative files."""
    blocks = {}
    for line in Path(path).read_text().splitlines():
        m = BLOCK_RE.match(line)
        if not m:
            continue  # the "mode:" header
        name, start, end, stmts, count = m.groups()
        file = name.removeprefix(module + "/")
        key = (file, int(start), int(end))
        # A block shows up once per test binary that loads its package.
        prev = blocks.get(key, [int(stmts), False])
        blocks[key] = [prev[0], prev[1] or int(count) > 0]
    return blocks


def changed_lines(diff_path):
    """Return {file: set of new-side line numbers added or modified}."""
    changed = defaultdict(set)
    file = None
    for line in Path(diff_path).read_text().splitlines():
        if line.startswith("+++ "):
            target = line[4:]
            file = target[2:] if target.startswith("b/") else None
            continue
        m = HUNK_RE.match(line)
        if m and file:
            cur = int(m.group(1))
            continue
        if file is None or not line or line.startswith("---"):
            continue
        if line[0] == "+":
            changed[file].add(cur)
            cur += 1
        elif line[0] == " ":
            cur += 1
    return changed


def pct(covered, total):
    return f"{100 * covered / total:.1f}%" if total else "n/a"


def ranges(lines):
    """Collapse sorted line numbers into "a-b" ranges."""
    out, start, prev = [], None, None
    for n in sorted(lines):
        if start is None:
            start = prev = n
        elif n == prev + 1:
            prev = n
        else:
            out.append(f"{start}" if start == prev else f"{start}-{prev}")
            start = prev = n
    if start is not None:
        out.append(f"{start}" if start == prev else f"{start}-{prev}")
    return ", ".join(out)


def main():
    if len(sys.argv) not in (3, 4):
        sys.exit(__doc__)
    root = Path(sys.argv[3] if len(sys.argv) == 4 else ".")
    if not Path(sys.argv[1]).is_file():
        print("## Test coverage\n\nNo cover profile: the tests did not build or run.")
        return
    blocks = read_profile(sys.argv[1], module_path(root))
    changed = changed_lines(sys.argv[2])

    per_pkg = defaultdict(lambda: [0, 0])
    for (file, _, _), (stmts, covered) in blocks.items():
        pkg = file.rsplit("/", 1)[0]
        per_pkg[pkg][0] += stmts
        per_pkg[pkg][1] += stmts if covered else 0
    total = sum(t for t, _ in per_pkg.values())
    total_cov = sum(c for _, c in per_pkg.values())

    print("## Test coverage\n")
    print("| Package | Statements | Coverage |")
    print("|---|---:|---:|")
    for pkg in sorted(per_pkg):
        t, c = per_pkg[pkg]
        print(f"| `{pkg}` | {t} | {pct(c, t)} |")
    print(f"| **Total** | {total} | **{pct(total_cov, total)}** |\n")

    print("### Changed code\n")
    rows, uncovered = [], {}
    for file in sorted(changed):
        if not file.endswith(".go") or file.endswith("_test.go"):
            continue
        lines = changed[file]
        t = c = 0
        missed = set()
        for (bfile, start, end), (stmts, covered) in blocks.items():
            if bfile != file:
                continue
            touched = {n for n in range(start, end + 1) if n in lines}
            if not touched:
                continue
            t += stmts
            if covered:
                c += stmts
            else:
                missed |= touched
        if t:
            rows.append(f"| `{file}` | {t} | {pct(c, t)} |")
        if missed:
            uncovered[file] = ranges(missed)
    if not rows:
        print("No executable Go statements changed.")
        return
    print("Statements on lines this change adds or modifies:\n")
    print("| File | Changed statements | Covered |")
    print("|---|---:|---:|")
    print("\n".join(rows))
    if uncovered:
        print("\nChanged lines that no test executes:\n")
        for file, r in uncovered.items():
            print(f"- `{file}`: {r}")
    else:
        print("\nEvery changed statement is executed by a test.")


if __name__ == "__main__":
    main()
