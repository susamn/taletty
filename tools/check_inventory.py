"""Fail if production source appears without a registered coverage target."""

import argparse
import json
from pathlib import Path
import re
import sys


MODULES = {
    "app": "github.com/susamn/taletty/app",
    "engine": "github.com/susamn/taletty/engine",
}
IGNORED = {".git", ".venv", "__pycache__", "coverage", "vendor"}


def inventory(root):
    files = [path.relative_to(root) for path in root.rglob("*")
             if path.is_file() and not IGNORED.intersection(path.relative_to(root).parts)]
    modules = {path.parent.as_posix() for path in files if path.name == "go.mod"}
    if modules != set(MODULES):
        raise ValueError("Go modules must match the registered app/engine targets")
    for directory, expected in MODULES.items():
        source = (root / directory / "go.mod").read_text(encoding="utf-8")
        match = re.search(r"(?m)^module\s+(\S+)[ \t]*$", source)
        if not match or match.group(1) != expected:
            raise ValueError(f"{directory}: unexpected module identity")
    for path in files:
        owner = path.parts[0]
        if path.suffix == ".py" and owner not in {"tools", "tests"}:
            raise ValueError("register Python adapter/source coverage before adding production Python")
        if path.suffix == ".go" and owner not in {*MODULES, "tests"}:
            raise ValueError("Go source must belong to a registered module")
    return sorted(modules)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("."))
    args = parser.parse_args(argv)
    try:
        modules = inventory(args.root)
    except (ValueError, OSError, UnicodeError) as error:
        print(f"FAIL inventory: {error}", file=sys.stderr)
        return 1
    print("modules=" + json.dumps(modules))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
