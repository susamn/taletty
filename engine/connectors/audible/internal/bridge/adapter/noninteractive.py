"""Dedicated root-only operation child; trusted host selects the entrypoint.

Library, download producer and read-only reconciliation reuse the same loader and
stdio isolation. No TTY, discovery, locks or retries; caller owns the gate and must
join/check exit before interpreting results. Public Download lifecycle is #112.
"""

import json
import os


def write_frame(control, frame):
    encoded = json.dumps(frame, ensure_ascii=True, separators=(",", ":"), allow_nan=False)
    if len(encoded.encode("utf-8")) > 4 << 20:
        raise ValueError("frame too large")
    control.write(encoded + "\n")
    control.flush()


def main(sources, load_sources, null_stdio, action="library"):
    # PEP446 dup is noninheritable. SDK stdio/exec never own the protocol channel.
    with os.fdopen(os.dup(1), "w", encoding="utf-8") as control:
        operation_id, sequence, count, code, package = "invalid", 0, 0, "internal", None
        result = {}

        def emit(kind, **payload):
            nonlocal sequence
            sequence += 1
            write_frame(control, dict(protocol=1, operation_id=operation_id,
                                     seq=sequence, type=kind, **payload))

        try:
            os.set_inheritable(3, False)
            null_stdio((1, 2))
            names = ("doctor", "auth_store", "login", "sdk_session", "library")
            if action != "library":
                names += ("chapters", "licensing", "media_store", "remux", "composition", "reconciliation")
            package = load_sources(sources, names)
            # Request errors are distinct from trusted-source/normalization errors.
            try:
                request = package.doctor.read_request()
                operation_id = request["operation_id"]
                package.doctor.require_action(request, action)
                if action != "library":
                    book = request.get("book_id")
                    if (not isinstance(book, dict) or book.get("provider") != "audible"
                            or book.get("namespace") != "us"):
                        raise ValueError()
                    try:
                        package.media_store._leaf(book.get("id"))
                    except package.media_store.MediaStoreError:
                        raise ValueError() from None
            except (ValueError, UnicodeError):
                code = "invalid_input"
            else:
                null_stdio((0,))
                if action == "library":
                    with package.sdk_session.authenticated(3) as client:
                        for books in package.library.pages(client):
                            emit("stage", stage="library_page", books=books)
                            count += len(books)
                    result = {"count": count}
                elif action == "download":
                    from dataclasses import asdict
                    result = {"outcome": asdict(package.composition.run(3, book["id"]))}
                else:  # Engine-trusted reconcile entrypoint; never license or write.
                    result = {"state": package.reconciliation.inspect(3, book["id"])}
                code = None  # Library exhaustion/SDK finally, or independent media/auth facts.
        except (KeyboardInterrupt, EOFError):
            code = "canceled"
        except Exception as error:
            code = "internal"
            if package is not None:
                if isinstance(error, package.auth_store.StoreError):
                    code = "storage_uncertain" if error.published else "storage_failed"
                elif isinstance(error, package.sdk_session.ProviderError):
                    code = error.code
        try:
            if code is None:
                emit("result", **result)
            else:
                emit("error", failure={"code": code})
        except OSError:
            pass  # Closed control during shutdown: no traceback, prose or success.
