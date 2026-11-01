# Portable Compose deployment

This directory contains a self-contained Docker Compose deployment for the
queue-based API. It builds its images from the checked-out repository and
does not require Go or Python on the host. The OCR build fetches the pinned
RussianDocsOCR snapshot and verifies every model against its lock file.

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

For example:

```sh
PASSPORT_PUBLIC_BIND=192.168.1.40 docker compose -f deploy/portable/compose.yaml up -d --build
```

The API, worker, and ingress use fixed addresses inside a private Docker
network so the existing API peer allowlist remains narrow. If that subnet
overlaps another network on the host, configure these values together before
the first start: `PASSPORT_PRIVATE_SUBNET`, `PASSPORT_PRIVATE_DYNAMIC_POOL`,
`PASSPORT_API_IP`, `PASSPORT_WORKER_IP`, and `PASSPORT_INGRESS_IP`. The
bootstrap rejects addresses outside the selected subnet or duplicate service
addresses. Changing the bind or network values after initialization fails
closed rather than replacing credentials or database volumes.

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
