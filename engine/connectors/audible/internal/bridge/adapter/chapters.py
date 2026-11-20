"""Private chapter preparation for the existing copy/remux path (no provider I/O).

Reuse audible-tui's positive-leaf, stable-start-order and ffmetadata escaping
algorithm. Retain the sanitized tree separately; never flatten the browsing model.
"""

import json

from .library import MAX_DURATION_MS, MAX_PAGE_BYTES, _text
from .sdk_session import ProviderError

MAX_NODES, MAX_DEPTH, MAX_TITLE_BYTES = 10000, 32, 4096


def prepare(info):
    """Return (owned private tree, optional ffmetadata), never mutate SDK input.

    Structural IDs are book-local index paths, stable for an unchanged tree, not
    provider revision IDs. Missing start defaults to0 as in the prototype; missing
    length remains unknown. No positive leaves means None: retain embedded chapters.
    """
    if info is None:
        info = {}
    if not isinstance(info, dict):
        raise ProviderError("protocol_error")
    count = 0
    leaves = []

    def visit(items, path, depth):
        nonlocal count
        if not isinstance(items, list) or depth > MAX_DEPTH:
            raise ProviderError("protocol_error")
        result = []
        for index, item in enumerate(items):
            count += 1
            if count > MAX_NODES or not isinstance(item, dict):
                raise ProviderError("protocol_error")
            start, length = item.get("start_offset_ms", 0), item.get("length_ms")
            if type(start) is not int or not 0 <= start <= MAX_DURATION_MS:
                raise ProviderError("protocol_error")
            if length is not None and (type(length) is not int or not 0 <= length <= MAX_DURATION_MS - start):
                raise ProviderError("protocol_error")
            title = "Chapter" if item.get("title") is None else _text(item["title"]) or "Chapter"
            if len(title.encode("utf-8")) > MAX_TITLE_BYTES:
                raise ProviderError("protocol_error")
            identity = f"{path}.{index}" if path else str(index)
            children = item.get("chapters")
            if children is None:
                children = []
            if not isinstance(children, list):
                raise ProviderError("protocol_error")
            node = dict(id=identity, title=title, start_ms=start)
            if length is not None:
                node["end_ms"] = start + length
            node["children"] = visit(children, identity, depth + 1) if children else []
            result.append(node)
            if not children and length is not None and length > 0:
                leaves.append(node)
        return result

    items = info.get("chapters")
    tree = visit([] if items is None else items, "", 1)
    if len(json.dumps(tree, ensure_ascii=True, separators=(",", ":")).encode()) > MAX_PAGE_BYTES:
        raise ProviderError("protocol_error")
    if not leaves:
        return tree, None
    lines = [";FFMETADATA1"]
    for leaf in sorted(leaves, key=lambda node: node["start_ms"]):
        title = leaf["title"]
        for special in ("\\", "=", ";", "#"):
            title = title.replace(special, "\\" + special)
        lines += ["[CHAPTER]", "TIMEBASE=1/1000", f'START={leaf["start_ms"]}',
                  f'END={leaf["end_ms"]}', "title=" + title]
    metadata = "\n".join(lines) + "\n"
    if len(metadata.encode("utf-8")) > MAX_PAGE_BYTES:
        raise ProviderError("protocol_error")
    return tree, metadata
