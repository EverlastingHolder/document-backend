# Portable Compose deployment

This directory contains a self-contained Docker Compose deployment for the
queue-based API. It builds its images from the checked-out repository and
does not require Go or Python on the host. The OCR build fetches the pinned
RussianDocsOCR snapshot and verifies every model against its lock file; this
first build needs network access to fetch those pinned public assets.

From the repository root, start the stack with:

```sh
docker compose -f deploy/portable/compose.yaml up -d --build
```

On first start, the `bootstrap` service creates random database, broker, and
worker credentials in separate named volumes, writes the API and worker
configuration, validates both configurations without network access, and
then lets Compose run the migration before the API and OCR consumer start.
The credentials are preserved across container recreation. Do not remove
volumes when stopping the stack.

By default, only Caddy is published, on `127.0.0.1:8080`. To expose the HTTP
endpoint on a specific LAN interface, set `PASSPORT_PUBLIC_BIND` to that
machine's private IPv4 address for both the initial start and later Compose
commands. The Compose file does not inspect host interfaces or publish the
API, PostgreSQL, or RabbitMQ ports.

Swagger UI is available separately at `http://127.0.0.1:8081`. It reads the
checked-in OpenAPI file through a read-only mount and receives no application
secrets. The docs listener remains loopback-bound when the API is exposed to
the LAN. Set `PASSPORT_DOCS_BIND` to the host's private IPv4 only if the UI
also needs LAN access, and restrict TCP 8081 in the firewall. Its `Try it out`
requests use Caddy's existing API route allowlist; unknown `/v1` routes remain
404. Do not expose the documentation listener to untrusted networks.

For example:

```sh
PASSPORT_PUBLIC_BIND=192.168.1.40 docker compose -f deploy/portable/compose.yaml up -d --build
```

To keep the LAN bind setting for later commands, use the ignored local env
file from this directory:

```sh
cd deploy/portable
cp .env.example .env
# Edit PASSPORT_PUBLIC_BIND in .env, then:
docker compose up -d --build
```

The API, worker, and ingress use fixed addresses inside a private Docker
network so the existing API peer allowlist remains narrow. If that subnet
overlaps another network on the host, configure these values together before
the first start: `PASSPORT_PRIVATE_SUBNET`, `PASSPORT_PRIVATE_DYNAMIC_POOL`,
`PASSPORT_API_IP`, `PASSPORT_WORKER_IP`, and `PASSPORT_INGRESS_IP`. The
bootstrap rejects addresses outside the selected subnet or duplicate service
addresses. The host bind may change without rotating credentials; changing
the private network topology fails closed rather than replacing credentials
or database volumes.

Check services with:

```sh
docker compose -f deploy/portable/compose.yaml ps
```

Stop containers while retaining all named volumes with:

```sh
docker compose -f deploy/portable/compose.yaml down
```

Do not use `down --volumes` unless deleting the database, credentials, images,
and runtime state is intentional. The optional offline retention sweep is
available through the `ops` profile and requires the API to be stopped:

```sh
PASSPORT_MAINTENANCE_API_STOPPED=true docker compose --profile ops -f deploy/portable/compose.yaml run --rm job-maintenance
```

This LAN profile uses HTTP. Use a TLS-terminating deployment for traffic that
crosses untrusted networks; this Compose file does not configure public TLS.

## Rehearsal boundary

The portable stack was cold-started and rerun on an isolated Linux ARM64
Docker engine. After `down` without volume removal and `up --build`, all
services became healthy and the seven generated secret/config fingerprints,
PostgreSQL schema/data, and RabbitMQ node identity remained stable. A synthetic
blank-image job reached the expected terminal `image_unprocessable` state on
attempt 1; receipt returned 204, status returned 410, and the test image/result
cleanup checks were zero. This verifies deployment, persistence, and the
receipt/retention path. It does not establish OCR recognition quality, native
AMD64 behavior, Windows/WSL2 LAN forwarding, or production capacity.
