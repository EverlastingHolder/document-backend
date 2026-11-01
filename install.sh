#!/usr/bin/env bash
set -Eeuo pipefail

kit_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
	if ! command -v sudo >/dev/null 2>&1; then
		echo "Run this installer as root (sudo is unavailable)." >&2
		exit 2
	fi
	exec sudo -- "$kit_root/install.sh" "$@"
fi

exec /bin/bash "$kit_root/deploy/linux/install-target.sh" --kit-root "$kit_root" "$@"
