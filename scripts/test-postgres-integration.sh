#!/bin/sh
set -eu

test_target=${1:-full}
case "$test_target" in
    full|enrollment|retention|backup) ;;
    *) printf '%s\n' "Usage: $0 [full|enrollment|retention|backup]" >&2; exit 2 ;;
esac
if [ "$#" -gt 1 ]; then
    printf '%s\n' "Usage: $0 [full|enrollment|retention|backup]" >&2
    exit 2
fi

fail() {
    printf '%s\n' "$1" >&2
    exit 1
}

docker_context=${PASSPORT_TEST_DOCKER_CONTEXT:-}
if [ -z "$docker_context" ]; then
    fail "Set PASSPORT_TEST_DOCKER_CONTEXT to an explicit Docker context"
fi
secret_root=${PASSPORT_TEST_SECRETS_ROOT:-}
if [ -z "$secret_root" ] || [ ! -d "$secret_root" ] || [ -L "$secret_root" ]; then
    fail "Set PASSPORT_TEST_SECRETS_ROOT to a private directory visible to the Docker daemon"
fi

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
python3 - "$secret_root" "$repo_root" <<'PY'
import os
import pathlib
import stat
import sys

root = pathlib.Path(sys.argv[1])
repo = pathlib.Path(sys.argv[2]).resolve()
resolved = root.resolve(strict=True)
try:
    resolved.relative_to(repo)
except ValueError:
    pass
else:
    raise SystemExit("PASSPORT_TEST_SECRETS_ROOT must be outside the checkout")
metadata = root.stat()
if not stat.S_ISDIR(metadata.st_mode) or metadata.st_uid != os.getuid() or metadata.st_mode & 0o077:
    raise SystemExit("PASSPORT_TEST_SECRETS_ROOT must be an owned private directory (mode 0700 or stricter)")
PY
docker_endpoint=$(docker context inspect "$docker_context" --format '{{ .Endpoints.docker.Host }}') \
    || fail "Could not inspect selected Docker context endpoint"
case "$docker_endpoint" in
    unix://*) ;;
    *) fail "File-backed secrets require a local unix-socket Docker context" ;;
esac
test_dir=$(mktemp -d "$secret_root/passport-pg-integration.XXXXXX") || fail "Cannot create private test directory"
chmod 700 "$test_dir"
project_token=$(python3 -c 'import secrets; print(secrets.token_hex(8))')
project_name="passport-pg-it-$project_token"
database_name="passport_pg_it_$project_token"
backup_source_database="${database_name}_source"
backup_target_database="${database_name}_target"
compose_override="$test_dir/compose.integration.yaml"

export PASSPORT_POSTGRES_DB="$database_name"
export PASSPORT_POSTGRES_USER=passport_test
export PASSPORT_RABBITMQ_USER=passport_test
export PASSPORT_POSTGRES_PASSWORD_FILE="$test_dir/postgres_password"
export PASSPORT_RABBITMQ_PASSWORD_FILE="$test_dir/rabbitmq_password"
export PASSPORT_POSTGRES_DSN_FILE="$test_dir/migration.dsn"
export PASSPORT_RABBITMQ_DSN_FILE="$test_dir/rabbitmq.dsn"
export PASSPORT_CLIENT_CREDENTIALS_FILE="$test_dir/client-credentials.json"
export PASSPORT_WORKER_CREDENTIALS_FILE="$test_dir/worker-credentials.json"
export PASSPORT_OCR_WORKER_TOKEN_FILE="$test_dir/ocr-worker-token"
export PASSPORT_API_CONFIG_FILE="$test_dir/api.queue.json"
export PASSPORT_OCR_CONFIG_FILE="$test_dir/ocr-worker.json"
export PASSPORT_TEST_API_ENTRYPOINT="$test_dir/api-entrypoint-diagnostic.sh"
export PASSPORT_IMAGE_ROOT="$test_dir/images"
export PASSPORT_API_STATE_ROOT="$test_dir/api-state"
export PASSPORT_RUSSIANDOCS_ROOT="$test_dir/russiandocs"
export PASSPORT_TEST_CLIENT_TOKEN_FILE="$test_dir/client-token"
export PASSPORT_TEST_SECOND_CLIENT_TOKEN_FILE="$test_dir/second-client-token"
export PASSPORT_TEST_CLIENT_OWNER_IDS_FILE="$test_dir/client-owner-ids.json"
export PASSPORT_TEST_STATIC_DB_TOKEN_FILE="$test_dir/static-db-token"
export PASSPORT_TEST_STATIC_ONLY_TOKEN_FILE="$test_dir/static-only-token"
export PASSPORT_TEST_STATIC_OWNER_IDS_FILE="$test_dir/static-owner-ids.json"
export PASSPORT_TEST_WORKER_TOKEN_FILE="$test_dir/ocr-worker-token"
export PASSPORT_TEST_STATUS_RESPONSE_FILE="/integration-images/stage5-status-responses.json"
export PASSPORT_APP_UID=$(id -u)
export PASSPORT_APP_GID=$(id -g)
export PASSPORT_TEST_IMAGE_ROOT="$PASSPORT_IMAGE_ROOT"
export PASSPORT_TEST_CONTROLLED_CONSUMER="$test_dir/controlled-consumer.py"
export PASSPORT_TEST_TARGET="$test_target"
export PASSPORT_TEST_TECHNICAL_BACKUP_SOURCE_DSN_FILE="$test_dir/technical-backup-source.dsn"
export PASSPORT_TEST_TECHNICAL_BACKUP_TARGET_DSN_FILE="$test_dir/technical-backup-target.dsn"
if [ "$test_target" = enrollment ]; then
    export PASSPORT_CLIENT_CREDENTIALS_FILE="$test_dir/static-client-credentials.json"
    export PASSPORT_WORKER_CREDENTIALS_FILE=/dev/null
    export PASSPORT_OCR_WORKER_TOKEN_FILE=/dev/null
