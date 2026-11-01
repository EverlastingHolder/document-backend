#!/bin/sh
set -eu

runtime_uid=${PASSPORT_CADDY_UID:-10001}
runtime_gid=${PASSPORT_CADDY_GID:-10001}
case "$runtime_uid" in ''|*[!0-9]*|0) echo "invalid ingress UID" >&2; exit 2 ;; esac
case "$runtime_gid" in ''|*[!0-9]*|0) echo "invalid ingress GID" >&2; exit 2 ;; esac

if [ "$(id -u)" -ne 0 ] || [ ! -d /run/passport-caddy ] || [ -L /run/passport-caddy ]; then
	echo "ingress bootstrap unavailable" >&2
	exit 1
fi
if ! awk '$2 == "/run/passport-caddy" && $3 == "tmpfs" { found = 1 } END { exit !found }' /proc/mounts; then
	echo "ingress runtime tmpfs unavailable" >&2
	exit 1
fi

chown "$runtime_uid:$runtime_gid" /run/passport-caddy
chmod 0700 /run/passport-caddy
for directory in /data /config; do
	if [ ! -d "$directory" ] || [ -L "$directory" ] || [ ! -r "$directory" ] || [ ! -w "$directory" ]; then
		echo "ingress persistent storage unavailable" >&2
		exit 1
	fi
done

exec su-exec "$runtime_uid:$runtime_gid" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
