#!/bin/sh
set -eu
umask 077
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
ctx=$PASSPORT_STAGE7_DOCKER_CONTEXT
tmp=$PASSPORT_STAGE7_TMP_ROOT
case "$ctx" in colima-passport-stage7-*) ;; *) echo "Use isolated passport-stage7 context." >&2; exit 2 ;; esac
[ -d "$tmp" ] && [ ! -L "$tmp" ] || { echo "Private temporary root required." >&2; exit 2; }
mode=$(stat -f '%Lp' "$tmp" 2>/dev/null || stat -c '%a' "$tmp" 2>/dev/null || true)
[ "$mode" = 700 ] || { echo "Temporary root mode must be 0700." >&2; exit 2; }
global=$(docker context show)
docker --context "$ctx" info --format '{{.OSType}}' | grep -qx linux || { echo "Expected Linux Docker engine." >&2; exit 2; }
free=$(df -Pk "$tmp" | awk 'NR == 2 {print $4}')
[ -n "$free" ] && [ "$free" -ge 8388608 ] || { echo "At least 8 GiB free disk is required." >&2; exit 2; }
for port in 18080 18081 18443; do
  if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then echo "Ingress test port is busy." >&2; exit 2; fi
done
private=$(mktemp -d "$tmp/passport-deploy-readiness.XXXXXX")
chmod 0700 "$private"
project="passport-stage7-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
tunnel_pid=
env_file=$private/compose.env
compose() {
  docker --context "$ctx" compose --project-name "$project" --env-file "$env_file" --profile queue --profile migrate --profile ops --profile lan-debug \
    -f "$repo/compose.yaml" -f "$repo/compose.production.yaml" -f "$repo/compose.lan-debug.yaml" "$@"
}
compose_target() {
  docker --context "$ctx" compose --project-name "$project" --env-file "$private/restore.env" --profile queue --profile migrate --profile ops --profile lan-debug \
    -f "$repo/compose.yaml" -f "$repo/compose.production.yaml" -f "$repo/compose.lan-debug.yaml" "$@"
}
provision_private_output_mount() {
  service=$1
  target=$2
  expected_source=$3
  container=$(compose run -d --rm --entrypoint /bin/sh "$service" -c 'sleep 30')
  mounted_source=$(docker --context "$ctx" inspect --format \
    "{{range .Mounts}}{{if eq .Destination \"$target\"}}{{.Source}}{{end}}{{end}}" "$container")
  if [ "$mounted_source" != "$expected_source" ]; then
    docker --context "$ctx" stop "$container" >/dev/null 2>&1 || true
    echo "Private output mount source did not match the newly created fixture." >&2
    exit 1
  fi
  docker --context "$ctx" exec "$container" sh -c \
    'test -d "$1" && test ! -L "$1" && chown "$PASSPORT_OPS_UID:$PASSPORT_OPS_GID" "$1" && chmod 0700 "$1"' \
    sh "$target"
  docker --context "$ctx" stop "$container" >/dev/null
}
cleanup() {
  if [ -n "$tunnel_pid" ]; then
    kill "$tunnel_pid" >/dev/null 2>&1 || true
    wait "$tunnel_pid" 2>/dev/null || true
  fi
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "$private"
  [ "$(docker context show)" = "$global" ] || echo "Global Docker context changed unexpectedly." >&2
}
trap cleanup EXIT HUP INT TERM
mkdir -m 0700 "$private/images" "$private/api-state" "$private/ocr-source" "$private/ocr-models" \
  "$private/technical-backup" "$private/technical-key"
touch "$private/client-empty" "$private/ocr-token-empty" "$private/ocr-worker.json"
python3 - "$repo/deploy/api.production.example.json" "$private" "$env_file" <<'PY'
import hashlib,json,os,secrets,sys
from pathlib import Path
sample=json.loads(Path(sys.argv[1]).read_text()); root=Path(sys.argv[2])
api,worker,edge,debug,subnet="172.29.27.10","172.29.27.11","172.29.27.20","172.29.27.21","172.29.27.0/24"
sample["allowed_client_networks"]=[edge+"/32",debug+"/32"]; sample["listen_address"]=api+":8080"
sample["queue_processing"]["private_listener_address"]=api+":8081"
sample["logging"]["path"]="/var/lib/passport/state/logs/api.jsonl"
sample["metrics"]["state_path"]="/var/lib/passport/state/metrics/state.json"
(root/"api.json").write_text(json.dumps(sample))
def put(name,value):
 p=root/name; p.write_text(value); p.chmod(0o600); return str(p)