fi
queue_worker_container="${project_name}-controlled-consumer"
api_container=

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    docker --context "$docker_context" rm --force "$queue_worker_container" >/dev/null 2>&1 || true
    if [ -n "$api_container" ]; then
        docker --context "$docker_context" rm --force "$api_container" >/dev/null 2>&1 || true
    fi
    docker --context "$docker_context" compose \
        --project-name "$project_name" \
        --file "$repo_root/compose.yaml" \
        --file "$compose_override" \
        --profile '*' \
        down --volumes --remove-orphans >/dev/null 2>&1 || true
    owned_containers=$(docker --context "$docker_context" ps --all --quiet --filter "label=com.docker.compose.project=$project_name")
    if [ -n "$owned_containers" ]; then
        docker --context "$docker_context" rm --force $owned_containers >/dev/null 2>&1 || true
    fi
    owned_networks=$(docker --context "$docker_context" network ls --quiet --filter "label=com.docker.compose.project=$project_name")
    if [ -n "$owned_networks" ]; then
        docker --context "$docker_context" network rm $owned_networks >/dev/null 2>&1 || true
    fi
    owned_volumes=$(docker --context "$docker_context" volume ls --quiet --filter "label=com.docker.compose.project=$project_name")
    if [ -n "$owned_volumes" ]; then
        docker --context "$docker_context" volume rm $owned_volumes >/dev/null 2>&1 || true
    fi
    rm -rf -- "$test_dir"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker --context "$docker_context" info --format '{{.ServerVersion}}' >/dev/null \
    || fail "Selected Docker context is unavailable"

python3 -c 'import secrets; print(secrets.token_urlsafe(48), end="")' > "$PASSPORT_POSTGRES_PASSWORD_FILE"
python3 -c 'import secrets; print(secrets.token_urlsafe(48), end="")' > "$PASSPORT_RABBITMQ_PASSWORD_FILE"
chmod 600 "$PASSPORT_POSTGRES_PASSWORD_FILE" "$PASSPORT_RABBITMQ_PASSWORD_FILE"
mkdir -m 700 "$PASSPORT_IMAGE_ROOT" "$PASSPORT_RUSSIANDOCS_ROOT"
mkdir -m 700 "$PASSPORT_API_STATE_ROOT"
mkdir -m 700 "$PASSPORT_RUSSIANDOCS_ROOT/document_processing"
mkdir -m 700 "$PASSPORT_RUSSIANDOCS_ROOT/document_processing/models"

python3 - "$test_dir" "$database_name" "$repo_root" "$backup_source_database" "$backup_target_database" <<'PY'
import base64
import hashlib
import json
import os
import pathlib
import secrets
import sys
import uuid

root = pathlib.Path(sys.argv[1])
database = sys.argv[2]
repo = pathlib.Path(sys.argv[3])
backup_source_database = sys.argv[4]
backup_target_database = sys.argv[5]
password = (root / "postgres_password").read_text()
dsn = f"postgres://passport_test:{password}@postgres:5432/{database}?sslmode=disable"
(root / "migration.dsn").write_text(dsn)
(root / "migration.dsn").chmod(0o600)
for name, database_name in (("technical-backup-source.dsn", backup_source_database), ("technical-backup-target.dsn", backup_target_database)):
    backup_dsn = f"postgres://passport_test:{password}@postgres:5432/{database_name}?sslmode=disable"
    (root / name).write_text(backup_dsn)
    (root / name).chmod(0o600)
rabbit_password = (root / "rabbitmq_password").read_text()
rabbit_dsn = f"amqp://passport_test:{rabbit_password}@rabbitmq:5672/%2f"
(root / "rabbitmq.dsn").write_text(rabbit_dsn)
(root / "rabbitmq.dsn").chmod(0o600)

def token(prefix: str) -> str:
    raw = base64.urlsafe_b64encode(secrets.token_bytes(32)).decode("ascii").rstrip("=")
    return prefix + raw

client_token = token("pbt1_")
second_client_token = token("pbt1_")
worker_token = token("pwt1_")
client_owner = str(uuid.uuid4())
second_client_owner = str(uuid.uuid4())
worker_id = str(uuid.uuid4())
static_db_token = token("pbt1_")
static_only_token = token("pbt1_")
static_db_owner = str(uuid.uuid4())
static_only_owner = str(uuid.uuid4())
(root / "client-token").write_text(client_token)
(root / "client-token").chmod(0o600)
(root / "second-client-token").write_text(second_client_token)
(root / "second-client-token").chmod(0o600)
(root / "ocr-worker-token").write_text(worker_token)
(root / "ocr-worker-token").chmod(0o600)
(root / "client-owner-ids.json").write_text(json.dumps([client_owner, second_client_owner], separators=(",", ":")))
(root / "client-owner-ids.json").chmod(0o600)
(root / "static-db-token").write_text(static_db_token)
(root / "static-db-token").chmod(0o600)
(root / "static-only-token").write_text(static_only_token)
(root / "static-only-token").chmod(0o600)
(root / "static-owner-ids.json").write_text(json.dumps([static_db_owner, static_only_owner], separators=(",", ":")))
(root / "static-owner-ids.json").chmod(0o600)
(root / "client-credentials.json").write_text(json.dumps({
    "schema_version": "passport-client-credentials/v1",
    "credentials": [
        {"owner_id": client_owner, "token_sha256": hashlib.sha256(client_token.encode()).hexdigest()},
        {"owner_id": second_client_owner, "token_sha256": hashlib.sha256(second_client_token.encode()).hexdigest()},
    ],
}, separators=(",", ":")))
(root / "client-credentials.json").chmod(0o600)
static_registry = {
    "schema_version": "passport-client-credentials/v1",
    "credentials": [
        {"owner_id": static_db_owner, "token_sha256": hashlib.sha256(static_db_token.encode()).hexdigest()},
        {"owner_id": static_only_owner, "token_sha256": hashlib.sha256(static_only_token.encode()).hexdigest()},
    ],
}
(root / "static-client-credentials.json").write_text(json.dumps(static_registry, separators=(",", ":")))
(root / "static-client-credentials.json").chmod(0o600)
(root / "worker-credentials.json").write_text(json.dumps({
    "schema_version": "passport-worker-credentials/v1",
    "credentials": [{"worker_id": worker_id, "token_sha256": hashlib.sha256(worker_token.encode()).hexdigest()}],
}, separators=(",", ":")))
(root / "worker-credentials.json").chmod(0o600)

api = json.loads((repo / "deploy" / "api.queue.example.json").read_text())
api["pipeline_version"] = "integration-client-enrollment"
api["logging"]["path"] = "/var/lib/passport/state/logs/api.jsonl"
api["metrics"]["state_path"] = "/var/lib/passport/state/metrics/state.json"
api["async_jobs"]["connect_timeout"] = "5s"
api["async_jobs"]["migration_lock_timeout"] = "10s"
api["async_jobs"]["max_stored_image_bytes"] = 1048576
if os.environ.get("PASSPORT_TEST_TARGET") == "enrollment":
    api.pop("queue_processing", None)
    api["async_jobs"]["credentials_file"] = "/run/passport-api/secrets/client_credentials"
    api["async_jobs"]["client_enrollment"] = {
        "max_clients": 20,
        "max_new_clients_per_minute": 20,
    }
