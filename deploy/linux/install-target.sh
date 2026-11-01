#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
	cat <<'EOF'
Usage: sudo ./install.sh --domain api.example.com [--public-bind 0.0.0.0]
       [--tls-cert FILE --tls-key FILE]
       sudo ./install.sh --lan --public-bind 192.168.1.20

By default Caddy obtains and renews HTTPS certificates automatically.
Provide both certificate options to use the static TLS overlay instead.
LAN mode is explicit, HTTP-only on port 8080, and requires a private or loopback
IPv4 address assigned to this host. It does not require a domain or certificates.
EOF
}

kit_root="" domain="" public_bind="0.0.0.0" tls_cert="" tls_key="" lan_mode=0 bind_was_set=0
while (($#)); do
	case "$1" in
		--kit-root) kit_root=${2:?missing kit root}; shift 2 ;;
		--domain) domain=${2:?missing domain}; shift 2 ;;
		--public-bind) public_bind=${2:?missing public bind}; bind_was_set=1; shift 2 ;;
		--lan) lan_mode=1; shift ;;
		--tls-cert) tls_cert=${2:?missing certificate path}; shift 2 ;;
		--tls-key) tls_key=${2:?missing private key path}; shift 2 ;;
		-h|--help) usage; exit 0 ;;
		*) echo "Unknown installer option." >&2; usage >&2; exit 2 ;;
	esac
done
fail() { echo "passport install: $1" >&2; exit 1; }

[[ -n "$kit_root" && -d "$kit_root" ]] || fail "installation kit is unavailable"
[[ "$(uname -s)" == Linux ]] || fail "this installer supports Linux targets only"
if ((lan_mode)); then
	[[ -z "$domain" && -z "$tls_cert" && -z "$tls_key" ]] || fail "--lan cannot be combined with --domain or TLS certificate options"
	((bind_was_set)) || fail "--lan requires an explicit --public-bind private IPv4 address"
	[[ -f "$kit_root/deploy/linux/install_state.py" ]] || fail "installation kit is incomplete"
	python3 "$kit_root/deploy/linux/install_state.py" validate-lan-bind --address "$public_bind" || \
		fail "LAN --public-bind must be a private or loopback IPv4 address assigned to this host"
else
	[[ -n "$domain" ]] || { usage >&2; fail "--domain is required outside LAN mode"; }
	[[ "$domain" =~ ^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[A-Za-z]{2,63}$ ]] || fail "domain format is invalid"
	domain=${domain,,}
fi
if ((!lan_mode)); then
python3 - "$public_bind" <<'PY' || fail "--public-bind must be 0.0.0.0 or an IPv4 address"
import ipaddress, sys
value = sys.argv[1]
if value != "0.0.0.0":
    ipaddress.IPv4Address(value)
PY
if [[ -n "$tls_cert" || -n "$tls_key" ]]; then
	[[ -n "$tls_cert" && -n "$tls_key" ]] || fail "--tls-cert and --tls-key must be provided together"
fi
fi
runtime_root=/srv/passport-runtime

command -v python3 >/dev/null 2>&1 || fail "Python 3.9 or newer is required"
python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 9) else 1)' || fail "Python 3.9 or newer is required"
command -v docker >/dev/null 2>&1 || fail "Docker Engine is missing; install the official Docker Engine and Compose plugin first"
command -v ip >/dev/null 2>&1 || fail "iproute2 is required to avoid private network overlap"
command -v curl >/dev/null 2>&1 || fail "curl is required for the startup check"
docker compose version >/dev/null 2>&1 || fail "Docker Compose plugin is missing; install the official Compose plugin"
compose_version=$(docker compose version --short 2>/dev/null | sed 's/^v//')
[[ -n "$compose_version" && "$(printf '%s\n%s\n' 2.33.1 "$compose_version" | sort -V | head -n 1)" == 2.33.1 ]] || fail "Docker Compose 2.33.1 or newer is required for explicit egress gateway selection"
docker buildx version >/dev/null 2>&1 || fail "Docker Buildx plugin is required"
[[ -z "${DOCKER_HOST:-}" ]] || fail "installer requires the local Docker Engine, not DOCKER_HOST"
docker_context=$(docker context show 2>/dev/null || true)
[[ -n "$docker_context" ]] || fail "Docker context could not be identified"
docker_host=$(docker context inspect "$docker_context" --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
[[ "$docker_host" == unix://* ]] || fail "installer requires a local Unix-socket Docker context"
docker info >/dev/null 2>&1 || fail "Docker Engine is not reachable by this user"
platform=$(docker info --format '{{.OSType}}/{{.Architecture}}' 2>/dev/null || true)
case "$platform" in linux/amd64|linux/arm64|linux/x86_64|linux/aarch64) ;; *) fail "Docker Engine must build natively for Linux amd64 or arm64" ;; esac
engine_version=$(docker info --format '{{.ServerVersion}}' 2>/dev/null || true)
[[ -n "$engine_version" && "$(printf '%s\n%s\n' 28.0.0 "$engine_version" | sort -V | head -n 1)" == 28.0.0 ]] || fail "Docker Engine 28.0.0 or newer is required for explicit Caddy egress gateway selection"
free_kib=$(df -Pk / 2>/dev/null | awk 'NR==2 {print $4}')
[[ "$free_kib" =~ ^[0-9]+$ ]] && ((free_kib >= 8 * 1024 * 1024)) || fail "at least 8 GiB free space is required for the bundled source/model install and local image builds"
if [[ ! -d /srv ]]; then install -d -m 0755 /srv; fi
cpu_count=$(nproc 2>/dev/null || echo 0)
mem_kib=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)
if [[ "$cpu_count" =~ ^[0-9]+$ && "$mem_kib" =~ ^[0-9]+$ ]] && { ((cpu_count < 4)) || ((mem_kib < 6 * 1024 * 1024)); }; then
	echo "Notice: the measured 4-vCPU/6-GiB test host is not a guaranteed minimum or capacity promise." >&2
