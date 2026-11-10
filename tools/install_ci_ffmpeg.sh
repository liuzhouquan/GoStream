#!/usr/bin/env bash
# Only for disposable GitHub-hosted Ubuntu runners, never a developer machine.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" ]]; then
  echo 'Refusing to modify apt sources outside GitHub Actions' >&2
  exit 1
fi

# The Azure HTTP mirror stalled downloading FFmpeg dependencies in run
# 42813554279. Keep Ubuntu signing checks and suites; change only that origin.
for source in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list; do
  if [[ -f "$source" ]]; then
    sudo sed -i \
      -e 's|http://azure.archive.ubuntu.com/ubuntu|https://archive.ubuntu.com/ubuntu|g' \
      -e 's|https://azure.archive.ubuntu.com/ubuntu|https://archive.ubuntu.com/ubuntu|g' \
      "$source"
  fi
done

if ! command -v ffmpeg >/dev/null || ! command -v ffprobe >/dev/null; then
  apt_options=(-o Acquire::Retries=1 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15)
  echo 'Updating Ubuntu package metadata (90s budget)'
  timeout 90s sudo apt-get "${apt_options[@]}" update
  echo 'Installing real FFmpeg without optional recommendations (240s budget)'
  timeout 240s sudo apt-get "${apt_options[@]}" install -y --no-install-recommends ffmpeg
fi
ffmpeg -version >/dev/null
ffprobe -version >/dev/null