elif os.environ.get("PASSPORT_TEST_STATUS_ONLY") == "1":
    api.pop("queue_processing", None)
else:
    api["queue_processing"].update({
    "connect_timeout": "3s", "poll_interval": "50ms", "publisher_lease": "2s",
    "publish_timeout": "500ms", "publish_backoff": "100ms",
    "reconnect_min": "100ms", "reconnect_max": "500ms",
    "consumer_ack_timeout": "5m", "max_attempts": 2,
    "retry_initial_delay": "100ms", "retry_max_delay": "500ms",
    "hard_processing_timeout": "5s", "completion_budget": "2s",
    "attempt_lease": "15s", "recovery_interval": "1s",
    })
(root / "api.queue.json").write_text(json.dumps(api, separators=(",", ":")))
(root / "api.queue.json").chmod(0o600)

worker = {
    "rabbit_host": "rabbitmq", "rabbit_port": 5672, "rabbit_user": "passport_test",
    "rabbit_password_file": "/run/secrets/rabbitmq_password", "rabbit_vhost": "/",
    "queue": "passport.recognition.work", "exchange": "passport.jobs",
    "routing_key": "recognition.requested", "dead_letter_exchange": "passport.jobs.dlx",
    "dead_letter_queue": "passport.recognition.dead", "go_worker_url": "http://api:8081",
    "worker_token_file": "/run/secrets/ocr_worker_token", "image_root": "/var/lib/passport/images",
    "russiandocs_repo": "/opt/russiandocs", "models_root": "/opt/russiandocs/document_processing/models",
    "work_root": "/var/lib/passport/ocr-work", "max_input_bytes": 8388608,
    "max_image_pixels": 40000000, "expected_upstream_version": "4.6.0", "expected_model_lock_version": "v7",
    "startup_timeout_seconds": 30, "processing_stop_grace_seconds": 1,
    "http_timeout_ms": 1000, "boundary_attempts": 1,
    "reconnect_backoff_seconds": 0.1, "wait_poll_max_seconds": 0.1,
    "rabbit_heartbeat_seconds": 30, "rabbit_blocked_connection_timeout_seconds": 5,
    "consumer_ack_timeout_ms": 300000,
}
(root / "ocr-worker.json").write_text(json.dumps(worker, separators=(",", ":")))
(root / "ocr-worker.json").chmod(0o600)

controlled = '''import os, signal
from pathlib import Path
from ocr.runtime import async_consumer as consumer
from ocr.runtime import secret_bootstrap

def stop(_signum, _frame):
    raise KeyboardInterrupt()

signal.signal(signal.SIGTERM, stop)
uid = int(os.environ.get("PASSPORT_WORKER_UID", "10001"))
gid = int(os.environ.get("PASSPORT_WORKER_GID", "10001"))
private_config = secret_bootstrap.prepare(Path(os.environ["PASSPORT_OCR_CONFIG_FILE"]), uid, gid)
os.setgroups([])
os.setgid(gid)
os.setuid(uid)
config = consumer.Config.load(private_config)
token_value = config.read_secret(config.worker_token_file)
client = consumer.GoWorkerClient(config.go_worker_url, token_value, config.http_timeout_ms, config.boundary_attempts)
mode = os.environ.get("PASSPORT_TEST_CONSUMER_MODE", "normal")
state = Path("/integration-test-state")
print("TEST_CONSUMER_MODE=" + mode, flush=True)

def append_private(name, value):
    descriptor = os.open(state / name, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    with os.fdopen(descriptor, "a") as output:
        output.write(value)
        output.flush()
        os.fsync(output.fileno())

class ControlledPipeline:
    def recognize(self, _request_id, _image_id, _path, _media_type, _timeout_ms):
        print("TEST_RECOGNIZE_ENTER", flush=True)
        if mode in ("crash", "guard"):
            append_private(mode + ".calls", "call\\n")
        if mode == "guard":
            raise RuntimeError("recognition_must_not_repeat")
        return {field: None for field in consumer.FIELDS}
    def close(self):
        return None

if mode == "crash":
    class CrashAfterCompleteClient(consumer.GoWorkerClient):
        def complete(self, envelope, execution, fields, completion_deadline):
            response = super().complete(envelope, execution, fields, completion_deadline)
            if response == {"decision": "ack"}:
                os._exit(73)
            return response
    client = CrashAfterCompleteClient(config.go_worker_url, token_value,
                                      config.http_timeout_ms, config.boundary_attempts)

processor = consumer.DeliveryProcessor(config, client, ControlledPipeline())
worker = consumer.RabbitWorker(config, client, processor)
if mode == "guard":
    original_delivery = worker._on_delivery
    def observe_redelivery(generation, channel, method, properties, body):
        if method.redelivered:
            append_private("guard.redelivered", "redelivered\\n")
        original_delivery(generation, channel, method, properties, body)
    worker._on_delivery = observe_redelivery
worker.run()
'''
(root / "controlled-consumer.py").write_text(controlled)
(root / "controlled-consumer.py").chmod(0o600)

entrypoint_path = root / "api-entrypoint-diagnostic.sh"
entrypoint = (repo / "deploy" / "api-entrypoint.sh").read_text()
original_exec = 'exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/passport-api -config "$app_config"\n'
diagnostic = r'''report_stat() {
    label=$1
    target=$2
    if [ -e "$target" ]; then
        stat -c "$label uid=%u gid=%g mode=%a" "$target"
    else
        printf '%s\n' "$label absent"
    fi
}
printf 'API bootstrap uid=%s gid=%s\n' "$runtime_uid" "$runtime_gid"
report_stat runtime_dir "$runtime_dir"
report_stat config_file "$app_config"
report_stat secret_dir "$secret_dir"
if cmp -s "$config_file" "$app_config"; then
    printf '%s\n' 'API bootstrap config_copy_equal=true'
else
    printf '%s\n' 'API bootstrap config_copy_equal=false'
fi
report_stat api_state_root /var/lib/passport/state
report_stat log_dir /var/lib/passport/state/logs
if su-exec "$runtime_uid:$runtime_gid" sh -c 'test -r "$1"' sh "$app_config"; then
    printf '%s\n' 'API bootstrap config_read=true'
else
    printf '%s\n' 'API bootstrap config_read=false'
fi
if su-exec "$runtime_uid:$runtime_gid" sh -c 'if [ ! -d "$1" ]; then mkdir -m 0700 "$1"; fi && probe="$1/.passport-stage4-write-probe.$$" && (umask 077; : > "$probe") && rm "$probe"' sh /var/lib/passport/state/logs; then
    printf '%s\n' 'API bootstrap log_write_probe=true'
else
    printf '%s\n' 'API bootstrap log_write_probe=false'
fi
'''
if entrypoint.count(original_exec) != 1:
    raise SystemExit("API entrypoint diagnostic fixture could not match the final exec")