fi
check_public_ports=1
marker_path=$runtime_root/install.json
if [[ -e "$marker_path" || -L "$marker_path" ]]; then
	[[ -f "$marker_path" && ! -L "$marker_path" ]] || fail "existing runtime marker is not a regular file"
	marker_owner_mode=$(stat -c '%u:%a' -- "$marker_path" 2>/dev/null || true)
	[[ "$marker_owner_mode" == "0:600" ]] || fail "existing runtime marker permissions are invalid"
	check_public_ports=0
fi
if ((check_public_ports)); then
if ((lan_mode)); then
python3 - "$public_bind" <<'PY' || fail "configured LAN HTTP port 8080 is already in use"
import socket, sys
sock = socket.socket()
try:
    sock.bind((sys.argv[1], 8080))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
else
python3 - "$public_bind" <<'PY' || fail "configured host ports 80/443 are already in use"
import socket, sys
address = sys.argv[1]
for port in (80, 443):
    sock = socket.socket()
    try:
        sock.bind((address, port))
    except OSError:
        raise SystemExit(1)
    finally:
        sock.close()
PY
fi
fi
for required in compose.yaml compose.managed-tls.yaml compose.production.yaml bundle-manifest.json \
	deploy/linux/install_state.py deploy/Dockerfile.api deploy/Dockerfile.ocr deploy/Dockerfile.migrate \
	deploy/ingress/Dockerfile.managed deploy/ingress/entrypoint-managed.sh deploy/ingress/Caddyfile.managed \
	deploy/api-entrypoint.sh deploy/ocr/entrypoint.sh deploy/ingress/.dockerignore .dockerignore go.mod go.sum; do
	[[ -f "$kit_root/$required" ]] || fail "installation kit is incomplete"
done
if ((lan_mode)); then
	for required in compose.lan.yaml deploy/ingress/Dockerfile.lan deploy/ingress/Caddyfile.lan; do
		[[ -f "$kit_root/$required" ]] || fail "LAN installation kit is incomplete"
	done
fi
python3 "$kit_root/deploy/linux/install_state.py" verify-bundle --kit-root "$kit_root" || fail "bundled source or model files failed integrity validation"

