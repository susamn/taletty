"""One synchronous authenticated operation; host holds the root gate until return.

Dedicated child only: host isolates OS stdio and keeps protocol on a separate FD.
No locks, root discovery, retries, pagination or kill-time finally guarantee here.
"""

from contextlib import contextmanager, redirect_stderr, redirect_stdout
import json
import os

from . import auth_store, doctor, login

_FAILURES = {
    "not_authenticated": ("Sign in to Audible first.", "sign_in"),
    "runtime_missing": ("The Audible SDK is missing.", "none"),
    "runtime_incompatible": ("The provider runtime is incompatible.", "none"),
    "network": ("Audible could not be reached.", "user_retry"),
    "throttled": ("Audible requested a pause before retrying.", "wait"),
    "entitlement_denied": ("Audible did not grant access.", "check_entitlement"),
    "canceled": ("Operation canceled.", "none"),
    "internal": ("Audible operation failed.", "none"),
    "protocol_error": ("Audible library response was invalid.", "none"),
    "unsupported": ("Audible marketplace is unsupported.", "none"),
}


class ProviderError(Exception):
    def __init__(self, code):
        message, self.retry = _FAILURES[code]
        super().__init__(message)
        self.code = code


def _failure(error, sdk):
    if isinstance(error, ProviderError):
        return error
    if isinstance(error, KeyboardInterrupt):
        return ProviderError("canceled")
    if isinstance(error, sdk.exceptions.RatelimitError):
        return ProviderError("throttled")
    if isinstance(error, (sdk.exceptions.Unauthorized, sdk.exceptions.AuthFlowError, sdk.exceptions.NoRefreshToken)):
        return ProviderError("not_authenticated")
    if isinstance(error, (sdk.exceptions.NetworkError, sdk.exceptions.NotResponding)):
        return ProviderError("network")
    return ProviderError("internal")


def _persist(root_fd, auth, sdk):
    try:
        payload = login.snapshot(auth, sdk)
    except Exception:
        raise auth_store.StoreError() from None
    auth_store.save(root_fd, payload)


@contextmanager
def authenticated(root_fd):
    """Yield the pinned SDK Client, close it, then persist even on provider failure.

    Storage failure takes precedence over provider failure; published=True means
    do not assume rollback. Keep the borrowed root/gate alive through this exit.
    Emit machine frames via the host's separate control FD, not SDK stdout.
    """
    with open(os.devnull, "w") as sink, redirect_stdout(sink), redirect_stderr(sink):
        try:
            payload = auth_store.load(root_fd)
        except auth_store.MissingAuth:
            raise ProviderError("not_authenticated") from None
        try:
            os.set_inheritable(root_fd, False)
        except OSError:
            raise auth_store.StoreError() from None
        state = doctor.python_status()
        if state == doctor.READY:
            state = doctor.sdk_status()
        if state != doctor.READY:
            raise ProviderError(state)
        import audible
        try:
            data = json.loads(payload.decode("utf-8"), object_pairs_hook=doctor.unique_object,
                              parse_constant=doctor.reject_constant)
            auth = audible.Authenticator.from_dict(data)
            login._validate(auth)
        except Exception:
            raise ProviderError("not_authenticated") from None
        failure, client = None, None
        try:
            try:
                client = audible.Client(auth=auth)
                yield client
            except (Exception, KeyboardInterrupt) as error:
                failure = _failure(error, audible)
            finally:
                if client is not None:
                    try:
                        client.close()
                    except Exception:
                        if failure is None:
                            failure = ProviderError("internal")
        finally:
            _persist(root_fd, auth, audible)
        if failure is not None:
            raise failure from None
