#!/usr/bin/env python3
"""Validate repository documentation.

Checks:
  * Relative links in Markdown files point to existing files, and #anchors
    point to existing headings (GitHub slug rules).
  * ADRs in docs/adr/ follow ADR 0000: file naming, sequential numbering,
    title line, Status and Date lines, required sections, and an index in
    docs/adr/README.md that matches each ADR's title and status.
  * docs/architecture/overview.json parses as JSON, meta.repository.revision
    is a full commit SHA on main, and every cited source (path, line,
    end_line) exists at that revision. Citations are permalinks, so this
    keeps them valid; it does not detect that the code at HEAD has moved
    away from them. It needs the pinned commit and main in the local clone,
    so CI checks out the full history.

External (http/https) links are not checked: they are slow and flaky in CI.

Usage: check_docs.py [repo-root]
"""

import json
import os
import re
import subprocess
import sys
from pathlib import Path

ADR_DIR = Path("docs/adr")
ADR_FILE_RE = re.compile(r"^(\d{4})-[a-z0-9]+(?:-[a-z0-9]+)*\.md$")
ADR_TITLE_RE = re.compile(r"^# (\d{4})\. (.+)$")
ADR_STATUS_RE = re.compile(r"^- Status: (Proposed|Accepted|Rejected|Superseded by (\d{4}))$")
ADR_DATE_RE = re.compile(r"^- Date: \d{4}-\d{2}-\d{2}$")
ADR_SECTIONS = ["## Context", "## Decision", "## Alternatives considered", "## Consequences"]
INDEX_ROW_RE = re.compile(r"^\| \[(\d{4})\]\((\d{4}-[^)]+\.md)\) \| (.+) \| (.+) \|$")

LINK_RE = re.compile(r"(?<!!)\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$")
FENCE_RE = re.compile(r"^(```|~~~)")
SKIP_DIRS = {".git", "node_modules"}

ARCH_JSON = Path("docs/architecture/overview.json")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
MAIN_REFS = ["refs/remotes/origin/main", "refs/heads/main"]


def strip_code(lines):
    """Yield (lineno, line) for lines outside fenced code blocks."""
    in_fence = False
    for n, line in enumerate(lines, 1):
        if FENCE_RE.match(line):
            in_fence = not in_fence
            continue
        if not in_fence:
            yield n, line


def slugify(text):
    """GitHub heading anchor: lowercase, drop punctuation, spaces to hyphens."""
    text = re.sub(r"`([^`]*)`", r"\1", text)
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = text.strip().lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


def anchors(path, cache={}):
    if path not in cache:
        seen, result = {}, set()
        for _, line in strip_code(path.read_text(encoding="utf-8").splitlines()):
            m = HEADING_RE.match(line)
            if not m:
                continue
            slug = slugify(m.group(2))
            n = seen.get(slug, 0)
            seen[slug] = n + 1
            result.add(slug if n == 0 else f"{slug}-{n}")
        cache[path] = result
    return cache[path]


def check_links(root, md_files):
    errors = []
    for md in md_files:
        lines = md.read_text(encoding="utf-8").splitlines()
        for n, line in strip_code(lines):
            line = re.sub(r"`[^`]*`", "", line)
            for target in LINK_RE.findall(line):
                if re.match(r"^[a-z][a-z0-9+.-]*:", target):
                    continue  # external or mailto
                path_part, _, fragment = target.partition("#")
                dest = (md.parent / path_part).resolve() if path_part else md.resolve()
                where = f"{md.relative_to(root)}:{n}"
                if not dest.exists():
                    errors.append(f"{where}: broken link: {target}")
                    continue
                if fragment and dest.suffix == ".md" and fragment not in anchors(dest):
                    errors.append(f"{where}: missing anchor: {target}")
    return errors


def check_adrs(root):
    errors = []
    adr_dir = root / ADR_DIR
    adrs = {}
    for f in sorted(adr_dir.glob("*.md")):
        if f.name == "README.md":
            continue
        m = ADR_FILE_RE.match(f.name)
        if not m:
            errors.append(f"{f.relative_to(root)}: name must be NNNN-kebab-case-title.md")
            continue
        num = m.group(1)
        if num in adrs:
            errors.append(f"{f.relative_to(root)}: duplicate ADR number {num}")
            continue
        lines = f.read_text(encoding="utf-8").splitlines()
        rel = f.relative_to(root)

        title = ADR_TITLE_RE.match(lines[0]) if lines else None
        if not title:
            errors.append(f"{rel}:1: first line must be '# {num}. Title'")
            continue
        if title.group(1) != num:
            errors.append(f"{rel}:1: title number {title.group(1)} does not match file number {num}")

        status = next((ADR_STATUS_RE.match(l) for l in lines if l.startswith("- Status:")), None)
        if not status:
            errors.append(f"{rel}: missing or invalid '- Status:' line")
        if not any(ADR_DATE_RE.match(l) for l in lines):
            errors.append(f"{rel}: missing or invalid '- Date: YYYY-MM-DD' line")
        for section in ADR_SECTIONS:
            if section not in lines:
                errors.append(f"{rel}: missing section '{section}'")

        adrs[num] = {
            "file": f.name,
            "title": title.group(2),
            "status": status.group(1) if status else None,
            "superseded_by": status.group(2) if status else None,
        }

    nums = sorted(adrs)
    expected = [f"{i:04d}" for i in range(len(nums))]
    if nums != expected:
        errors.append(f"{ADR_DIR}: ADR numbers must be sequential from 0000, found {', '.join(nums)}")
    for num, adr in adrs.items():
        if adr["superseded_by"] and adr["superseded_by"] not in adrs:
            errors.append(f"{ADR_DIR / adr['file']}: superseded by unknown ADR {adr['superseded_by']}")

    index = {}
    readme = adr_dir / "README.md"
    for n, line in enumerate(readme.read_text(encoding="utf-8").splitlines(), 1):
        m = INDEX_ROW_RE.match(line)
        if m:
            index[m.group(1)] = (n, m.group(2), m.group(3), m.group(4))
    rel_readme = readme.relative_to(root)
    for num, adr in adrs.items():
        if num not in index:
            errors.append(f"{rel_readme}: ADR {num} is missing from the index")
            continue
        n, file, title, status = index[num]
        if file != adr["file"]:
            errors.append(f"{rel_readme}:{n}: ADR {num} links to {file}, expected {adr['file']}")
        if title != adr["title"]:
            errors.append(f"{rel_readme}:{n}: ADR {num} title '{title}' does not match '{adr['title']}'")
        if adr["status"] and status != adr["status"]:
            errors.append(f"{rel_readme}:{n}: ADR {num} status '{status}' does not match '{adr['status']}'")
    for num in index.keys() - adrs.keys():
        errors.append(f"{rel_readme}:{index[num][0]}: index lists ADR {num}, which does not exist")
    return errors