if [[ -n "$tls_cert" ]]; then
	command -v openssl >/dev/null 2>&1 || fail "openssl is required to validate the supplied certificate pair"
	[[ -f "$tls_cert" && ! -L "$tls_cert" && -f "$tls_key" && ! -L "$tls_key" ]] || fail "certificate pair must be regular non-symlink files"
	tls_cert=$(realpath -- "$tls_cert")
	tls_key=$(realpath -- "$tls_key")
	cert_pub=$(openssl x509 -in "$tls_cert" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
	key_pub=$(openssl pkey -in "$tls_key" -pubout -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
	[[ -n "$cert_pub" && "$cert_pub" == "$key_pub" ]] || fail "certificate and key do not match"
fi

uid=${PASSPORT_INSTALL_UID:-10001}
gid=${PASSPORT_INSTALL_GID:-10001}
[[ "$uid" =~ ^[1-9][0-9]*$ && "$gid" =~ ^[1-9][0-9]*$ ]] || fail "runtime UID/GID must be positive integers"
state_args=(prepare --kit-root "$kit_root" --runtime-root "$runtime_root" --domain "$domain" --public-bind "$public_bind" --runtime-uid "$uid" --runtime-gid "$gid" --docker-context "$docker_context")
if ((lan_mode)); then state_args+=(--lan); fi
if [[ -n "$tls_cert" ]]; then
	state_args+=(--tls-cert "$tls_cert" --tls-key "$tls_key")
fi
python3 "$kit_root/deploy/linux/install_state.py" "${state_args[@]}" || fail "target runtime state could not be prepared safely"

compose_env=compose.env
compose_files=(-f compose.yaml -f compose.managed-tls.yaml)
if ((lan_mode)); then
	compose_env=compose.lan.env
	compose_files=(-f compose.yaml -f compose.lan.yaml)
elif [[ -n "$tls_cert" ]]; then
	compose_env=compose.static.env
	compose_files=(-f compose.yaml -f compose.production.yaml)
fi
compose=(env -i "PATH=$PATH" "HOME=${HOME:-/root}" "DOCKER_CONFIG=${DOCKER_CONFIG:-${HOME:-/root}/.docker}" docker --context "$docker_context" compose --project-name passport-backend --env-file "$runtime_root/$compose_env" "${compose_files[@]}")

cd "$kit_root"
"${compose[@]}" --profile queue --profile migrate config --quiet || fail "Compose configuration validation failed"
"${compose[@]}" --profile queue build --pull api ocr caddy
"${compose[@]}" --profile migrate build --pull migrate

# Config-only checks run before database startup. They do not start OCR or bind
# the API listener; the OCR entrypoint still applies its real secret bootstrap.
preflight_override="$runtime_root/preflight-no-network.yaml"
cat > "$preflight_override" <<'YAML'
services:
  api:
    network_mode: none
    networks: !reset []
  ocr:
    network_mode: none
    networks: !reset []
YAML
chmod 0600 "$preflight_override"
preflight_compose=("${compose[@]}" -f "$preflight_override")
"${preflight_compose[@]}" --profile queue config --quiet || fail "No-network config-check Compose validation failed"
"${preflight_compose[@]}" --profile queue run --rm --no-deps api --check-config
"${preflight_compose[@]}" --profile queue run --rm --no-deps ocr --check-config

"${compose[@]}" up -d postgres rabbitmq
wait_healthy() {
	local service=$1 timeout_seconds=$2 container health
	for ((elapsed = 0; elapsed < timeout_seconds; elapsed += 2)); do
		container=$("${compose[@]}" --profile queue ps -q "$service")
		if [[ -n "$container" ]]; then
			health=$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || true)
			[[ "$health" == healthy ]] && return 0
		fi
		sleep 2
	done
	return 1
}
wait_healthy postgres 180 || fail "PostgreSQL did not become healthy"
wait_healthy rabbitmq 300 || fail "RabbitMQ did not become healthy"
"${compose[@]}" --profile migrate run --rm migrate
"${compose[@]}" --profile queue up -d api ocr caddy
wait_healthy api 300 || fail "API readiness did not become healthy"
wait_healthy ocr 1200 || fail "OCR worker readiness handshake did not become healthy"
wait_healthy caddy 300 || fail "Caddy process did not become healthy"

status=""
probe_address=$public_bind
if ((lan_mode)); then
	for _ in $(seq 1 120); do
		status=$(curl --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 3 --max-time 5 \
			"http://$probe_address:8080/v1/passport/jobs/00000000-0000-4000-8000-000000000000" 2>/dev/null || true)
		[[ "$status" == 401 ]] && break
		sleep 5
	done
	[[ "$status" == 401 ]] || fail "LAN HTTP ingress did not reach the API auth boundary"
else
	[[ "$probe_address" == "0.0.0.0" ]] && probe_address=127.0.0.1
	curl_tls_args=()
	if [[ -n "$tls_cert" ]]; then curl_tls_args+=(--cacert "$tls_cert"); fi
	for _ in $(seq 1 120); do
		status=$(curl "${curl_tls_args[@]}" --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 3 --max-time 5 \
			--resolve "$domain:443:$probe_address" "https://$domain/v1/passport/jobs/00000000-0000-4000-8000-000000000000" 2>/dev/null || true)
		[[ "$status" == 401 ]] && break
		sleep 5
	done
	[[ "$status" == 401 ]] || fail "TLS ingress did not reach the API auth boundary; correct DNS/firewall and rerun"
fi
python3 "$kit_root/deploy/linux/install_state.py" mark-complete --runtime-root "$runtime_root" || fail "install marker could not be finalized"
if ((lan_mode)); then
	echo "Passport backend LAN mode is running at http://$public_bind:8080. HTTP is unencrypted; keep access on a trusted LAN/firewall."
else
	echo "Passport backend is running for $domain. Use the runtime env file and Compose files shown in deploy/linux/README.md for status and stop commands."
fi
