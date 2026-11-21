"""Private licensed transfer; dedicated RunRoot child, caller retains root/gate.

No public action, lock, retry, path discovery or kill-time cleanup promise. Outcomes
contain facts, not exceptions/provider material. Auth precedence and media commit
are independent: future public callers must not turn an auth failure into rollback.
"""
from dataclasses import dataclass
import selectors

from . import auth_store, licensing, media_store, remux, sdk_session


@dataclass
class Outcome:
    ready: bool = False
    reused: bool = False
    published: bool | None = False
    media_code: str | None = None
    auth_code: str | None = None
    # Only a fresh, validated publication can retain provider structural IDs.
    # Reuse/restart cannot recover hierarchy from flattened container chapters.
    chapters: list | None = None


def _canceled(cancel_fd):
    if cancel_fd is None:
        return False
    with selectors.DefaultSelector() as selector:
        selector.register(cancel_fd, selectors.EVENT_READ)
        return bool(selector.select(0))


def _cancel(cancel_fd):
    if _canceled(cancel_fd):
        raise sdk_session.ProviderError("canceled")


class _EmitError(Exception):
    pass


def _emit(emit, kind, **payload):
    if emit is not None:
        try:
            emit(kind, **payload)
        except Exception as error:
            raise _EmitError() from error


def _transfer(plan, source, cancel_fd, emit=None):
    # signed_stream sanitizes consumer exceptions as network failures. Capture
    # storage/cancellation/emit errors inside its context so their meaning survives.
    failure = None
    emit_error = None
    received = 0
    with licensing.signed_stream(plan) as chunks:
        total = getattr(plan, "_total_bytes", None)
        for chunk in chunks:
            try:
                _cancel(cancel_fd)
                if not chunk:
                    continue
                source.write(chunk)
                received += len(chunk)
                if emit is not None:
                    payload = {"bytes": received}
                    if total is not None and total >= received:
                        payload["total"] = total
                    _emit(emit, "progress", **payload)
            except media_store.MediaStoreError:
                failure = "storage_failed"
                break
            except sdk_session.ProviderError as error:
                failure = error.code
                break
            except _EmitError as error:
                emit_error = error
                break
    if emit_error is not None:
        raise emit_error
    if failure == "storage_failed":
        raise media_store.MediaStoreError()
    if failure is not None:
        raise sdk_session.ProviderError(failure)


def _compose(store, plan, asin, outcome, cancel_fd, emit=None):
    with store.partial() as source, store.partial() as output:
        _cancel(cancel_fd)
        _emit(emit, "stage", stage="downloading")
        _transfer(plan, source, cancel_fd, emit)
        _cancel(cancel_fd)
        _emit(emit, "stage", stage="preparing")
        tree = remux.copy(source, output, store, plan._chapter_info, plan, cancel_fd)
        _cancel(cancel_fd)
        _emit(emit, "stage", stage="publishing")
        outcome.reused = not store.publish(output, asin, remux.validator(tree, cancel_fd))
        # No cancellation check after the commit point. Publication wins.
        outcome.ready = True
        if not outcome.reused:
            outcome.chapters = tree or None


def run(root_fd, asin, cancel_fd=None, emit=None):
    """Return safe independent auth/media facts after ordinary owned cleanup.

    published describes this operation's store commit exactly (including None),
    not existence. ready also includes independently verified prior/concurrent
    media. media_code storage_uncertain must not be treated as a durable success;
    auth_code storage_uncertain concerns auth only. Caller still owns clean child
    exit, group cleanup and reconciliation after abrupt death before public use.
    """
    outcome, store = Outcome(), None
    try:
        media_store._leaf(asin)
    except media_store.MediaStoreError:
        outcome.media_code = "invalid_input"
        return outcome
    try:
        with media_store.opened(root_fd) as store:
            _cancel(cancel_fd)
            if store.ready(asin, remux.validator(cancel_fd=cancel_fd)):
                outcome.ready = outcome.reused = True
            else:
                _cancel(cancel_fd)
                _emit(emit, "stage", stage="licensing")
                try:
                    with licensing.entitled(root_fd, asin, prepare_chapters=True) as plan:
                        try:
                            _compose(store, plan, asin, outcome, cancel_fd, emit)
                        except media_store.MediaStoreError as error:
                            outcome.published = error.published
                            outcome.media_code = ("storage_uncertain" if error.published is not False else
                                                  "conflict" if error.conflict else
                                                  "canceled" if _canceled(cancel_fd) else "storage_failed")
                        except (remux.RemuxError, sdk_session.ProviderError) as error:
                            outcome.media_code = error.code
                        except KeyboardInterrupt:
                            outcome.media_code = "canceled"
                except auth_store.StoreError as error:
                    outcome.auth_code = "storage_uncertain" if error.published else "storage_failed"
                except (licensing.LicenseError, sdk_session.ProviderError) as error:
                    # SDK finally preserves ordinary safe provider failure, but
                    # must not erase a media failure already recorded above.
                    if outcome.media_code is None:
                        outcome.media_code = error.code
    except media_store.MediaStoreError as error:
        outcome.published = error.published
        outcome.media_code = ("storage_uncertain" if error.published is not False else
                              "canceled" if _canceled(cancel_fd) else "storage_failed")
    except (sdk_session.ProviderError, remux.RemuxError) as error:
        outcome.media_code = error.code
    except KeyboardInterrupt:
        outcome.media_code = "canceled"
    except Exception:
        outcome.media_code = "internal"
    if store is not None:
        # Also covers SIGINT between publish returning and Python assigning ready,
        # plus partial/directory close faults after an irrevocable commit.
        if store.published is not False:
            outcome.published = store.published
        if outcome.published is True:
            outcome.ready = True
            if outcome.media_code == "canceled":
                outcome.media_code = None
    return outcome