pg,rabbit=secrets.token_hex(24),secrets.token_hex(24)
worker_token="pwt1_"+secrets.token_urlsafe(32).rstrip("=")
registry={"schema_version":"passport-worker-credentials/v1","credentials":[{"worker_id":"11111111-1111-4111-8111-111111111111","token_sha256":hashlib.sha256(worker_token.encode()).hexdigest()}]}
put("pg-password",pg); put("rabbit-password",rabbit)
put("pg-dsn",f"postgres://passport:{pg}@postgres:5432/passport?sslmode=disable")
put("pg-restore-dsn",f"postgres://passport:{pg}@postgres:5432/passport_restore?sslmode=disable")
put("broker-dsn",f"amqp://passport:{rabbit}@rabbitmq:5672/%2F")
put("worker-registry",json.dumps(registry)); put("worker-token",worker_token)
values={
"PASSPORT_POSTGRES_DB":"passport","PASSPORT_POSTGRES_USER":"passport","PASSPORT_POSTGRES_PASSWORD_FILE":str(root/"pg-password"),
"PASSPORT_RABBITMQ_USER":"passport","PASSPORT_RABBITMQ_PASSWORD_FILE":str(root/"rabbit-password"),
"PASSPORT_POSTGRES_DSN_FILE":str(root/"pg-dsn"),"PASSPORT_RABBITMQ_DSN_FILE":str(root/"broker-dsn"),
"PASSPORT_CLIENT_CREDENTIALS_FILE":str(root/"client-empty"),"PASSPORT_WORKER_CREDENTIALS_FILE":str(root/"worker-registry"),
"PASSPORT_OCR_WORKER_TOKEN_FILE":str(root/"worker-token"),"PASSPORT_API_CONFIG_FILE":str(root/"api.json"),
"PASSPORT_OCR_CONFIG_FILE":str(root/"ocr-worker.json"),"PASSPORT_IMAGE_ROOT":str(root/"images"),
"PASSPORT_OCR_IMAGE":"passport-backend-ocr:stage4",
"PASSPORT_API_STATE_ROOT":str(root/"api-state"),"PASSPORT_RUSSIANDOCS_ROOT":str(root/"ocr-source"),
"PASSPORT_MODELS_ROOT":str(root/"ocr-models"),
"PASSPORT_TECHNICAL_BACKUP_DIR":str(root/"technical-backup"),
"PASSPORT_TECHNICAL_BACKUP_KEY_DIR":str(root/"technical-key"),
"PASSPORT_TECHNICAL_BACKUP_KEY_FILE":str(root/"technical-key"/"backup.key"),
"PASSPORT_APP_UID":str(os.getuid()),"PASSPORT_APP_GID":str(os.getgid()),
"PASSPORT_API_IP":api,"PASSPORT_WORKER_IP":worker,"PASSPORT_INGRESS_IP":edge,"PASSPORT_PRIVATE_SUBNET":subnet,
"PASSPORT_LAN_DEBUG_INGRESS_IP":debug,
"PASSPORT_PRIVATE_DYNAMIC_POOL":"172.29.27.128/25",
"PASSPORT_TLS_DOMAIN":"passport-stage7.test","PASSPORT_PUBLIC_BIND":"127.0.0.1",
"PASSPORT_TLS_CERTIFICATE_FILE":str(root/"tls.crt"),"PASSPORT_TLS_PRIVATE_KEY_FILE":str(root/"tls.key")}
Path(sys.argv[3]).write_text("\n".join(f"{k}={v}" for k,v in values.items())+"\n"); Path(sys.argv[3]).chmod(0o600)
target_values=dict(values)
target_values["PASSPORT_POSTGRES_DSN_FILE"]=str(root/"pg-restore-dsn")
target_values["PASSPORT_TECHNICAL_BACKUP_API_STOPPED"]="true"
Path(root/"restore.env").write_text("\n".join(f"{k}={v}" for k,v in target_values.items())+"\n")
Path(root/"restore.env").chmod(0o600)
PY
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout "$private/tls.key" -out "$private/tls.crt" -subj "/CN=passport-stage7.test" -addext "subjectAltName=DNS:passport-stage7.test" >/dev/null 2>&1
chmod 0600 "$private/tls.key" "$private/tls.crt"
compose config --quiet
compose build api migrate caddy lan-debug-caddy client-admin
compose run --rm migrate >/dev/null
compose up -d postgres api caddy lan-debug-caddy >/dev/null
profile=${ctx#colima-}
ssh_config="$HOME/.colima/_lima/$ctx/ssh.config"
[ -r "$ssh_config" ] || { echo "Isolated Colima SSH config unavailable." >&2; exit 1; }
ingress_container=$(compose ps -q caddy)
ingress_ip=$(docker --context "$ctx" inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ingress_container")
case "$ingress_ip" in 172.*) ;; *) echo "Caddy private address unavailable." >&2; exit 1 ;; esac
debug_container=$(compose ps -q lan-debug-caddy)
debug_ip=$(docker --context "$ctx" inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$debug_container")
case "$debug_ip" in 172.*) ;; *) echo "LAN debug ingress private address unavailable." >&2; exit 1 ;; esac
ssh -F "$ssh_config" -S none -o ControlMaster=no -o ControlPath=none -o ControlPersist=no -N \
  -L "127.0.0.1:18080:$ingress_ip:8080" \
  -L "127.0.0.1:18443:$ingress_ip:8443" \
  -L "127.0.0.1:18081:$debug_ip:8080" \
  "lima-$ctx" >"$private/ssh-tunnel.log" 2>&1 &
