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
	echo "ingress secret tmpfs unavailable" >&2
	exit 1
fi

copy_tls_file() {
	name=$1
	target=$2
	source="/run/secrets/$name"
	if [ ! -f "$source" ] || [ -L "$source" ]; then
		echo "ingress certificate unavailable" >&2
		exit 1
	fi
	mode=$(stat -c '%a' "$source" 2>/dev/null || true)
	case "$mode" in
		*[1-7][0-7]|*[0-7][1-7]) echo "ingress certificate permissions invalid" >&2; exit 1 ;;
	esac
	cp "$source" "$target"
	chmod 0600 "$target"
	chown "$runtime_uid:$runtime_gid" "$target"
}

copy_tls_file ingress_tls_certificate /run/passport-caddy/tls.crt
copy_tls_file ingress_tls_private_key /run/passport-caddy/tls.key
chown "$runtime_uid:$runtime_gid" /run/passport-caddy
chmod 0700 /run/passport-caddy

exec su-exec "$runtime_uid:$runtime_gid" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
