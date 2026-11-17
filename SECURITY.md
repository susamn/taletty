# Security policy

Taletty is currently planning-stage software. No release is supported yet.

Do not post credentials, final OAuth redirect URLs, auth files, signed media URLs,
content licenses, decryption keys, customer identifiers, or private downloads in
issues, pull requests, screenshots, logs or CI artifacts. Redact before sharing.
Do not use a live Audible account in public automated tests.

Report sensitive findings privately using GitHub private vulnerability reporting
when available; otherwise contact the repository owner privately before sharing
reproduction material. Do not open a public issue containing secrets.

## Implementation requirements

- Private directories (0700) and files (0600), atomic publication, safe path handling.
- No credentials in source, fixtures, environment dumps or process diagnostics.
- Signed media requests must not inherit Amazon authorization headers.
- Preserve sanitized provider-denial handling and auth-refresh persistence.
- Bound subprocess lifetime, cancel download/conversion process groups, reap children.
- User data belongs under `~/.config/taletty/`; never discover/import other apps' data.
- Taletty-owned schema upgrades preserve data; destructive actions confirm and preserve history.
- Unofficial provider integration may fail despite successful login. Respect refusal,
  entitlement and rate limiting; never promise to bypass DRM/account restrictions.

The prototype passes ffmpeg content keys as process arguments. Extraction work must
record this inherited limitation and investigate safer interfaces without logging
commands or claiming the risk has been eliminated. Tokens/audio are not encrypted
at rest; document disk-encryption recommendations and local device-registration
semantics. Removing local auth is not remote device revocation.
