#!/bin/sh
set -eu

runtime_uid=${PASSPORT_APP_UID:-10001}
runtime_gid=${PASSPORT_APP_GID:-10001}
case "$runtime_uid" in ''|*[!0-9]*|0) echo "invalid runtime UID" >&2; exit 2 ;; esac
case "$runtime_gid" in ''|*[!0-9]*|0) echo "invalid runtime GID" >&2; exit 2 ;; esac

if [ "$(id -u)" -ne 0 ]; then
    echo "API entrypoint requires root bootstrap privileges" >&2
    exit 2
fi

if [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != "--check-config" ]; }; then
    echo "unsupported API entrypoint argument" >&2
    exit 2
fi

runtime_dir=/run/passport-api
secret_dir="$runtime_dir/secrets"
config_file=/run/config/api.queue.json
if [ ! -d "$runtime_dir" ] || [ -L "$runtime_dir" ] || [ ! -f "$config_file" ] || [ -L "$config_file" ] || [ ! -r "$config_file" ]; then
    echo "API runtime files unavailable" >&2
    exit 1
fi
config_size=$(wc -c < "$config_file")
case "$config_size" in ''|*[!0-9]*) echo "API runtime files unavailable" >&2; exit 1 ;; esac
if [ "$config_size" -gt 65536 ]; then
    echo "API runtime files unavailable" >&2
    exit 1
fi
if ! awk '$2 == "/run/passport-api" && $3 == "tmpfs" { found = 1 } END { exit !found }' /proc/mounts; then
    echo "API secret tmpfs unavailable" >&2
    exit 1
fi

chmod 0700 "$runtime_dir"
chown "$runtime_uid:$runtime_gid" "$runtime_dir"
mkdir -m 0700 "$secret_dir"
chown "$runtime_uid:$runtime_gid" "$secret_dir"

copy_secret() {
    name=$1
    max_bytes=$2
    source="/run/secrets/$name"
    target="$secret_dir/$name"
    if [ ! -f "$source" ] || [ -L "$source" ]; then
        echo "required API secret unavailable" >&2
        exit 1
    fi
    size=$(wc -c < "$source")
    case "$size" in ''|*[!0-9]*) echo "required API secret unavailable" >&2; exit 1 ;; esac
    if [ "$size" -le 0 ] || [ "$size" -gt "$max_bytes" ]; then
        echo "required API secret unavailable" >&2
        exit 1
    fi
    cp "$source" "$target"
    chmod 0600 "$target"
    chown "$runtime_uid:$runtime_gid" "$target"
}

copy_secret postgres_dsn 16384
if grep -Eq '"credentials_file"[[:space:]]*:[[:space:]]*"/run/passport-api/secrets/client_credentials"' "$config_file"; then
    copy_secret client_credentials 65536
fi
if grep -Eq '"queue_processing"[[:space:]]*:' "$config_file"; then
    copy_secret broker_dsn 16384
    copy_secret worker_credentials 65536
fi

app_config="$runtime_dir/api.json"
cp "$config_file" "$app_config"
chmod 0600 "$app_config"
chown "$runtime_uid:$runtime_gid" "$app_config"

exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/passport-api -config "$app_config" "$@"
