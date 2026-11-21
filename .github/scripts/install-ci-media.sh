#!/usr/bin/env bash
# Ephemeral Ubuntu 24.04 CI only, not a Taletty runtime installer.
set -euo pipefail
if command -v ffmpeg >/dev/null && command -v ffprobe >/dev/null && command -v mpv >/dev/null; then
  exit 0
fi
sources=$(mktemp --suffix=.list)
trap 'rm -f "$sources"' EXIT
# Explicit invocation-local sources avoid cloud-image mirror/config differences.
# Keep Ubuntu signature verification; do not modify global apt configuration.
printf '%s\n' \
  'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] https://archive.ubuntu.com/ubuntu noble main restricted universe multiverse' \
  'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] https://archive.ubuntu.com/ubuntu noble-updates main restricted universe multiverse' \
  'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] https://security.ubuntu.com/ubuntu noble-security main restricted universe multiverse' > "$sources"
sudo apt-get -o "Dir::Etc::sourcelist=$sources" -o Dir::Etc::sourceparts=- update
sudo apt-get -o "Dir::Etc::sourcelist=$sources" -o Dir::Etc::sourceparts=- install -y --no-install-recommends ffmpeg mpv
