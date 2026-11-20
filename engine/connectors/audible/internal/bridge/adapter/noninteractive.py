"""Dedicated root-only library child. Host supplies trusted loader/stdio helpers.

Not a stand-alone CLI/Go entrypoint yet. No TTY, root discovery, locks or retries;
caller retains the verified root/gate and owns cancel/drain/exit before any success.
"""

import json
import os


def write_frame(control, frame):
    encoded = json.dumps(frame, ensure_ascii=True, separators=(",", ":"), allow_nan=False)
    if len(encoded.encode("utf-8")) > 4 << 20:
        raise ValueError("frame too large")
    control.write(encoded + "\n")
    control.flush()


def main(sources, load_sources, null_stdio):
    # PEP446 dup is noninheritable. SDK stdio/exec never own the protocol channel.
    with os.fdopen(os.dup(1), "w", encoding="utf-8") as control:
        operation_id, sequence, count, code, package = "invalid", 0, 0, "internal", None

        def emit(kind, **payload):
            nonlocal sequence
            sequence += 1
            write_frame(control, dict(protocol=1, operation_id=operation_id,
                                     seq=sequence, type=kind, **payload))

        try:
            os.set_inheritable(3, False)
            null_stdio((1, 2))
            package = load_sources(sources, ("doctor", "auth_store", "login", "sdk_session", "library"))
            # Request errors are distinct from trusted-source/normalization errors.
            try:
                request = package.doctor.read_request()
                operation_id = request["operation_id"]
                package.doctor.require_action(request, "library")
            except (ValueError, UnicodeError):
                code = "invalid_input"
            else:
                null_stdio((0,))
                with package.sdk_session.authenticated(3) as client:
                    for books in package.library.pages(client):
                        emit("stage", stage="library_page", books=books)
                        count += len(books)
                code = None  # Only after exhaustion AND SDK close/finally persistence.
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
                emit("result", count=count)
            else:
                emit("error", failure={"code": code})
        except OSError:
            pass  # Closed control during shutdown: no traceback, prose or success.