entrypoint_path.write_text(entrypoint.replace(original_exec, diagnostic + original_exec))
entrypoint_path.chmod(0o700)
PY

python3 - "$test_dir" "$repo_root" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
repo = pathlib.Path(sys.argv[2])
source = root / "generated_queue_config_test.go"
source.write_text(r'''package main

import (
	"os"
	"testing"
)

func TestStageGeneratedQueueConfigParses(t *testing.T) {
	configPath := os.Getenv("PASSPORT_TEST_API_CONFIG_FILE")
	_, _, _, runtimeConfig, err := loadFullRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("generated queue runtime config rejected: %s", err.Error())
	}
	if runtimeConfig == nil || !runtimeConfig.Enabled || runtimeConfig.SyncOCREnabled {
		t.Fatal("generated runtime config did not enable durable async intake")
	}
	if (os.Getenv("PASSPORT_TEST_STATUS_ONLY") == "1" || os.Getenv("PASSPORT_TEST_TARGET") == "enrollment") && runtimeConfig.QueueProcessing != nil {
		t.Fatal("this focused runtime config unexpectedly requires queue processing")
	}
	if os.Getenv("PASSPORT_TEST_STATUS_ONLY") != "1" && os.Getenv("PASSPORT_TEST_TARGET") != "enrollment" && runtimeConfig.QueueProcessing == nil {
		t.Fatal("queue runtime config did not enable queue processing")
	}
}
''')
overlay = root / "config-overlay.json"
target = repo / "cmd" / "api" / "stage4_generated_queue_config_test.go"
overlay.write_text(json.dumps({"Replace": {str(target): str(source)}}))
overlay.chmod(0o600)
PY
PASSPORT_TEST_API_CONFIG_FILE="$PASSPORT_API_CONFIG_FILE" GOTOOLCHAIN=go1.25.0 \
    go test -overlay "$test_dir/config-overlay.json" ./cmd/api \
    -run '^TestStageGeneratedQueueConfigParses$' -count=1

cat > "$compose_override" <<'YAML'
services:
  api:
    volumes:
      - type: bind
        source: ${PASSPORT_TEST_API_ENTRYPOINT}
        target: /usr/local/bin/passport-api-entrypoint
        read_only: true
        bind:
          create_host_path: false
  postgres:
    secrets:
      - postgres_dsn
      - broker_dsn
      - integration_client_token
      - integration_second_client_token
      - integration_worker_token
      - integration_client_owner_ids
      - integration_static_db_token
      - integration_static_only_token
      - integration_static_owner_ids
    volumes:
      - integration_images:/integration-images
      - ocr_test_state:/integration-test-state
      - type: bind
        source: ${PASSPORT_TEST_IMAGE_ROOT}
        target: /integration-app-images
        read_only: true
        bind:
          create_host_path: false
  ocr:
    volumes:
      - ocr_test_state:/integration-test-state
      - type: bind
        source: ${PASSPORT_TEST_CONTROLLED_CONSUMER}
        target: /app/passport-stage4-controlled-consumer.py
        read_only: true
        bind:
          create_host_path: false
secrets:
  integration_client_token:
    file: ${PASSPORT_TEST_CLIENT_TOKEN_FILE}
  integration_second_client_token:
    file: ${PASSPORT_TEST_SECOND_CLIENT_TOKEN_FILE}
  integration_worker_token:
    file: ${PASSPORT_TEST_WORKER_TOKEN_FILE}
  integration_client_owner_ids:
    file: ${PASSPORT_TEST_CLIENT_OWNER_IDS_FILE}
  integration_static_db_token:
    file: ${PASSPORT_TEST_STATIC_DB_TOKEN_FILE}
  integration_static_only_token:
    file: ${PASSPORT_TEST_STATIC_ONLY_TOKEN_FILE}
  integration_static_owner_ids:
    file: ${PASSPORT_TEST_STATIC_OWNER_IDS_FILE}
volumes:
  integration_images:
  ocr_test_state:
YAML
chmod 600 "$compose_override"

compose() {
    docker --context "$docker_context" compose \
        --project-name "$project_name" \
        --file "$repo_root/compose.yaml" \
        --file "$compose_override" "$@"
}

diagnose_api_startup() {
    docker --context "$docker_context" inspect --format \
        'API state={{.State.Status}} exit={{.State.ExitCode}} restarts={{.RestartCount}} ip={{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$api_container" >&2
    docker --context "$docker_context" logs --tail 30 "$api_container" >&2
    if [ -f "$PASSPORT_API_STATE_ROOT/logs/api.jsonl" ]; then
        python3 - "$PASSPORT_API_STATE_ROOT/logs/api.jsonl" <<'PY' >&2
import json
import pathlib
import sys

events = []
for line in pathlib.Path(sys.argv[1]).read_text().splitlines()[-30:]:
    try:
        record = json.loads(line)
    except json.JSONDecodeError:
        continue
    event = record.get("event")
    code = record.get("error_code")
    if isinstance(event, str) and isinstance(code, str):
        events.append(event + "/" + code)
print("API private log categories=" + (",".join(events) if events else "none"))
PY
    else
        printf '%s\n' 'API persistent log file absent' >&2
    fi
}

compose config --quiet
if [ "$test_target" = enrollment ]; then
    compose up --detach --wait --build postgres rabbitmq
    compose --profile migrate run --build --rm migrate
    compose --profile queue up --detach --wait --no-deps --build api
elif [ "$test_target" = retention ]; then
    compose up --detach --wait --build postgres
    compose --profile migrate run --build --rm migrate
elif [ "$test_target" = backup ]; then
    compose up --detach --wait --build postgres
elif [ "${PASSPORT_TEST_STATUS_ONLY:-0}" = 1 ]; then
    compose up --detach --wait --build postgres
else
    compose up --detach --wait --build postgres rabbitmq
fi
if [ "${PASSPORT_TEST_API_BOOTSTRAP_ONLY:-0}" = 1 ]; then
    compose --profile migrate run --build --rm migrate
    compose --profile queue up --detach --build api
    api_container=$(compose --profile queue ps --quiet api)
    if [ -z "$api_container" ]; then
        fail "Could not identify this Compose project's API container"
    fi
    sleep 3
    diagnose_api_startup
    docker --context "$docker_context" inspect --format \
        'API restarts={{.RestartCount}}' "$api_container"
    exit 0