tunnel_pid=$!
domain=passport-stage7.test
i=0; ready=0
while [ "$i" -lt 60 ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --connect-to "$domain:443:127.0.0.1:18443" --cacert "$private/tls.crt" "https://$domain/healthz" 2>/dev/null || true)
  [ "$code" = 404 ] && { ready=1; break; }
  i=$((i+1)); sleep 1
done
[ "$ready" -eq 1 ] || {
  echo "TLS ingress did not become ready." >&2
  compose ps >&2 || true
  compose logs --no-color --tail=40 caddy >&2 || true
  exit 1
}
debug_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-to "$domain:8080:127.0.0.1:18081" "http://$domain:8080/healthz" 2>/dev/null || true)
[ "$debug_code" = 404 ] || { echo "LAN debug HTTP ingress is not reachable or allowlisted incorrectly." >&2; exit 1; }
redirect=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --connect-to "$domain:80:127.0.0.1:18080" "http://$domain/jobs" 2>/dev/null || true)
case "$redirect" in "301 https://$domain/jobs"*) ;; *) echo "HTTP redirect failed." >&2; exit 1 ;; esac
for path in /v1/stats /v1/passport/recognize /healthz /readyz /internal/worker/v1/claim; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --connect-to "$domain:443:127.0.0.1:18443" --cacert "$private/tls.crt" "https://$domain$path" 2>/dev/null || true)
  [ "$code" = 404 ] || { echo "Ingress allowlist failed." >&2; exit 1; }
done
api_container=$(compose ps -q api)
ports=$(docker --context "$ctx" inspect --format '{{json .NetworkSettings.Ports}}' "$api_container")
printf '%s' "$ports" | grep -q '"HostPort"' && { echo "API host port unexpectedly published." >&2; exit 1; }
[ -z "$(compose ps -q rabbitmq)" ] || { echo "RabbitMQ started before outage test." >&2; exit 1; }
PASSPORT_TEST_CERT="$private/tls.crt" PASSPORT_TEST_PORT=18443 PASSPORT_TEST_TOKEN_FILE="$private/client-token" PASSPORT_TEST_JOB_ID_FILE="$private/job-id" PASSPORT_TEST_CLIENT_ID_FILE="$private/client-id" python3 - "$private" <<'PY'
import hashlib,http.client,json,os,secrets,socket,ssl,sys,urllib.error,urllib.request
from pathlib import Path
host="passport-stage7.test"; ctx=ssl.create_default_context(cafile=os.environ["PASSPORT_TEST_CERT"])
class LoopbackHTTPSConnection(http.client.HTTPSConnection):
 def connect(self):
  sock=socket.create_connection(("127.0.0.1",int(os.environ["PASSPORT_TEST_PORT"])),self.timeout)
  self.sock=self._context.wrap_socket(sock,server_hostname=self.host)
class LoopbackHTTPSHandler(urllib.request.HTTPSHandler):
 def https_open(self,req):
  return self.do_open(lambda request_host,**kwargs:LoopbackHTTPSConnection(request_host,context=self._context),req)
opener=urllib.request.build_opener(LoopbackHTTPSHandler(context=ctx))
token="pbt1_"+secrets.token_urlsafe(32).rstrip("="); path=Path(os.environ["PASSPORT_TEST_TOKEN_FILE"]); path.write_text(token); path.chmod(0o600)
def request(route,method="GET",data=None,headers=None):
 req=urllib.request.Request("https://"+host+route,data=data,headers=headers or {},method=method)
 try:
  with opener.open(req,timeout=8) as response:return response.status,response.read(1<<20)
 except urllib.error.HTTPError as error:return error.code,error.read(1<<20)
