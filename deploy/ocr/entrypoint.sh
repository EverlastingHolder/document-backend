#!/bin/sh
set -eu

config_file=${PASSPORT_OCR_CONFIG_FILE:-/run/config/ocr-worker.json}
if [ ! -f "$config_file" ]; then
    echo "worker_config_unavailable" >&2
    exit 2
fi

exec python /app/ocr/runtime/secret_bootstrap.py "$config_file" "$@"