fi
docker_arch=$(docker --context "$docker_context" info --format '{{.Architecture}}')
case "$docker_arch" in
    aarch64|arm64) test_arch=arm64 ;;
    x86_64|amd64) test_arch=amd64 ;;
    *) fail "Unsupported Docker test architecture" ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$test_arch" go test -c -tags=integration \
    -o "$test_dir/postgres-integration.test" ./internal/jobstore/postgres
postgres_container=$(compose ps --quiet postgres)
docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
    mkdir -p /integration-images
docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
    chmod 0700 /integration-images
docker --context "$docker_context" cp "$test_dir/postgres-integration.test" \
    "$postgres_container:/tmp/passport-postgres-integration.test"
if [ "$test_target" = enrollment ]; then
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_STATIC_DB_TOKEN_FILE=/run/secrets/integration_static_db_token \
        --env PASSPORT_POSTGRES_TEST_STATIC_ONLY_TOKEN_FILE=/run/secrets/integration_static_only_token \
        --env PASSPORT_POSTGRES_TEST_STATIC_OWNER_IDS_FILE=/run/secrets/integration_static_owner_ids \
        --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresClientEnrollmentReplayAdmissionAndOwnerAuthentication$' \
        -test.count=1 -test.timeout=150s -test.v
    printf '%s\n' "PostgreSQL and composed Go API client enrollment integration checks passed for isolated Compose project $project_name"
    exit 0
fi
if [ "$test_target" = retention ]; then
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresRetentionPurgesTerminalDataAndPreservesIdempotency$' \
        -test.count=1 -test.timeout=180s -test.v
    printf '%s\n' "PostgreSQL terminal retention integration checks passed for isolated Compose project $project_name"
    exit 0
fi
if [ "$test_target" = backup ]; then
    docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
        psql -U passport_test -d "$database_name" -v ON_ERROR_STOP=1 \
        -c "CREATE DATABASE $backup_source_database" -c "CREATE DATABASE $backup_target_database" >/dev/null
    docker --context "$docker_context" cp "$PASSPORT_TEST_TECHNICAL_BACKUP_SOURCE_DSN_FILE" \
        "$postgres_container:/integration-images/technical-backup-source.dsn"
    docker --context "$docker_context" cp "$PASSPORT_TEST_TECHNICAL_BACKUP_TARGET_DSN_FILE" \
        "$postgres_container:/integration-images/technical-backup-target.dsn"
    docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
        chmod 0600 /integration-images/technical-backup-source.dsn /integration-images/technical-backup-target.dsn
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_TECHNICAL_BACKUP_SOURCE_DSN_FILE=/integration-images/technical-backup-source.dsn \
        --env PASSPORT_TECHNICAL_BACKUP_TARGET_DSN_FILE=/integration-images/technical-backup-target.dsn \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresTechnicalBackupRestoreTombstones$' \
        -test.count=1 -test.timeout=180s -test.v
    printf '%s\n' "PostgreSQL technical backup integration checks passed for isolated source/target DBs in $project_name"
    exit 0
fi
if [ "${PASSPORT_TEST_STATUS_ONLY:-0}" = 1 ]; then
    compose --profile migrate run --build --rm migrate
    compose --profile queue up --detach --no-deps --build api
    api_container=$(compose --profile queue ps --quiet api)
    if [ -z "$api_container" ]; then
        fail "Could not identify this Compose project's status API container"
    fi
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
        --env PASSPORT_POSTGRES_TEST_SECOND_CLIENT_TOKEN_FILE=/run/secrets/integration_second_client_token \
        --env PASSPORT_POSTGRES_TEST_WORKER_TOKEN_FILE=/run/secrets/integration_worker_token \
        --env PASSPORT_POSTGRES_TEST_CLIENT_OWNER_IDS_FILE=/run/secrets/integration_client_owner_ids \
        --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
        --env PASSPORT_POSTGRES_TEST_STATUS_RESPONSE_FILE=/integration-images/stage5-status-responses.json \
        --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresHTTPJobStatus' -test.count=1 -test.timeout=150s -test.v
    python3 - "$PASSPORT_API_STATE_ROOT/logs/api.jsonl" \
        "$PASSPORT_TEST_CLIENT_TOKEN_FILE" "$PASSPORT_TEST_SECOND_CLIENT_TOKEN_FILE" \
        "$PASSPORT_TEST_WORKER_TOKEN_FILE" "$PASSPORT_TEST_CLIENT_OWNER_IDS_FILE" <<'PY'
import json
import pathlib
import sys

log_path = pathlib.Path(sys.argv[1])
if not log_path.exists():
    print("API status log privacy scan: no log file produced")
    raise SystemExit(0)
contents = log_path.read_text()
private_values = []
for raw_path in sys.argv[2:5]:
    private_values.append(pathlib.Path(raw_path).read_text())
private_values.extend(json.loads(pathlib.Path(sys.argv[5]).read_text()))
if any(value and value in contents for value in private_values):
    raise SystemExit("API status log contains a test credential or owner identifier")
print("API status log privacy scan: credentials and owner identifiers absent")
PY
    docker --context "$docker_context" cp \
        "$postgres_container:/integration-images/stage5-status-responses.json" \
        "$test_dir/stage5-status-responses.json"
    python3 -m venv "$test_dir/openapi-venv"
    "$test_dir/openapi-venv/bin/python" -m pip install --disable-pip-version-check \
        openapi-spec-validator==0.9.0 openapi-schema-validator==0.9.0 PyYAML==6.0.3 \
        >"$test_dir/openapi-validator-install.log" 2>&1 || {
        cat "$test_dir/openapi-validator-install.log" >&2
        fail "Could not provision the disposable pinned OpenAPI validator environment"
    }
    "$test_dir/openapi-venv/bin/python" - "$repo_root/api/openapi.yaml" \
        "$test_dir/stage5-status-responses.json" <<'PY'
import json
import pathlib
import sys

import yaml
from openapi_schema_validator import OAS30Validator
from openapi_spec_validator import validate_spec
from jsonschema import RefResolver

class UniqueLoader(yaml.SafeLoader):
    pass

def construct_mapping(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in mapping:
            raise ValueError("duplicate YAML key")
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping

UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping)
spec = yaml.load(pathlib.Path(sys.argv[1]).read_text(), Loader=UniqueLoader)
validate_spec(spec)
schema = spec["components"]["schemas"]["JobStatusResponse"]
resolver = RefResolver.from_schema(spec)
validator = OAS30Validator(schema, resolver=resolver, format_checker=OAS30Validator.FORMAT_CHECKER)
branch_validators = [
    OAS30Validator(spec["components"]["schemas"][reference["$ref"].rsplit("/", 1)[1]],
                   resolver=resolver, format_checker=OAS30Validator.FORMAT_CHECKER)
    for reference in schema["oneOf"]
]
payloads = json.loads(pathlib.Path(sys.argv[2]).read_text())
if set(payloads) != {"queued", "queued-retry", "processing", "completed", "failed"}:
    raise SystemExit("unexpected actual GET state sample set")
