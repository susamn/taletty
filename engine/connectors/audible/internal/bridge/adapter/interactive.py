"""Fileless dedicated-child launcher. Control output is NOT SDK stdio or TTY input."""

import importlib
import json
import os
import sys
import types


def load_sources(sources):
    # Only engine-embedded sources, never cwd/PYTHONPATH or persisted scripts.
    package = types.ModuleType("_taletty_login")
    package.__path__ = []
    sys.modules[package.__name__] = package
    for name in ("doctor", "auth_store", "login", "sdk_session"):
        module = types.ModuleType(package.__name__ + "." + name)
        module.__package__ = package.__name__
        sys.modules[module.__name__] = module
        setattr(package, name, module)
        exec(compile(sources[name], "<embedded-" + name + ">", "exec"), module.__dict__)
    return package


def null_stdio(descriptors):
    with open(os.devnull, "r+") as sink:
        for fd in descriptors:
            os.dup2(sink.fileno(), fd)


def main(sources):
    # PEP 446: dup creates a noninheritable descriptor. No SDK/browser process
    # receives control output, root or terminal capabilities across another exec.
    with os.fdopen(os.dup(1), "w", encoding="utf-8") as control:
        operation_id, code, completed = "invalid", "internal", False
        package = None
        try:
            for fd in (3, 4):
                os.set_inheritable(fd, False)
            null_stdio((1, 2))
            package = load_sources(sources)
            request = package.doctor.read_request()
            operation_id = request["operation_id"]
            package.doctor.require_action(request, "login")
            null_stdio((0,))
            package.login.login(3, 4)
            # Independent reload after publication, not merely SDK callback success.
            auth = importlib.import_module("audible").Authenticator.from_dict(
                json.loads(package.auth_store.load(3)))
            package.login._validate(auth)
            completed = True
        except (KeyboardInterrupt, EOFError):
            code = "canceled"
        except (ValueError, UnicodeError):
            code = "invalid_input"
        except Exception as error:
            if package is not None and isinstance(error, package.login.LoginError):
                code = error.code
        frame = dict(protocol=1, operation_id=operation_id, seq=1,
                     type="result" if completed else "error")
        if completed:
            frame["status"] = "signed_in"
        else:
            frame["failure"] = {"code": code}
        try:
            control.write(json.dumps(frame, separators=(",", ":")) + "\n")
            control.flush()
        except OSError:
            pass  # Closed control pipe during cancellation: no traceback/prose.
