"""Private, ephemeral entitled media boundary; never serialize a plan.

Caller owns the authenticated root gate/process. No retries or media publication.
URLs come only from the authenticated provider response, not arbitrary user input.
"""
from contextlib import contextmanager
import ipaddress
import re
from urllib.parse import urlsplit

import httpx

from . import sdk_session


class LicenseError(Exception):
    """Fixed codes only; never retain provider exceptions or response objects."""
    def __init__(self, code):
        self.code = code
        self.retry = {"entitlement_denied": "check_entitlement", "throttled": "wait",
                      "network": "user_retry"}.get(code, "none")
        super().__init__("Private media operation failed (" + code + ").")


class _Plan:
    __slots__ = ("_url", "_key", "_iv", "_chapter_info")

    def __init__(self, url, key, iv):
        self._url, self._key, self._iv = url, key, iv
        self._chapter_info = None

    def __repr__(self):
        return "<private media plan>"

    def __reduce_ex__(self, protocol):
        raise TypeError("Private media plans cannot be serialized.")

    def clear(self):
        # Drop references, not a promise of zeroizing Python/SDK immutable strings.
        self._url = self._key = self._iv = self._chapter_info = None


def _url(value):
    if not isinstance(value, str) or not 1 <= len(value) <= 16384 or any(ord(c) <= 32 or ord(c) >= 127 for c in value):
        raise ValueError()
    parsed = urlsplit(value)
    host = parsed.hostname
    if (parsed.scheme != "https" or not host or parsed.username is not None
            or parsed.password is not None or parsed.fragment or parsed.port not in (None, 443)
            or "\\" in value or not re.fullmatch(r"[a-zA-Z0-9.-]+", host)
            or "." not in host or host.endswith(".") or host.endswith(".localhost")):
        raise ValueError()
    try:
        ipaddress.ip_address(host)
    except ValueError:
        return value
    raise ValueError()


def _request(client, asin, prepare_chapters=False):
    from audible.aescipher import decrypt_voucher_from_licenserequest
    response = client.post("content/" + asin + "/licenserequest", body={
        "supported_drm_types": ["Mpeg", "Adrm"], "quality": "High",
        "consumption_type": "Download", "response_groups": "content_reference,chapter_info",
    })
    license_ = response["content_license"]
    if license_.get("status_code") == "Denied":
        reasons = license_.get("license_denial_reasons") or []
        code = "throttled" if any(isinstance(r, dict) and r.get("rejectionReason") == "CustomerThrottled"
                                  for r in reasons) else "entitlement_denied"
        raise LicenseError(code)
    # Do not infer entitlement from library membership or an unknown status.
    if license_["status_code"] != "Granted" or license_["asin"] != asin:
        raise ValueError()
    url = _url(license_["content_metadata"]["content_url"]["offline_url"])
    key = iv = None
    if license_.get("license_response"):
        voucher = decrypt_voucher_from_licenserequest(client.auth, response)
        key, iv = voucher["key"], voucher["iv"]
        if not all(isinstance(v, str) and re.fullmatch(r"[a-fA-F0-9]{32}", v) for v in (key, iv)):
            raise ValueError()
    info = None
    if prepare_chapters:
        info = license_["content_metadata"].get("chapter_info")
        if not info:
            try:
                extra = client.get("content/" + asin + "/metadata", response_groups="chapter_info",
                                   quality="High", drm_type="Adrm", chapter_titles_type="Tree")
                info = extra.get("content_metadata", {}).get("chapter_info")
            except Exception:
                # Prototype policy: unavailable optional endpoint retains embedded chapters.
                info = None
    plan = _Plan(url, key, iv)
    plan._chapter_info = info
    return plan


@contextmanager
def entitled(root_fd, asin, *, prepare_chapters=False):
    """Borrow one SDK session and erase plan references before finally persistence."""
    if not isinstance(asin, str) or re.fullmatch(r"[A-Za-z0-9]{10}", asin) is None:
        raise LicenseError("invalid_input")
    with sdk_session.authenticated(root_fd) as client:
        plan, code = None, None
        try:
            plan = _request(client, asin, prepare_chapters)
        except LicenseError as error:
            code = error.code
        except Exception as error:
            # Preserve SDK's safe refusal/network mapping, never its text/cause.
            import audible
            code = sdk_session._failure(error, audible).code
            if isinstance(error, (ValueError, KeyError, TypeError, AttributeError)):
                code = "protocol_error"
        if code is not None:
            raise sdk_session.ProviderError(code)
        try:
            yield plan
        finally:
            plan.clear()


@contextmanager
def signed_stream(plan):
    """Fresh unauthenticated transport, no ambient proxies, cookies or redirects.

    All redirects are refused rather than guessing a CDN host allowlist. HTTPS
    DNS/PKI and the authenticated provider's URL are trusted; this is not a general
    SSRF-safe URL fetcher. Never call with user-supplied URLs. Caller consumes bytes
    only while inside this context; response/client are closed on every exit.
    """
    failure = None
    try:
        url = _url(plan._url)
        with httpx.Client(auth=None, trust_env=False, follow_redirects=False, timeout=120) as client:
            with client.stream("GET", url) as response:
                if response.status_code != 200:
                    raise LicenseError("throttled" if response.status_code == 429 else "network")
                yield response.iter_bytes(1024 * 1024)
    except KeyboardInterrupt:
        failure = "canceled"
    except LicenseError as error:
        failure = error.code
    except Exception:
        failure = "network"
    if failure is not None:
        raise sdk_session.ProviderError(failure) from None
