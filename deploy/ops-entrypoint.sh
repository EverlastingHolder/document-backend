#!/bin/sh
set -eu

runtime_uid=${PASSPORT_OPS_UID:-10001}
runtime_gid=${PASSPORT_OPS_GID:-10001}
case "$runtime_uid" in ''|*[!0-9]*|0) echo "invalid ops UID" >&2; exit 2 ;; esac
case "$runtime_gid" in ''|*[!0-9]*|0) echo "invalid ops GID" >&2; exit 2 ;; esac
if [ "$(id -u)" -ne 0 ] || [ ! -d /run/passport-ops ] || [ -L /run/passport-ops ]; then
	echo "ops bootstrap unavailable" >&2
	exit 1
fi
if ! awk '$2 == "/run/passport-ops" && $3 == "tmpfs" { found = 1 } END { exit !found }' /proc/mounts; then
	echo "ops secret tmpfs unavailable" >&2
	exit 1
fi
if [ "$#" -lt 1 ]; then echo "ops command required" >&2; exit 2; fi
binary=$1
shift
case "$binary" in
	client-admin|passport-ops|job-maintenance|technical-backup) ;;
	*) echo "unsupported ops command" >&2; exit 2 ;;
esac
chown "$runtime_uid:$runtime_gid" /run/passport-ops
chmod 0700 /run/passport-ops

# Key generation does not need a database or any input secret.
if [ "$binary" = technical-backup ] && [ "${1:-}" = keygen ]; then
	exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/technical-backup "$@"
fi

if [ ! -f /run/secrets/postgres_dsn ] || [ -L /run/secrets/postgres_dsn ]; then
	echo "ops database secret unavailable" >&2
	exit 1
fi
mode=$(stat -c '%a' /run/secrets/postgres_dsn 2>/dev/null || true)
case "$mode" in *[1-7][0-7]|*[0-7][1-7]) echo "ops database secret permissions invalid" >&2; exit 1 ;; esac
cp /run/secrets/postgres_dsn /run/passport-ops/postgres_dsn
chmod 0600 /run/passport-ops/postgres_dsn
chown "$runtime_uid:$runtime_gid" /run/passport-ops/postgres_dsn

if [ "$binary" = client-admin ] && [ "${1:-}" = revoke-hash ]; then
	if [ ! -f /run/secrets/revocation_hash ] || [ -L /run/secrets/revocation_hash ]; then
		echo "revocation hash secret unavailable" >&2
		exit 1
	fi
	mode=$(stat -c '%a' /run/secrets/revocation_hash 2>/dev/null || true)
	case "$mode" in *[1-7][0-7]|*[0-7][1-7]) echo "revocation hash permissions invalid" >&2; exit 1 ;; esac
	cp /run/secrets/revocation_hash /run/passport-ops/token_hash
	chmod 0600 /run/passport-ops/token_hash
	chown "$runtime_uid:$runtime_gid" /run/passport-ops/token_hash
fi

if [ "$binary" = technical-backup ] && { [ "${1:-}" = create ] || [ "${1:-}" = restore ]; }; then
	if [ ! -f /run/secrets/backup_key ] || [ -L /run/secrets/backup_key ]; then
		echo "backup key secret unavailable" >&2
		exit 1
	fi
	mode=$(stat -c '%a' /run/secrets/backup_key 2>/dev/null || true)
	case "$mode" in *[1-7][0-7]|*[0-7][1-7]) echo "backup key permissions invalid" >&2; exit 1 ;; esac
	cp /run/secrets/backup_key /run/passport-ops/backup_key
	chmod 0600 /run/passport-ops/backup_key
	chown "$runtime_uid:$runtime_gid" /run/passport-ops/backup_key
	if [ "$1" = restore ] && [ "${PASSPORT_TECHNICAL_BACKUP_API_STOPPED:-false}" != true ]; then
		echo "restore requires explicit API-stop acknowledgement" >&2
		exit 2
	fi
fi

export PASSPORT_MAINTENANCE_DSN_FILE=/run/passport-ops/postgres_dsn
exec su-exec "$runtime_uid:$runtime_gid" "/usr/local/bin/$binary" "$@"