for name, payload in payloads.items():
    validator.validate(payload)

queued = payloads["queued"]
processing = payloads["processing"]
completed = payloads["completed"]
failed = payloads["failed"]
invalid = [
    (dict(queued, result=completed["result"])),
    (dict(processing, error=failed["error"])),
    ({key: value for key, value in completed.items() if key != "result"}),
    (dict(failed, result=completed["result"])),
    (dict(queued, status="unknown")),
    (dict(queued, owner_id="must-not-be-public")),
]
for payload in invalid:
    matching_branches = sum(branch.is_valid(payload) for branch in branch_validators)
    if matching_branches == 1:
        raise SystemExit("invalid lifecycle/status mutation matched a JobStatusResponse branch")
print("OpenAPI 3.0.3 validated; actual GET lifecycle samples=5; invalid oneOf mutations rejected=6")
PY
    printf '%s\n' "Stage 5 real PostgreSQL/API status integration checks passed for isolated Compose project $project_name"
    exit 0
fi
if [ "${PASSPORT_TEST_EXECUTION_ONLY:-0}" = 1 ]; then
    compose --profile migrate run --build --rm migrate
    docker --context "$docker_context" exec --user postgres "$postgres_container" \
        psql -U "$PASSPORT_POSTGRES_USER" -d "$database_name" -Atc \
        "SELECT conname || '=' || pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='public.jobs'::regclass AND conname='jobs_check5'"
    execution_test_pattern='^TestPostgresExecutionLifecycle$'
    if [ "${PASSPORT_TEST_EXECUTION_FLAKE_ONLY:-0}" = 1 ]; then
        execution_test_pattern='^TestPostgresExecutionLifecycle/expired_attempt_recovery_and_late_completion_fencing$'
    fi
    if [ "${PASSPORT_TEST_TERMINAL_CLOCK_ONLY:-0}" = 1 ]; then
        execution_test_pattern='^TestPostgresTerminalTransitionsClampFutureTimestamps$'
    fi
    execution_test_count=${PASSPORT_TEST_EXECUTION_REPEAT:-1}
    case "$execution_test_count" in ''|*[!0-9]*) fail "PASSPORT_TEST_EXECUTION_REPEAT must be a positive integer" ;; esac
    if [ "$execution_test_count" -le 0 ]; then
        fail "PASSPORT_TEST_EXECUTION_REPEAT must be a positive integer"
    fi
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run "$execution_test_pattern" -test.count="$execution_test_count" -test.timeout=120s -test.v
    exit 0
