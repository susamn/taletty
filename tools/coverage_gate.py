"""Validate independent coverage targets without trusting rounded percentages."""

import argparse
import json
from pathlib import Path
import re
import sys


GO_BLOCK = re.compile(r"(\S+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)")
NAME = re.compile(r"[a-z][a-z0-9_-]*")


def counts(summary):
    """Read coverage.py statement counts; excluded code cannot hide a deficit."""
    if not isinstance(summary, dict):
        raise ValueError("coverage summary must be an object")
    values = [summary.get(key) for key in
              ("covered_lines", "num_statements", "excluded_lines")]
    if any(type(value) is not int or value < 0 for value in values):
        raise ValueError("coverage counts must be nonnegative integers")
    covered, total, excluded = values
    if covered > total or excluded:
        raise ValueError("impossible counts or excluded statements")
    return covered, total


def python_counts(path):
    report = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(report, dict):
        raise ValueError("Python report must be an object")
    files = report.get("files")
    if not isinstance(files, dict) or not files:
        raise ValueError("Python report must include file summaries")
    covered = total = 0
    for filename, details in files.items():
        if not filename.strip() or not isinstance(details, dict):
            raise ValueError("invalid Python file entry")
        file_covered, file_total = counts(details.get("summary"))
        covered += file_covered
        total += file_total
    if (covered, total) != counts(report.get("totals")):
        raise ValueError("Python totals do not match file summaries")
    return covered, total


def go_counts(path):
    lines = path.read_text(encoding="utf-8").splitlines()
    if not lines or lines[0] not in ("mode: set", "mode: count", "mode: atomic"):
        raise ValueError("missing or invalid Go coverage mode")
    covered = total = 0
    seen = {}
    for line in lines[1:]:
        match = GO_BLOCK.fullmatch(line)
        if not match:
            raise ValueError("invalid Go coverage record")
        filename, *numbers = match.groups()
        start_line, start_col, end_line, end_col, statements, hits = map(int, numbers)
        if min(start_line, start_col, end_line, end_col) < 1:
            raise ValueError("Go source coordinates must be positive")
        if (start_line, start_col) > (end_line, end_col):
            raise ValueError("Go source range is reversed")
        block = (filename, start_line, start_col, end_line, end_col)
        if block in seen:
            previous_statements, previous_hits = seen[block]
            if statements != previous_statements:
                raise ValueError("conflicting Go coverage block weights")
            if hits and not previous_hits:
                covered += statements
            seen[block] = (statements, previous_hits or bool(hits))
            continue
        seen[block] = (statements, bool(hits))
        total += statements
        if hits:
            covered += statements
    return covered, total


def targets(go_reports, python_reports, required):
    """Require an explicit inventory, not whichever reports happen to exist."""
    if any(not NAME.fullmatch(name) for name in required):
        raise ValueError("invalid required target name")
    if len(set(required)) != len(required):
        raise ValueError("duplicate required target")
    reports = {}
    paths = set()
    for kind, entries in (("go", go_reports), ("python", python_reports)):
        for entry in entries:
            name, separator, filename = entry.partition("=")
            if not separator or not NAME.fullmatch(name) or not filename.strip():
                raise ValueError("report must be NAME=PATH with a valid target name")
            path = Path(filename).resolve()
            if name in reports or path in paths:
                raise ValueError("duplicate target or reused report path")
            reports[name] = (kind, path)
            paths.add(path)
    if set(reports) != set(required):
        raise ValueError("report targets must exactly match required targets")
    return reports


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require", action="append", required=True, metavar="NAME")
    parser.add_argument("--go", action="append", default=[], metavar="NAME=PATH")
    parser.add_argument("--python", action="append", default=[], metavar="NAME=PATH")
    args = parser.parse_args(argv)
    try:
        reports = targets(args.go, args.python, args.require)
        for name, (kind, path) in reports.items():
            covered, total = go_counts(path) if kind == "go" else python_counts(path)
            if total == 0:
                raise ValueError(f"{name}: no executable statements; not a coverage pass")
            if covered * 100 < total * 95:
                raise ValueError(f"{name}: {covered}/{total} statements is below 95%")
            print(f"PASS {name}: {covered}/{total} statements (>=95%)")
    except (ValueError, OSError, UnicodeError) as error:
        print(f"FAIL coverage: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
