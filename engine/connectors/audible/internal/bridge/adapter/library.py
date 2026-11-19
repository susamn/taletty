"""Private provisional library pages. Exhaust inside sdk_session.authenticated.

Never apply a caller snapshot until exhaustion AND SDK finally persistence/clean
transport complete. No cache writes, framing/CLI, retries or entitlement claims.
"""

import json
import re
import unicodedata

from .sdk_session import ProviderError

PAGE_SIZE, MAX_PAGES = 100, 10000
MAX_DURATION_MS = 9_223_372_036_854  # floor(int64 nanoseconds / 1e6)
MAX_PAGE_BYTES = (4 << 20) - 512  # Reserve fixed bridge-v1 header/stage/ID/seq bytes.
_ANSI = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))")
_ASCII_CONTROLS = dict.fromkeys(range(32), " ") | {127: " "}


def _text(value):
    if not isinstance(value, str):
        raise ProviderError("protocol_error")
    # Strip complete CSI/OSC; flatten remaining C0/C1, bidi/format/surrogate code
    # points so even incomplete terminal sequences cannot execute on presentation.
    text = _ANSI.sub("", value)
    if text.isascii():
        return text.translate(_ASCII_CONTROLS).strip()
    return "".join(" " if unicodedata.category(c).startswith("C") else c
                   for c in text).strip()


def _contributors(value):
    if not isinstance(value, list):
        raise ProviderError("protocol_error")
    names = []
    for person in value:
        if not isinstance(person, dict):
            raise ProviderError("protocol_error")
        if person.get("name") is not None:
            name = _text(person["name"])
            if name:
                names.append({"name": name})
    return names


def _book(item):
    if not isinstance(item, dict):
        raise ProviderError("protocol_error")
    asin = item.get("asin")
    if not isinstance(asin, str) or not re.fullmatch(r"[A-Za-z0-9]{10}", asin):
        raise ProviderError("protocol_error")
    title = "Untitled" if item.get("title") is None else _text(item["title"]) or "Untitled"
    book = dict(id=dict(provider="audible", namespace="us", id=asin), title=title)
    if item.get("subtitle") is not None:
        book["subtitle"] = _text(item["subtitle"])
    for field in ("authors", "narrators"):
        if item.get(field) is not None:
            book[field] = _contributors(item[field])
    minutes = item.get("runtime_length_min")
    if minutes is not None:
        if type(minutes) not in (int, float) or minutes < 0:
            raise ProviderError("protocol_error")
        try:
            duration = round(minutes * 60000)  # Reject NaN/infinity, then bound converted value.
        except (ValueError, OverflowError):
            raise ProviderError("protocol_error") from None
        if duration > MAX_DURATION_MS:
            raise ProviderError("protocol_error")
        book["duration_ms"] = duration
    return book


def pages(client):
    """Yield deduplicated, bounded candidate book lists, never an authoritative cache."""
    if client.marketplace != "us":
        raise ProviderError("unsupported")
    seen = set()
    for page in range(1, MAX_PAGES + 1):
        result = client.get("library", page=page, num_results=PAGE_SIZE,
                            response_groups="contributors,media,product_desc,product_attrs,relationships",
                            sort_by="-PurchaseDate")
        if not isinstance(result, dict) or not isinstance(result.get("items"), list) or len(result["items"]) > PAGE_SIZE:
            raise ProviderError("protocol_error")
        fresh = []
        for item in result["items"]:
            book = _book(item)
            identity = book["id"]["id"]
            if identity not in seen:
                seen.add(identity)
                fresh.append(book)
        if result["items"] and not fresh:
            raise ProviderError("protocol_error")  # Repeated page: cannot trust completion.
        encoded = json.dumps({"books": fresh}, ensure_ascii=True, separators=(",", ":"), allow_nan=False)
        if len(encoded.encode("utf-8")) > MAX_PAGE_BYTES:
            raise ProviderError("protocol_error")
        if fresh:
            yield fresh
        if len(result["items"]) < PAGE_SIZE:
            return
    raise ProviderError("protocol_error")  # Never return a silently truncated library.