def git(root, *args):
    """Run git in root; return (ok, stdout)."""
    try:
        r = subprocess.run(
            ["git", "-C", str(root), *args],
            capture_output=True,
            encoding="utf-8",
            errors="replace",
        )
    except OSError as e:
        return False, str(e)
    return r.returncode == 0, r.stdout


def is_line(n):
    """A 1-based line number. bool is excluded because True is an int."""
    return isinstance(n, int) and not isinstance(n, bool) and n >= 1


def find_sources(node, where="$"):
    """Yield (json_path, source) for every entry of every "sources" list."""
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "sources" and isinstance(v, list):
                for i, src in enumerate(v):
                    yield f"{where}.sources[{i}]", src
            else:
                yield from find_sources(v, f"{where}.{k}")
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from find_sources(v, f"{where}[{i}]")


def check_architecture(root):
    """Return (errors, number of citations checked)."""
    path = root / ARCH_JSON
    if not path.exists():
        return [], 0
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        return [f"{ARCH_JSON}:{e.lineno}: invalid JSON: {e.msg}"], 0

    rev = data
    for key in ("meta", "repository", "revision"):
        rev = rev.get(key) if isinstance(rev, dict) else None
    if not isinstance(rev, str) or not SHA_RE.match(rev):
        return [f"{ARCH_JSON}: meta.repository.revision must be a full 40-character commit SHA"], 0
    ok, _ = git(root, "cat-file", "-e", f"{rev}^{{commit}}")
    if not ok:
        return [
            f"{ARCH_JSON}: pinned revision {rev} is not in this clone; "
            "fetch the full history (git fetch --unshallow, or fetch-depth: 0 in CI) "
            "or pin a commit that exists on main"
        ], 0
    main = next((ref for ref in MAIN_REFS if git(root, "rev-parse", "-q", "--verify", ref)[0]), None)
    if main is None:
        return [f"{ARCH_JSON}: cannot find {' or '.join(MAIN_REFS)} to check the pinned revision"], 0
    if not git(root, "merge-base", "--is-ancestor", rev, main)[0]:
        return [
            f"{ARCH_JSON}: pinned revision {rev[:7]} is not on {main}; "
            "pin a commit that is already on main, so the permalinks survive the merge"
        ], 0

    errors, files = [], {}
    sources = list(find_sources(data))
    for where, src in sources:
        prefix = f"{ARCH_JSON}: {where}"
        if not isinstance(src, dict) or not isinstance(src.get("path"), str):
            errors.append(f"{prefix}: source must be an object with a string 'path'")
            continue
        cited = src["path"]
        if cited not in files:
            ok, text = git(root, "cat-file", "blob", f"{rev}:{cited}")
            files[cited] = len(text.splitlines()) if ok else None
        nlines = files[cited]
        if nlines is None:
            errors.append(f"{prefix}: {cited} is not a file at {rev[:7]}")
            continue
        line, end = src.get("line"), src.get("end_line")
        if end is not None and line is None:
            errors.append(f"{prefix}: {cited} has 'end_line' without 'line'")
            continue
        if line is None:
            continue
        if not is_line(line) or (end is not None and (not is_line(end) or end < line)):
            errors.append(
                f"{prefix}: {cited} has line={line!r}, end_line={end!r}; "
                "both must be integers >= 1 with end_line >= line"
            )
            continue
        last = end if end is not None else line
        if last > nlines:
            span = f"{line}-{end}" if end is not None else f"{line}"
            errors.append(
                f"{prefix}: {cited}:{span} is out of range at {rev[:7]} "
                f"(the file has {nlines} lines); fix the citation or re-pin the revision"
            )
    return errors, len(sources)


def main():
    root = Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    md_files = sorted(
        p for p in root.rglob("*.md") if not SKIP_DIRS.intersection(p.relative_to(root).parts)
    )
    arch_errors, citations = check_architecture(root)
    errors = check_links(root, md_files) + check_adrs(root) + arch_errors
    for e in errors:
        print(f"::error::{e}" if "GITHUB_ACTIONS" in os.environ else e)
    print(
        f"checked {len(md_files)} Markdown files and {citations} architecture citations: "
        f"{len(errors)} error(s)"
    )
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