hash_body=json.dumps({"token_sha256":hashlib.sha256(token.encode()).hexdigest()}).encode()
code,registration=request("/v1/client-installations","POST",hash_body,{"Content-Type":"application/json"})
if code not in (200,201): raise SystemExit("client enrollment failed")
client_id=json.loads(registration).get("client_id")
if not client_id: raise SystemExit("client enrollment response invalid")
client_path=Path(os.environ["PASSPORT_TEST_CLIENT_ID_FILE"]); client_path.write_text(client_id); client_path.chmod(0o600)
boundary="stage7"+secrets.token_hex(12); image=b"synthetic deployment fixture; no OCR inference"
body=(f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"fixture.png\"\r\nContent-Type: image/png\r\n\r\n").encode()+image+f"\r\n--{boundary}--\r\n".encode()
auth={"Authorization":"Bearer "+token}; headers={**auth,"Content-Type":"multipart/form-data; boundary="+boundary,"Idempotency-Key":"stage7-deployment-readiness"}
code,raw=request("/v1/passport/jobs","POST",body,headers); accepted=json.loads(raw)
if code!=202 or accepted.get("status")!="queued" or not accepted.get("job_id"): raise SystemExit("durable acceptance failed")
job=accepted["job_id"]; out=Path(os.environ["PASSPORT_TEST_JOB_ID_FILE"]); out.write_text(job); out.chmod(0o600)
code,raw=request("/v1/passport/jobs/"+job,headers=auth)
if code!=200 or json.loads(raw).get("status")!="queued": raise SystemExit("owner status failed")
code,_=request("/v1/passport/jobs/"+job+"/receipt","POST",b"",auth)
if code!=409: raise SystemExit("nonterminal receipt guard failed")
print("TLS enrollment, durable queued acceptance, owner status, and receipt guard passed.")
PY
snapshot_log=$private/passport-ops.log
if ! snapshot=$(compose run --rm passport-ops passport-ops snapshot \
  -dsn-file /run/passport-ops/postgres_dsn \
  -image-root /var/lib/passport/images \
  -max-upload-bytes 8388608 -max-stored-image-bytes 16777216 \
  -connect-timeout 5s -migration-lock-timeout 5s -operation-timeout 5s \
  -queued-jobs-threshold 100 -oldest-queued-threshold 1h \
  -expired-leases-threshold 100 -terminal-images-threshold 100 \
  -terminal-results-threshold 100 -image-used-bytes-threshold 16777216 2>"$snapshot_log"); then
  echo "Read-only aggregate ops snapshot failed; safe mount diagnostics follow." >&2
  compose run --rm --entrypoint /bin/sh passport-ops -c '
    id
    for path in /run/passport-ops /run/secrets/postgres_dsn /var/lib/passport/images; do
      if [ -e "$path" ]; then stat -c "%a %u:%g %F" "$path"; else echo missing; fi
    done
    [ -r /run/secrets/postgres_dsn ] && echo dsn_readable || echo dsn_unreadable
    wget -q -T 2 -O /dev/null http://api:8080/readyz && echo api_ready_reachable || echo api_ready_unreachable
  ' >&2 || true
  cat "$snapshot_log" >&2
  exit 1
fi
printf '%s' "$snapshot" | python3 -c 'import json,sys; d=json.load(sys.stdin); required={"queued_jobs","oldest_queued_age_ms","expired_leases","terminal_images_pending","terminal_results_pending","image_used_bytes","image_quota_bytes","image_free_bytes","alerts"}; assert set(d)==required and set(d["alerts"])=={"queued_jobs","oldest_queued_age","expired_leases","terminal_images_pending","terminal_results_pending","image_used_bytes"}; assert all(type(d[k]) is int for k in required-{"alerts"}); print("Read-only aggregate ops snapshot passed.")'
job=$(cat "$private/job-id"); db=$(compose ps -q postgres)
pending=$(docker --context "$ctx" exec "$db" psql -U passport -d passport -Atqc "SELECT count(*) FROM outbox WHERE job_id='$job'::uuid AND published_at IS NULL")
[ "$pending" = 1 ] || { echo "Outbox not pending during broker outage." >&2; exit 1; }
compose up -d rabbitmq >/dev/null
i=0; published=0
while [ "$i" -lt 60 ]; do
  state=$(docker --context "$ctx" exec "$db" psql -U passport -d passport -Atqc "SELECT count(*) FROM outbox WHERE job_id='$job'::uuid AND published_at IS NOT NULL" 2>/dev/null || true)
  [ "$state" = 1 ] && { published=1; break; }
  i=$((i+1)); sleep 1