fi
if [ "${PASSPORT_TEST_API_E2E_ONLY:-0}" = 1 ]; then
    compose --profile migrate run --build --rm migrate
    compose --profile queue up --detach --build api
    api_container=$(compose --profile queue ps --quiet api)
    if [ -z "$api_container" ]; then
        fail "Could not identify this Compose project's API container"
    fi
    queue_worker_id=$(compose --profile queue run --detach --no-deps \
        --name "$queue_worker_container" --entrypoint python ocr \
        /app/passport-stage4-controlled-consumer.py)
    if [ -z "$queue_worker_id" ] || \
        [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
        fail "The isolated controlled OCR consumer did not remain running"
    fi
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
        --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
        --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
        --env PASSPORT_POSTGRES_TEST_E2E_IMAGE_ROOT=/integration-app-images \
        --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresAPIQueueToControlledConsumerCompletesOneAttempt$' \
        -test.count=1 -test.timeout=90s -test.v
    exit 0
fi
if [ "${PASSPORT_TEST_ACK_CRASH_ONLY:-0}" = 1 ]; then
    compose --profile migrate run --build --rm migrate
    compose --profile queue up --detach --build api
    api_container=$(compose --profile queue ps --quiet api)
    if [ -z "$api_container" ]; then
        fail "Could not identify this Compose project's API container"
    fi
    compose --profile queue run --rm --no-deps --user 0:0 --entrypoint sh ocr \
        -c 'chown "$PASSPORT_WORKER_UID:$PASSPORT_WORKER_GID" /integration-test-state && chmod 0700 /integration-test-state'
    compose --profile queue run --rm --no-deps --user "$PASSPORT_APP_UID:$PASSPORT_APP_GID" --entrypoint sh ocr \
        -c 'stat -c "test_state_owner=%u:%g mode=%a" /integration-test-state; test -w /integration-test-state && printf "%s\\n" test_state_writable=yes || printf "%s\\n" test_state_writable=no'
    queue_worker_id=$(compose --profile queue run --detach --no-deps \
        --name "${project_name}-ack-crash" --env PASSPORT_TEST_CONSUMER_MODE=crash \
        --entrypoint python ocr /app/passport-stage4-controlled-consumer.py)
    if [ -z "$queue_worker_id" ] || \
        [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
        fail "Could not start the controlled commit-before-ACK crash consumer"
    fi
    if ! docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
        --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
        --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
        --env PASSPORT_POSTGRES_TEST_OCR_STATE=/integration-test-state \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresAPIQueueCommitBeforeAckCrash$' \
        -test.count=1 -test.timeout=90s -test.v; then
        docker --context "$docker_context" inspect --format \
            'crash_worker_state={{.State.Status}} exit={{.State.ExitCode}}' "$queue_worker_id" >&2
        docker --context "$docker_context" logs "$queue_worker_id" 2>&1 |
            rg '^TEST_(CONSUMER_MODE|RECOGNIZE_ENTER)=' >&2 || true
        fail "The isolated commit-before-ACK fixture failed; see safe PostgreSQL test state above"
    fi
    crash_status=
    attempt=0
    while [ "$attempt" -lt 100 ]; do
        crash_state=$(docker --context "$docker_context" inspect \
            --format '{{.State.Status}}' "$queue_worker_id")
        if [ "$crash_state" = exited ]; then
            crash_status=$(docker --context "$docker_context" inspect \
                --format '{{.State.ExitCode}}' "$queue_worker_id")
            break
        fi
        sleep 0.1
        attempt=$((attempt + 1))
    done
    if [ "$crash_status" != 73 ]; then
        fail "Controlled consumer did not exit at the durable-completion/pre-ACK fault point"
    fi
    queue_worker_id=$(compose --profile queue run --detach --no-deps \
        --name "${project_name}-ack-guard" --env PASSPORT_TEST_CONSUMER_MODE=guard \
        --entrypoint python ocr /app/passport-stage4-controlled-consumer.py)
    if [ -z "$queue_worker_id" ] || \
        [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
        fail "Could not start the controlled redelivery guard consumer"
    fi
    redelivery_acked=no
    attempt=0
    rabbit_container=$(compose ps --quiet rabbitmq)
    while [ "$attempt" -lt 100 ]; do
        if docker --context "$docker_context" exec --user 0:0 "$rabbit_container" \
            rabbitmqctl list_queues -p / name messages_ready messages_unacknowledged --no-table-headers |
            python3 -c 'import sys; rows=[line.split() for line in sys.stdin if line.split() and line.split()[0]=="passport.recognition.work"]; raise SystemExit(0 if rows and rows[0][1:]==["0","0"] else 1)'; then
            redelivery_acked=yes
            break
        fi
        sleep 0.1
        attempt=$((attempt + 1))
    done
    if [ "$redelivery_acked" != yes ]; then
        fail "Redelivered completed job was not ACKed from durable terminal state"
    fi
    docker --context "$docker_context" exec --user 0:0 \
        --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
        --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
        --env PASSPORT_POSTGRES_TEST_OCR_STATE=/integration-test-state \
        "$postgres_container" /tmp/passport-postgres-integration.test \
        -test.run '^TestPostgresAPIQueueRedeliveryAfterCommitDoesNotReprocess$' \
        -test.count=1 -test.timeout=45s -test.v
    docker --context "$docker_context" stop "$queue_worker_id" >/dev/null
    printf '%s\n' 'Commit-before-ACK crash, RabbitMQ redelivery, terminal DB ACK, and no reprocessing checks passed'
    exit 0
fi
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
    --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
    --env PASSPORT_POSTGRES_TEST_E2E_IMAGE_ROOT=/integration-app-images \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgres(MigrateConcurrentAndSchemaConstraints|RepositoryCreateQueued)$' \
    -test.count=1 -test.timeout=150s -test.v
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgresExecutionLifecycle$' -test.count=1 -test.timeout=120s -test.v
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgres(JobIntakeUsesRealFilesystemAndDatabase|JobIntakeConservativeErrorsAndCommitAckLoss|HTTPAsyncIntakeIsDurableOwnerScopedAndIdempotent|HTTPAsyncIntakeRejectsInvalidRequestsWithoutPersistence|HTTPAsyncIntakeConcurrentUploadsDoNotUseOCRLimiter|HTTPAsyncIntakeQuotaAndCommitAckRetry|HTTPAsyncIntakeResponseLossCanReplay)$' \
    -test.count=1 -test.timeout=150s -test.v

docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgres(OutboxRelayPublishes|OutboxRelayLeaseExpiry|QuorumQueue)' \
    -test.count=1 -test.timeout=120s -test.v

docker --context "$docker_context" exec --detach --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    "$postgres_container" sh -c \
    '/tmp/passport-postgres-integration.test -test.run "^TestPostgresOutboxRelayReconnectsAfterBrokerRestart$" -test.count=1 -test.timeout=90s -test.v > /tmp/passport-relay-reconnect.log 2>&1; echo $? > /tmp/passport-relay-reconnect.exit'
ready=no
attempt=0
while [ "$attempt" -lt 300 ]; do
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /integration-images/stage4-relay-restart-ready; then
        ready=yes
        break
    fi
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /tmp/passport-relay-reconnect.exit; then
        docker --context "$docker_context" exec "$postgres_container" \
            cat /tmp/passport-relay-reconnect.log >&2
        fail "Relay restart integration test exited before requesting RabbitMQ restart"
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$ready" != yes ]; then
    fail "Timed out waiting for the relay restart test readiness marker"
fi
rabbit_container=$(compose ps --quiet rabbitmq)
docker --context "$docker_context" restart "$rabbit_container" >/dev/null
compose up --detach --wait rabbitmq
docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
    sh -c 'umask 077; printf continue > /integration-images/stage4-relay-restart-continue'
finished=no
attempt=0
while [ "$attempt" -lt 300 ]; do
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /tmp/passport-relay-reconnect.exit; then
        finished=yes
        break
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$finished" != yes ]; then
    fail "Timed out waiting for relay reconnection acceptance"
fi
relay_status=$(docker --context "$docker_context" exec "$postgres_container" cat /tmp/passport-relay-reconnect.exit)
docker --context "$docker_context" exec "$postgres_container" cat /tmp/passport-relay-reconnect.log
if [ "$relay_status" != 0 ]; then
    fail "Relay reconnection integration test failed"
fi

compose --profile migrate run --build --rm migrate

compose restart postgres
compose up --detach --wait postgres

postgres_container=$(compose ps --quiet postgres)
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgresRowsSurviveRestart$' -test.count=1 -test.timeout=60s -test.v

docker --context "$docker_context" exec --user postgres "$postgres_container" \
    psql -U "$PASSPORT_POSTGRES_USER" -d "$database_name" -c 'TRUNCATE TABLE jobs CASCADE' >/dev/null
compose --profile queue up --detach --build api

# Restart the already-created API while RabbitMQ is stopped. Its startup check
# must degrade safely, accept a durable job, and let the background relay publish
# that same outbox event once the broker returns.
rabbit_container=$(compose ps --quiet rabbitmq)
api_container=$(compose --profile queue ps --quiet api)
if [ -z "$rabbit_container" ] || [ -z "$api_container" ]; then
    fail "Could not identify this Compose project's RabbitMQ and API containers"
fi
docker --context "$docker_context" stop "$rabbit_container" >/dev/null
docker --context "$docker_context" restart "$api_container" >/dev/null
docker --context "$docker_context" exec --detach --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
    --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    "$postgres_container" sh -c \
    '/tmp/passport-postgres-integration.test -test.run "^TestPostgresAPIIntakeStartsWhileBrokerUnavailable$" -test.count=1 -test.timeout=75s -test.v > /tmp/passport-api-outage.log 2>&1; echo $? > /tmp/passport-api-outage.exit'
ready=no
attempt=0
while [ "$attempt" -lt 300 ]; do
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /integration-images/stage4-api-outage-ready; then
        ready=yes
        break
    fi
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /tmp/passport-api-outage.exit; then
        docker --context "$docker_context" exec "$postgres_container" \
            cat /tmp/passport-api-outage.log >&2
        diagnose_api_startup
        fail "API outage integration test exited before accepting durable intake"
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$ready" != yes ]; then
    diagnose_api_startup
    fail "Timed out waiting for API outage integration readiness"
fi
compose up --detach --wait rabbitmq
docker --context "$docker_context" exec --user 0:0 "$postgres_container" \
    sh -c 'umask 077; printf continue > /integration-images/stage4-api-outage-continue'
finished=no
attempt=0
while [ "$attempt" -lt 300 ]; do
    if docker --context "$docker_context" exec "$postgres_container" \
        test -f /tmp/passport-api-outage.exit; then
        finished=yes
        break
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$finished" != yes ]; then
    fail "Timed out waiting for API broker-restoration acceptance"
fi
outage_status=$(docker --context "$docker_context" exec "$postgres_container" cat /tmp/passport-api-outage.exit)
docker --context "$docker_context" exec "$postgres_container" cat /tmp/passport-api-outage.log
if [ "$outage_status" != 0 ]; then
    fail "API durable-intake broker-outage integration test failed"
fi

queue_worker_id=$(compose --profile queue run --detach --no-deps \
    --name "$queue_worker_container" --entrypoint python ocr \
    /app/passport-stage4-controlled-consumer.py)
if [ -z "$queue_worker_id" ]; then
    fail "Could not start the isolated controlled OCR consumer"
fi
if [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
    fail "The isolated controlled OCR consumer did not remain running"
fi
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE=/run/secrets/broker_dsn \
    --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
    --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
    --env PASSPORT_POSTGRES_TEST_E2E_IMAGE_ROOT=/integration-app-images \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgresAPIQueueToControlledConsumerCompletesOneAttempt$' \
    -test.count=1 -test.timeout=90s -test.v
rabbit_container=$(compose ps --quiet rabbitmq)
ack_confirmed=no
attempt=0
while [ "$attempt" -lt 30 ]; do
    if docker --context "$docker_context" exec --user 0:0 "$rabbit_container" \
        rabbitmqctl list_queues -p / name messages_ready messages_unacknowledged --no-table-headers |
        python3 -c 'import sys; rows=[line.split() for line in sys.stdin if line.split() and line.split()[0]=="passport.recognition.work"]; raise SystemExit(0 if rows and rows[0][1:]==["0","0"] else 1)'; then
        ack_confirmed=yes
        break
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$ack_confirmed" != yes ]; then
    fail "Controlled OCR job committed, but RabbitMQ did not record its delivery ACK"
fi

# Crash a controlled consumer after Go has durably committed completion but
# before Pika can schedule basic_ack. A replacement consumer must ACK the
# redelivery from terminal DB state without invoking recognition again.
docker --context "$docker_context" stop "$queue_worker_id" >/dev/null
compose --profile queue run --rm --no-deps --user 0:0 --entrypoint sh ocr \
    -c 'chown "$PASSPORT_WORKER_UID:$PASSPORT_WORKER_GID" /integration-test-state && chmod 0700 /integration-test-state'
compose --profile queue run --rm --no-deps --user "$PASSPORT_APP_UID:$PASSPORT_APP_GID" --entrypoint sh ocr \
    -c 'stat -c "test_state_owner=%u:%g mode=%a" /integration-test-state; test -w /integration-test-state && printf "%s\\n" test_state_writable=yes || printf "%s\\n" test_state_writable=no'
crash_worker_name="${project_name}-ack-crash"
queue_worker_id=$(compose --profile queue run --detach --no-deps \
    --name "$crash_worker_name" --env PASSPORT_TEST_CONSUMER_MODE=crash \
    --entrypoint python ocr /app/passport-stage4-controlled-consumer.py)
if [ -z "$queue_worker_id" ] || \
    [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
    fail "Could not start the controlled commit-before-ACK crash consumer"
fi
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE=/run/secrets/integration_client_token \
    --env PASSPORT_POSTGRES_TEST_API_URL=http://api:8080 \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    --env PASSPORT_POSTGRES_TEST_OCR_STATE=/integration-test-state \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgresAPIQueueCommitBeforeAckCrash$' \
    -test.count=1 -test.timeout=90s -test.v
crash_status=
attempt=0
while [ "$attempt" -lt 100 ]; do
    crash_state=$(docker --context "$docker_context" inspect \
        --format '{{.State.Status}}' "$queue_worker_id")
    if [ "$crash_state" = exited ]; then
        crash_status=$(docker --context "$docker_context" inspect \
            --format '{{.State.ExitCode}}' "$queue_worker_id")
        break
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$crash_status" != 73 ]; then
    fail "Controlled consumer did not exit at the durable-completion/pre-ACK fault point"
fi
queue_worker_id=$(compose --profile queue run --detach --no-deps \
    --name "${project_name}-ack-guard" --env PASSPORT_TEST_CONSUMER_MODE=guard \
    --entrypoint python ocr /app/passport-stage4-controlled-consumer.py)
if [ -z "$queue_worker_id" ] || \
    [ "$(docker --context "$docker_context" inspect --format '{{.State.Running}}' "$queue_worker_id")" != true ]; then
    fail "Could not start the controlled redelivery guard consumer"
fi
redelivery_acked=no
attempt=0
while [ "$attempt" -lt 100 ]; do
    if docker --context "$docker_context" exec --user 0:0 "$rabbit_container" \
        rabbitmqctl list_queues -p / name messages_ready messages_unacknowledged --no-table-headers |
        python3 -c 'import sys; rows=[line.split() for line in sys.stdin if line.split() and line.split()[0]=="passport.recognition.work"]; raise SystemExit(0 if rows and rows[0][1:]==["0","0"] else 1)'; then
        redelivery_acked=yes
        break
    fi
    sleep 0.1
    attempt=$((attempt + 1))
done
if [ "$redelivery_acked" != yes ]; then
    fail "Redelivered completed job was not ACKed from durable terminal state"
fi
docker --context "$docker_context" exec --user 0:0 \
    --env PASSPORT_POSTGRES_TEST_DSN_FILE=/run/secrets/postgres_dsn \
    --env PASSPORT_POSTGRES_TEST_IMAGE_ROOT=/integration-images \
    --env PASSPORT_POSTGRES_TEST_OCR_STATE=/integration-test-state \
    "$postgres_container" /tmp/passport-postgres-integration.test \
    -test.run '^TestPostgresAPIQueueRedeliveryAfterCommitDoesNotReprocess$' \
    -test.count=1 -test.timeout=45s -test.v
docker --context "$docker_context" stop "$queue_worker_id" >/dev/null

printf '%s\n' "PostgreSQL, RabbitMQ, and controlled OCR integration checks passed for isolated Compose project $project_name"
