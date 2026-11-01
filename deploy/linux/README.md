# Linux installer and configuration preflight

## Prerequisites and first install

The target must be Linux `amd64` or `arm64` with Python 3.9+, Docker Engine
28.0.0+, the Docker Compose plugin 2.33.1+, Buildx, `iproute2`, `curl`, and at
least 8 GiB free on `/`. Docker must already be installed and reachable through
the local Unix-socket context; the installer does not install or upgrade it.
Native builds require outbound access to the image registry and ACME endpoints.

## Temporary LAN HTTP for the current debug APK

Keep the archive and its `.sha256` sidecar together. Verify the downloaded
archive before extracting it, for example:

```sh
sha256sum -c passport-backend-linux-kit-20261101-lan-v2.tar.gz.sha256
tar -xzf passport-backend-linux-kit-20261101-lan-v2.tar.gz
```

Run the installer from the extracted kit root.

For the current Android **debug** APK on a trusted, isolated LAN, install the
HTTP profile with the host's private IPv4 address:

```sh
sudo ./install.sh --lan --public-bind 192.168.1.100
```

Replace the example with an IPv4 address assigned to a local interface on the
Linux host. The installer publishes only Caddy at `http://192.168.1.100:8080`;
the API, PostgreSQL, RabbitMQ, and OCR worker remain private and have no host
ports. The Go API continues to accept traffic only from the private Caddy
container. Configure the host firewall to allow TCP 8080 only from the
intended LAN clients. In the debug APK, use `http://192.168.1.100:8080` as the
server URL.

This profile uses HTTP without TLS, so its bearer credential is visible to
other devices able to observe that network. Use it only on a trusted isolated
LAN and stop the test deployment afterward. It is incompatible with the current
**release** APK, which requires HTTPS. Use the TLS procedure below for release
builds or any untrusted network; the LAN profile does not configure Internet
forwarding or a public ingress.

For managed TLS, point the domain to this host and allow inbound TCP ports 80
and 443, then run from the extracted kit root:

```sh
sudo ./install.sh --domain api.example.com
```

Caddy obtains and renews the certificate and keeps its ACME data in named
Compose volumes. For a pre-provisioned static certificate and matching key, use
both `--tls-cert FILE --tls-key FILE`; the installer validates the pair and
copies them into the private runtime directory. For this mode the final HTTPS
smoke uses the supplied certificate chain as a trust bundle; hostname and
expiry validation stay enabled. Managed TLS uses the system trust store and
public ACME. `--public-bind ADDRESS` can
limit published ports to one IPv4 address; the default publishes only Caddy on
`0.0.0.0`. API, PostgreSQL, RabbitMQ, and OCR have no host-published ports.

The first run validates the bundle, builds images, checks generated API/worker
configuration, starts PostgreSQL and RabbitMQ, runs migrations, then starts API,
worker, and ingress. The final TLS probe verifies the authentication boundary;
it does not submit a document or measure OCR. The operator-owned state lives in
`/srv/passport-runtime` with a private install marker, configuration, secrets,
source/model snapshot, database state, and image root. Re-running the same
command resumes that installation and preserves its credentials and named
volumes; changed domain, bind, TLS mode/certificate, bundle, UID, or model
snapshot is refused instead of overwriting the existing runtime. Do not remove
the runtime directory or Compose volumes to “retry” a partial installation.

## Status, stop, and resume

For the default managed-TLS installation, run these from the extracted kit
directory. Keep the same project name, env file, and Compose overlays on every
invocation so the database and RabbitMQ volumes remain attached. The installer
records the validated local Docker context in the root-only install marker and
rejects reruns from a different context; select that same context for operator
commands:

```sh
DOCKER_CONTEXT=$(sudo python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["docker_context"])' \
  /srv/passport-runtime/install.json)

sudo docker --context "$DOCKER_CONTEXT" compose --project-name passport-backend \
  --env-file /srv/passport-runtime/compose.env \
  -f compose.yaml -f compose.managed-tls.yaml --profile queue ps

sudo docker --context "$DOCKER_CONTEXT" compose --project-name passport-backend \
  --env-file /srv/passport-runtime/compose.env \
  -f compose.yaml -f compose.managed-tls.yaml --profile queue stop

sudo docker --context "$DOCKER_CONTEXT" compose --project-name passport-backend \
  --env-file /srv/passport-runtime/compose.env \
  -f compose.yaml -f compose.managed-tls.yaml --profile queue start
```

For static TLS, replace the env file with
`/srv/passport-runtime/compose.static.env` and the overlay with
`compose.production.yaml`. The installer creates those files only when the
static certificate options are used. `start` resumes existing containers and
volumes; do not run `down -v` as a stop/restart operation. Check readiness via
`docker compose ... ps` and the existing health status; this is operational
health, not OCR quality or capacity acceptance.

The installer should validate both generated JSON files with the exact binaries
and runtime images that will be started. These checks parse configuration and
verify the selected OCR source/model snapshot; they do not start listeners,
connect to PostgreSQL or RabbitMQ, or run recognition.

## API

Run the API through its normal root bootstrap. It copies the configured
secrets to private runtime storage, drops privileges, and forwards the
check-only flag instead of starting the server:

```sh
docker compose --profile queue run --rm --no-deps \
  api --check-config
```

Exit status `0` means the strict API JSON configuration passed validation. A
nonzero exit means it must not be deployed. The check emits only a fixed error
category and does not print configuration values or paths. It does not open
secret files or make network connections.

## OCR worker

Run the worker through its normal entrypoint so the root bootstrap copies
secrets to private runtime storage and drops privileges before validation:

```sh
docker compose --profile queue run --rm --no-deps ocr --check-config
```

The bootstrap forwards `--check-config` to the worker after dropping to its
configured runtime UID. The worker validates its strict JSON field set, secret
file permissions and token/password format, selected RussianDocsOCR source
version, model-lock version, and every locked model file's size and SHA-256. It
does not import or construct the OCR `Pipeline`, connect to Go or RabbitMQ, or
create a readiness marker.
Successful validation prints only `configuration_valid`; failure output is a
fixed category with no paths, secrets, model hashes, or parser details.

The worker configuration must contain exactly these fields:

```text
rabbit_host, rabbit_port, rabbit_user, rabbit_password_file, rabbit_vhost,
queue, exchange, routing_key, dead_letter_exchange, dead_letter_queue,
go_worker_url, worker_token_file, readiness_marker, image_root,
russiandocs_repo, models_root, work_root, max_input_bytes, max_image_pixels,
expected_upstream_version, expected_model_lock_version,
startup_timeout_seconds, processing_stop_grace_seconds, http_timeout_ms,
boundary_attempts, reconnect_backoff_seconds, wait_poll_max_seconds,
rabbit_heartbeat_seconds, rabbit_blocked_connection_timeout_seconds,
consumer_ack_timeout_ms
```

The expected version labels must identify the operator-selected read-only source
and model snapshot. Validation does not upgrade either asset and does not prove
OCR accuracy or runtime readiness. Run preflight again after changing either
configuration or mounted source/model assets.