done
[ "$published" -eq 1 ] || { echo "Outbox relay did not recover." >&2; exit 1; }
client_id=$(cat "$private/client-id")
revoked=$(compose run --rm client-admin client-admin revoke -dsn-file /run/passport-ops/postgres_dsn -client-id "$client_id" -connect-timeout 5s -migration-lock-timeout 30s)
[ "$revoked" = "client installation is inactive" ] || { echo "Private client-admin revoke failed." >&2; exit 1; }
PASSPORT_TEST_CERT="$private/tls.crt" PASSPORT_TEST_PORT=18443 PASSPORT_TEST_TOKEN_FILE="$private/client-token" PASSPORT_TEST_JOB_ID_FILE="$private/job-id" python3 - <<'PY'
import http.client,os,socket,ssl,urllib.error,urllib.request
host="passport-stage7.test"; ctx=ssl.create_default_context(cafile=os.environ["PASSPORT_TEST_CERT"])
class LoopbackHTTPSConnection(http.client.HTTPSConnection):
 def connect(self):
  sock=socket.create_connection(("127.0.0.1",int(os.environ["PASSPORT_TEST_PORT"])),self.timeout)
  self.sock=self._context.wrap_socket(sock,server_hostname=self.host)
class LoopbackHTTPSHandler(urllib.request.HTTPSHandler):
 def https_open(self,req):
  return self.do_open(lambda request_host,**kwargs:LoopbackHTTPSConnection(request_host,context=self._context),req)
opener=urllib.request.build_opener(LoopbackHTTPSHandler(context=ctx))
token=open(os.environ["PASSPORT_TEST_TOKEN_FILE"]).read(); job=open(os.environ["PASSPORT_TEST_JOB_ID_FILE"]).read()
request=urllib.request.Request("https://"+host+"/v1/passport/jobs/"+job,headers={"Authorization":"Bearer "+token})
try:
 opener.open(request,timeout=8)
 raise SystemExit("revoked client remained authenticated")
except urllib.error.HTTPError as error:
 if error.code!=401: raise SystemExit("revoked client returned unexpected status")
print("Private client-admin revocation and credential denial passed.")
PY
provision_private_output_mount technical-backup-keygen /run/key-output "$private/technical-key"
if ! compose run --rm technical-backup-keygen \
  technical-backup keygen --output /run/key-output/backup.key >/dev/null; then
  echo "Technical-backup keygen failed; safe output mount diagnostics follow." >&2
  compose run --rm --entrypoint /bin/sh technical-backup-keygen -c '
    id
    stat -c "%a %u:%g %F" /run/key-output
    su-exec "$PASSPORT_OPS_UID:$PASSPORT_OPS_GID" sh -c \
      '\''id; test -w /run/key-output && echo key_directory_writable || echo key_directory_not_writable; touch /run/key-output/.probe && rm /run/key-output/.probe'\''
  ' >&2 || true
  exit 1
fi
[ "$(stat -f '%Lp' "$private/technical-key/backup.key" 2>/dev/null || stat -c '%a' "$private/technical-key/backup.key")" = 600 ] || {
  echo "Technical-backup key file permissions failed." >&2; exit 1;
}
[ "$(wc -c <"$private/technical-key/backup.key" | tr -d ' ')" = 32 ] || {
  echo "Technical-backup key size failed." >&2; exit 1;
}
provision_private_output_mount technical-backup /run/backup "$private/technical-backup"
backup_output=$(compose run --rm technical-backup \
  technical-backup create \
  --dsn-file /run/passport-ops/postgres_dsn \
  --key-file /run/passport-ops/backup_key \
  --output /run/backup/technical.pstb)
[ "$backup_output" = "technical backup created" ] || { echo "Technical-backup create failed." >&2; exit 1; }
db=$(compose ps -q postgres)
docker --context "$ctx" exec "$db" createdb -U passport passport_restore
compose_target run --rm migrate >/dev/null
restore_output=$(compose_target run --rm technical-backup \
  technical-backup restore \
  --dsn-file /run/passport-ops/postgres_dsn \
  --key-file /run/passport-ops/backup_key \
  --input /run/backup/technical.pstb \
  --confirm-empty-target)
[ "$restore_output" = "technical backup restored as non-processable tombstones" ] || {
  echo "Technical-backup restore failed." >&2; exit 1;
}
echo "Technical-backup keygen/create/restore CLI smoke passed on an isolated empty migrated target."
echo "TLS ingress, RabbitMQ-down intake, outbox recovery, and private admin command passed; OCR inference/production rollout were not exercised."
