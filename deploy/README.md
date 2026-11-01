# Локальный запуск и LAN-конфигурация

## Linux install kit

Для чистого Linux-сервера используйте готовый архив установки, а не этот checkout целиком. Архив содержит allowlisted public source, шаблоны, зафиксированный RussianDocsOCR snapshot и только файлы моделей из `models.lock.json`; `.git`, `.local`, результаты, пользовательские изображения и секреты в него не входят. После распаковки из корня комплекта:

```sh
sudo ./install.sh --domain api.example.com
```

По умолчанию Caddy автоматически получает и обновляет публичный TLS-сертификат. До запуска у домена должны быть корректные DNS записи на сервер, а входящие TCP-порты `80` и `443` должны доходить до него. Для уже подготовленной пары статического сертификата и ключа можно передать оба параметра `--tls-cert FILE --tls-key FILE`; installer проверяет соответствие пары, затем сохраняет копии как private files. `--public-bind ADDRESS` задаёт конкретный IPv4 адрес для published ingress ports; default `0.0.0.0` публикует только Caddy на всех интерфейсах. API, PostgreSQL, RabbitMQ и OCR worker остаются без published ports.

На target нужны Linux amd64 или arm64, Python 3.9+, Docker Engine `28.0.0+`, Compose plugin `2.33.1+`, Buildx и не менее 8 GiB свободного места. Установка Docker — отдельный предварительный шаг; installer не обновляет существующий Engine, Compose plugin или чужие контейнеры. Сервер должен иметь исходящий доступ к registry/package repositories и к ACME endpoints. Installer делает локальную native-architecture сборку образов и выполняет bundle integrity/model-lock checks, config-only checks, отдельную миграцию PostgreSQL, worker warmup/readiness и ingress smoke. Он не запускает реальное пользовательское распознавание.

Installer создаёт target-only credentials и runtime state в `/srv/passport-runtime`, сохраняет их при повторном запуске и отказывает при неизвестном содержимом или несовпадающем domain/assets. Архив не содержит внешних model/source inputs в Docker build context: installer копирует проверенный snapshot в закрытые `/srv/passport-runtime/russiandocs` и `models`, подключает их worker-у read-only, а фотографии хранит только в отдельном image root. Один host/volume не является HA; примерные upload/quota/timeouts — стартовая конфигурация, не обещание capacity или SLA.

Команды первой установки, health/status, stop/resume, firewall/DNS требования и ограничения повторного запуска находятся в [Linux installer runbook](linux/README.md). Формат архивного комплекта и platform support описаны в [официальной инструкции Docker Engine](https://docs.docker.com/engine/install/), [Compose plugin для Linux](https://docs.docker.com/compose/install/linux/) и [multi-platform builds](https://docs.docker.com/build/building/multi-platform/). Автоматический TLS использует [Caddy Automatic HTTPS](https://caddyserver.com/docs/automatic-https); сохранённые `/data` и `/config` Compose volumes нужны для ACME state.

Go API и единственный production RussianDocsOCR Python worker работают на одном компьютере. Worker запускается приватно, без отдельного сетевого порта. `deploy/api.local.example.json` предназначен только для loopback (`127.0.0.1:8080`) и не является конфигурацией для публикации в Интернет.

## Сборка и запуск

Из корня репозитория:

```sh
go build -o .local/bin/passport-api ./cmd/api
./.local/bin/passport-api -config deploy/api.local.example.json
```

Пример задаёт все настройки Go API, logging, metrics и один RussianDocsOCR runtime: Python environment, локальный clone, каталог весов, startup timeout и приватный `work_root`. Пути разрешаются относительно каталога config. Перед запуском подготовьте clone, environment и веса; API сам не устанавливает пакеты и не загружает модели. Не коммитьте clone, weights, private environment и локальные absolute paths.

Example config использует уникальный `pipeline_version` и private metrics snapshot. После смены версии pipeline создайте новый `metrics.state_path`, чтобы не смешивать cumulative counters разных вариантов.

## LAN-доступ

Для Android в локальной сети настройте в собственной копии config:

- `listen_address` — конкретный числовой локальный адрес интерфейса и порт;
- `allowed_client_networks` — CIDR устройства или доверенной локальной подсети.

Сервер отвергает wildcard/public bind и проверяет прямой TCP peer по CIDR. Эта ACL не является identity authentication; настройки не обеспечивают TLS, firewall, VPN или internet routing. Loopback example доступен только с того же компьютера. Не коммитьте локальный IP в общий пример.

## Async client installation enrollment

Новые Android installations не требуют ручного ввода token. Клиент создаёт собственный случайный `pbt1_` secret, сохраняет его в encrypted private storage до сетевого запроса и отправляет на `POST /v1/client-installations` только его lowercase SHA-256 (`token_sha256`, 64 hex-символа). Endpoint не принимает bearer token, owner ID или device metadata. `201` означает новую регистрацию, `200` — replay того же hash с прежними `client_id` и `created_at`. Это анонимная identity установки, не login, аккаунт или проверка личности; переустановка с новым secret получает новую identity. Не помещайте secret или hash в логи, конфиг, БД, Git или APK. Release traffic с bearer secret требует TLS; прямой peer CIDR ACL остаётся сетевым фильтром, но не подтверждением личности.

Enrollment включается только при наличии async-конфигурации и отдельного `client_enrollment` блока. Оператор задаёт положительные `max_clients` и `max_new_clients_per_minute`; значения production по умолчанию не выбираются. Без блока enrollment endpoint возвращает `503`. Зарезервированный статическим registry или отозванный hash не регистрируется. `credentials_file` необязателен, если включён `client_enrollment`; optional static registry продолжает обслуживать ранее provisioned clients. Команда `cmd/client-token` остаётся для контролируемого legacy provisioning/rotation: её raw-token artifact следует передавать вручную только существующим клиентам через доверенный канал, а hash-only registry artifact устанавливать на сервере. Штатная новая Android установка этот helper не использует.

`deploy/api.queue.example.json` — controlled queue example со static client registry; он не включает `client_enrollment` и не включает новый enrollment route. Его числа являются примером конфигурации для проверки, не выбранными production defaults. Для автоматической регистрации оператор должен явно добавить `async_jobs.client_enrollment` с ограничениями, согласованными для своего окружения.

Async intake также требует `database_dsn_file`, positive `connect_timeout`, positive `migration_lock_timeout`, отдельный `image_root`, positive `max_stored_image_bytes` и positive `max_concurrent_uploads`. Существующий top-level `max_upload_bytes` ограничивает один файл. Пути разрешаются относительно JSON config; DSN и optional registry задаются файлами вне Git. Secret/DSN file должны быть regular files с mode `0600` в private parent directory mode `0700` или строже; image root — отдельный private writable directory mode `0700`. Положительная суммарная квота считает logical bytes объектов, orphan files и временных файлов. Один API process должен быть единственным writer для этого image root. `async_jobs.retention` задаётся явно: positive `claim_lease`, `interval` (не более 5 секунд), `operation_timeout` короче claim lease, positive `batch_size`; production defaults не подставляются. Result retention фиксирован: ACK удаляет результат раньше, иначе максимум 15 минут после terminal state. Terminal OCR удаляет исходное изображение сразу после сохранения terminal state. Во время недоступности PostgreSQL или диска физическое удаление задерживается до восстановления maintenance loop. Перед включением enrollment примените текущие миграции через `cmd/migrate`; API проверяет schema read-only.

Для обработки очереди задайте отдельный объект `queue_processing`: `worker_credentials_file`, `private_listener_address`, `broker_dsn_file` и explicit positive connect/poll/publish/reconnect, consumer-ack, retry, processing, completion, attempt-lease и recovery durations plus `max_attempts`. API сам запускает relay, recovery loop и worker-only HTTP listener; Python consumer — отдельный процесс/service. Worker credentials используют `pwt1_` и `passport-worker-credentials/v1`, отдельно от клиентских `pbt1_` credentials; provisioning описан в [worker runtime](../docs/WORKER_RUNTIME.md). `pipeline_version` задаётся один раз на верхнем уровне API config и передаётся consumer через Go claim response. Если нужен queue-only режим без legacy sync OCR, явно укажите top-level `sync_ocr_enabled: false`; иначе текущий sync endpoint продолжает работать и требует настроенный RussianDocsOCR runtime.

Одного `async_jobs` достаточно для durable acceptance, но не для queue processing: без отдельного объекта `queue_processing` jobs остаются `queued` и исходные images занимают quota. С `queue_processing` Go запускает relay, recovery loop и private worker listener; Python consumer должен быть запущен отдельно и иметь согласованный private shared image root. Stages 4–6 прошли controlled backend/Android acceptance; это не live OCR на выбранных private weights. `202` не означает завершение OCR.

Перед включением enrollment/intake отдельно запустите `cmd/migrate` с приватным DSN. При старте API проверяет migration ledger/schema read-only; отсутствующая или drifted schema закрывает startup, но API не применяет DDL автоматически. Если очередь включается, `queue_processing` дополнительно требует отдельный worker registry, private listener address, RabbitMQ DSN file и положительные publish/retry/lease/timeouts. Go использует top-level `pipeline_version`; отдельного queue-level значения нет. DSN должен быть достижим из network namespace процесса `cmd/api`. Checked-in Compose содержит внутренний `queue` profile для controlled integration и оставляет PostgreSQL/RabbitMQ/API/worker без опубликованных host-портов. Этот profile прошёл controlled integration acceptance, но не является production deployment acceptance: он требует operator-supplied source/model snapshot с явно настроенными `expected_upstream_version` и `expected_model_lock_version`, а также persistent state/image roots; source/assets не копируются в образ и автоматически не обновляются. Worker healthcheck проверяет свежий закрытый readiness marker после загрузки OCR pipeline и готовности Rabbit consumer; marker подтверждает процессный/readiness handshake, но не выполнение recognition, качество модели или throughput. API `/readyz` проверяет PostgreSQL и image root и намеренно не зависит от RabbitMQ. Если API запущен как host process, имена `postgres`/`rabbitmq` из Compose DNS и loopback host address сами по себе не дают ему доступ к контейнерам.

## Runtime boundaries

Все uploads обрабатываются в приватном временном workspace и удаляются после success, error или cancellation. Logs и cumulative stats не содержат request-level OCR text или passport values. Подробности config, process lifecycle, платформ и limits см. в [OCR API runtime](../docs/OCR_API_RUNTIME.md).

## PostgreSQL and RabbitMQ foundation

`compose.yaml` запускает PostgreSQL 18 и RabbitMQ 4 на внутренней сети без опубликованных портов. Infrastructure services доступны по умолчанию; профиль `queue` поднимает Go API и отдельный Python consumer для контролируемой integration проверки. Этот профиль использует внешние private config/secrets и выбранные оператором OCR source/model snapshot; source/weights не упаковываются в образ. Он не подтверждает production deployment, реальное качество OCR или HA. При настроенном Go `queue_processing` API запускает relay/recovery и private worker listener. API readiness проверяет только PostgreSQL и image root, поэтому durable intake может стартовать при отключённом RabbitMQ; outbox relay публикует позже при восстановлении брокера. API получает собственный persistent RW bind `PASSPORT_API_STATE_ROOT` → `/var/lib/passport/state` для логов и cumulative metrics; этот каталог не монтируется worker-у. Укажите существующий каталог с прежними state files, если требуется сохранить метрики через пересоздание контейнера; подготовьте его с правами `0700` и владельцем API UID/GID. Не заменяйте его пустым каталогом без явного решения сбросить state. Entrypoint не делает recursive `chown`; при несовпадении UID/GID исправьте владельца явно до запуска. `/run/passport-api` и `/run/passport-ocr` — временные private tmpfs для конфигурации и секретов, не хранилища состояния. Go migration runner создаёт PostgreSQL schema отдельным запуском с профилем `migrate`; API при включении async config проверяет schema read-only и не выполняет DDL при старте.

Для проверки используйте локальный Docker context, endpoint которого начинается с `unix://`, и закрытый каталог вне checkout, доступный Docker daemon. Каталог должен принадлежать текущему пользователю и иметь права `0700` или строже. Например, на хосте с локальным daemon:

```sh
test_secrets_root=$(mktemp -d /tmp/passport-backend-test.XXXXXX)
chmod 700 "$test_secrets_root"
PASSPORT_TEST_DOCKER_CONTEXT=my-local-context \
PASSPORT_TEST_SECRETS_ROOT="$test_secrets_root" \
  ./scripts/test-postgres-integration.sh
rmdir "$test_secrets_root"
```

Harness создаёт случайные пароль и DSN в закрытом временном подкаталоге, запускает уникальный Compose project, выполняет реальные PostgreSQL migration и constraint tests, запускает контейнерный migration runner, перезапускает PostgreSQL для проверки сохранённых строк и удаляет только созданный project вместе с его volumes. PostgreSQL доступен только сервисам на внутренней сети; Go integration test бинарник копируется в принадлежащий этому project контейнер PostgreSQL и подключается по имени сервиса `postgres`. Общий Docker context не меняется; harness отказывает для TCP и SSH endpoints, чтобы file-backed secrets не отправлялись удалённому daemon.

Пароли и DSN передаются через Compose file-backed secrets. Храните исходные файлы вне Git с правами `0600`; не вставляйте значения в Compose, shell arguments или вывод CI. Ограниченный одноразовый migration container запускается от root, потому что права bind-mounted secret-файла могут не совпадать с UID контейнера; у него read-only filesystem, нет Linux capabilities кроме `DAC_OVERRIDE` для чтения файла и включён `no-new-privileges`. RabbitMQ wrapper читает собственный secret-файл перед запуском официального entrypoint.

Docker build context мигратора ограничен `.dockerignore`: в daemon передаются только Go module manifests, `cmd/migrate` и пакет PostgreSQL с его миграциями. OCR models, private environments, локальные outputs, `.git` и остальные файлы репозитория в этот context не входят. При расширении образа API allowlist нужно пересмотреть отдельно.

## Production TLS ingress

`compose.production.yaml` добавляет только Caddy ingress и публикует host ports `80` и `443` на выбранном через `PASSPORT_PUBLIC_BIND` адресе. API, PostgreSQL, RabbitMQ и OCR worker остаются в private network без `ports`. Caddy использует закреплённый официальный Caddy `2.11.7-alpine` image digest, отключённый admin API и статические operator-issued certificate/key files; он не получает DSN, API bearer, client registry, worker token или RabbitMQ credentials. Оригиналы сертификата и ключа должны быть файлами с правами `0600` в private директории `0700`; ingress bootstrap копирует их в отдельный tmpfs и запускает Caddy как UID/GID `10001`. Автоматический ACME issuance не настроен.

PostgreSQL, RabbitMQ, API, OCR worker и ingress имеют `restart: unless-stopped`; migration и one-shot ops services используют `restart: "no"`. RabbitMQ hostname задаётся через `PASSPORT_RABBITMQ_HOSTNAME` (стабильное значение по умолчанию `passport-rabbitmq`); сохраняйте то же значение при каждом старте и пересоздании контейнера с существующим `rabbitmq_data`, иначе Mnesia node identity может измениться. Для уже созданного volume значение должно совпадать с его текущим node name. API, OCR и Caddy получают read-only root filesystem и `no-new-privileges`; bootstrap-контейнеры запускаются с минимальными capabilities только для чтения/copy/chown секретов и перехода на runtime UID/GID. Для API, OCR и Caddy включён init process. PostgreSQL выделено `shm_size: 128mb`; явные CPU/RAM limits или резервирования для сервисов не заданы. Данная Compose-конфигурация не обещает заданную throughput/capacity, а одна host/volume копия не является HA.

Caddy проксирует только `POST /v1/client-installations`, `POST /v1/passport/jobs`, UUID-путь `GET /v1/passport/jobs/{job_id}` и `POST /v1/passport/jobs/{job_id}/receipt`. Все остальные пути получают 404, включая `/v1/stats`, legacy sync recognition, `/healthz`, `/readyz` и `/internal/worker/v1`. Не включайте access logs с raw request paths: job IDs и client correlation headers не должны попадать в ingress logs. API allowlist в production config должна содержать только direct peer Caddy `/32`; это не заменяет bearer authentication.

Для настройки скопируйте `deploy/api.production.example.json` в приватный runtime config и явно замените illustrative limits/timing/version values. Создайте непустой `PASSPORT_API_CONFIG_FILE`, DSN/secrets, existing image root и API state root вне репозитория; задайте `PASSPORT_RUSSIANDOCS_ROOT` и `PASSPORT_MODELS_ROOT` как отдельные read-only mounts из уже выбранного приватного snapshot, и задайте `PASSPORT_OCR_IMAGE` на проверенный worker image reference (предпочтительно registry digest). Не копируйте source/model assets в образ и не выполняйте автоматические обновления. Задайте `PASSPORT_TLS_DOMAIN`, `PASSPORT_PUBLIC_BIND`, `PASSPORT_TLS_CERTIFICATE_FILE`, `PASSPORT_TLS_PRIVATE_KEY_FILE`, `PASSPORT_API_IP`, `PASSPORT_INGRESS_IP`, `PASSPORT_PRIVATE_SUBNET` и `PASSPORT_PRIVATE_DYNAMIC_POOL`, согласовав IP адреса с `listen_address`, `private_listener_address` и `allowed_client_networks`. Статические API/worker/Caddy адреса должны оставаться вне IPAM `ip_range` для PostgreSQL/RabbitMQ; базовый dynamic pool по умолчанию — верхняя половина private subnet. В стандартной конфигурации Caddy использует `.20`. Сначала запустите `migrate`, затем `postgres`, `api`, `rabbitmq`, worker и ingress. RabbitMQ может быть недоступен при старте API: пока брокер отсутствует, intake отвечает после durable PostgreSQL commit, relay ждёт восстановления. Для последующих `docker compose` запусков передавайте тот же приватный `--env-file`: значение `PASSPORT_RABBITMQ_HOSTNAME` должно оставаться неизменным, пока используется существующий `rabbitmq_data` volume.

## LAN debug ingress

`compose.lan-debug.yaml` запускает отдельный Caddy без TLS только для контролируемой локальной/LAN отладки. Он слушает private address `.21`, отдельный от production ingress `.20` и dynamic pool. Для него используйте отдельный runtime config на основе `deploy/api.lan-debug.example.json`, где `allowed_client_networks` содержит только private peer `.21/32`; не расширяйте CIDR для удобства. Базовый overlay не публикует Docker host port. Для отдельного тестового порта используйте дополнительный `compose.lan-debug-published.yaml` и явно задайте точный host LAN IP и свободный alternate port; wildcard bind (`0.0.0.0`) запрещён. В Colima сначала проверяйте локальным SSH-forward. Не подменяйте production Caddy и не перенаправляйте старый listener на `8080`.

Для Colima храните descriptor bridge в ignored private runtime directory с mode `0600` внутри parent directory `0700`, затем используйте `python3 scripts/async-runtime-bridge.py start|status|stop <absolute-private-config-path>`. Скрипт запускает detached SSH-forward с `ControlMaster=no` и `ControlPath=none`; private PID record рядом с descriptor хранит start time и digest аргументов. Перед остановкой он сверяет PID, executable, точный bind и forward, чтобы не сигнализировать переиспользованный PID или чужой listener. Команда `start` переживает выход вызвавшего shell; `status` проверяет живой forward, `stop` отправляет только ему `TERM`. Для Linux используйте published overlay только с конкретным адресом и внешним firewall.

Rollback до регистрации async-клиента: остановите bridge через `stop`, затем запустите прежний API ровно с argv и рабочей директорией из приватного rollback manifest. После регистрации нового клиента старый sync API не знает его credential и job state; в этом случае сохраняйте candidate PostgreSQL/RabbitMQ volumes и восстанавливайте candidate bridge, не очищайте `.local/async-runtime`.

Этот вариант передаёт bearer credential открытым HTTP и предназначен только для временного тестирования в доверенной изолированной сети. Не используйте его для production, через публичный Wi-Fi или в release Android build. После проверки остановите `lan-debug-caddy` и удалите временный tunnel; production ingress остаётся отдельным TLS overlay с установленным оператором сертификатом.

Контролируемая проверка deployment topology:

```sh
PASSPORT_STAGE7_DOCKER_CONTEXT=<isolated-context> \
PASSPORT_STAGE7_TMP_ROOT=<private-directory-outside-repository> \
scripts/test-deployment-readiness.sh
```

Скрипт использует собственный Compose project, случайные credentials, синтетическое multipart тело, self-signed TLS certificate и временные private roots. Он не читает/копирует реальные изображения или model weights и не запускает OCR inference. Проверка подтверждает HTTPS/HTTP redirect, закрытый ingress allowlist, отсутствие опубликованных API/broker ports, startup/readiness API при неработающем RabbitMQ, durable `202`/queued status, и последующую публикацию outbox после запуска broker. Она не выбирает production domain/certificate, не измеряет реальную OCR throughput/capacity и не является HA-проверкой. Сценарий удаляет только принадлежащий ему Compose project/volume и temp directory; он не удаляет Docker profile/context.

## Private operator commands

`client-admin`, `passport-ops` и `job-maintenance` доступны только через one-shot profile `ops`, подключённый к internal network без published ports. Все три сервиса используют DSN только как file-backed secret: entrypoint копирует его в одноразовый private tmpfs `0700`, задаёт `0600`, меняет владельца на API UID/GID и запускает бинарник без raw DSN в argv/environment. API config, raw client token, worker token и registry в ops containers не монтируются. `passport-ops` получает exact selected image root read-only; `job-maintenance` получает его RW для ограниченного удаления.

Revocation команды (UUID установки должен быть известен оператору из доверенного административного источника):

```sh
docker compose --profile ops run --rm client-admin \
  client-admin revoke \
  -dsn-file /run/passport-ops/postgres_dsn \
  -client-id <client-uuid> \
  -connect-timeout <duration> \
  -migration-lock-timeout <duration>
```

Aggregate health snapshot требует explicit positive upload/quota limits, timeouts и alert thresholds; результат не содержит job IDs, client IDs, images или OCR values:

```sh
docker compose --profile ops run --rm passport-ops \
  passport-ops snapshot \
  -dsn-file /run/passport-ops/postgres_dsn \
  -image-root /var/lib/passport/images \
  -max-upload-bytes <bytes> \
  -max-stored-image-bytes <bytes> \
  -connect-timeout <duration> \
  -migration-lock-timeout <duration> \
  -operation-timeout <duration> \
  -queued-jobs-threshold <count> \
  -oldest-queued-threshold <duration> \
  -expired-leases-threshold <count> \
  -terminal-images-threshold <count> \
  -terminal-results-threshold <count> \
  -image-used-bytes-threshold <bytes>
```

Offline privacy maintenance is deliberately manual. Stop every API/intake process using that database and image root first, then explicitly set `PASSPORT_MAINTENANCE_API_STOPPED=true` and the documented positive limit/timeout settings. Run `docker compose --profile ops run --rm job-maintenance job-maintenance sweep`; it checks schema without migrating, purges terminal image/result rows and removes eligible orphan/temp objects only under the exact private image root. Do not set the acknowledgement while any API instance can accept uploads. The profile does not start or depend on API automatically, and the image root is never replaced with an empty default.


## Stage 7 operator controls and backup boundary

`cmd/client-admin revoke` disables a database-enrolled installation by UUID and prints only a generic inactive confirmation; it does not delete the SHA-256 tombstone, jobs, or results. Run it only from a private environment that can reach PostgreSQL, with `-dsn-file` pointing to a regular `0600` secret in an existing exact-`0700` parent; never pass the DSN as an argument or environment variable. Example syntax: `client-admin revoke -dsn-file /run/secrets/postgres_dsn -client-id <installation-uuid> -connect-timeout 5s -migration-lock-timeout 10s`. The same opaque ID is never echoed.

This UUID command cannot revoke a legacy static-only credential that has no PostgreSQL row. Remove that credential from the private static registry and restart every API instance. If the token hash must also remain blocked from future enrollment, persist a DB tombstone with `client-admin revoke-hash -dsn-file /run/passport-ops/postgres_dsn -token-hash-file /run/passport-ops/token_hash -connect-timeout <duration> -migration-lock-timeout <duration>` before removing the registry entry. The hash file must contain exactly 64 lowercase SHA-256 hex characters, be mode `0600`, and live in a private `0700` directory; it is never printed. A `revoke-hash` tombstone overrides a matching static entry and prevents later registration of the same hash. Do not delete the tombstone to re-enable a credential; issue a new installation credential instead.

For a static-only credential, provide `PASSPORT_CLIENT_TOKEN_HASH_FILE` as a read-only mount at `/run/secrets/revocation_hash`; the ops entrypoint copies it into its private tmpfs before dropping privileges. The source file must remain outside the repository and must not contain the raw token. Example:

```sh
docker compose --profile ops run --rm \
  --volume "${PASSPORT_CLIENT_TOKEN_HASH_FILE:?set a private hash file}:/run/secrets/revocation_hash:ro" \
  client-admin client-admin revoke-hash \
  -dsn-file /run/passport-ops/postgres_dsn \
  -token-hash-file /run/passport-ops/token_hash \
  -connect-timeout <duration> \
  -migration-lock-timeout <duration>
```

`cmd/passport-ops snapshot` emits only queue, lease, cleanup backlog and aggregate image-quota values with threshold flags. Every threshold is explicit and positive; the command does not accept owner/job/token/image identifiers. The DB and image root must be reachable in the private operations network; the image root must already exist with mode `0700`. No public admin or statistics route is added for these thresholds.

Do not back up passport photos, canonical OCR results, temporary files, or raw OCR artifacts. Technical schema/config secrets and opaque IDs/hashes/status/idempotency metadata can support infrastructure recovery, but cannot restore missing image/result payloads. If the single host or image volume is lost, queued work with missing images needs a new client scan; restored technical rows are not proof that OCR input is recoverable. Images are removed after terminal OCR and result payloads at receipt or after 15 minutes; physical cleanup may be delayed during filesystem/DB outage and does not promise secure media erasure. Retained technical tombstones can grow without bound and require aggregate DB-growth monitoring.

### Encrypted technical backup

The `technical-backup-keygen` one-shot service writes a new raw 32-byte key only to the separately mounted `PASSPORT_TECHNICAL_BACKUP_KEY_DIR`. Keep that key in a private backup location separate from the database and encrypted artifact directory; do not regenerate it for an existing archive. Create an encrypted technical archive with `technical-backup` and operator-supplied private paths:

```sh
docker compose --profile ops run --rm technical-backup-keygen \
  technical-backup keygen --output /run/key-output/backup.key

docker compose --profile ops run --rm technical-backup \
  technical-backup create \
  --dsn-file /run/passport-ops/postgres_dsn \
  --key-file /run/passport-ops/backup_key \
  --output /run/backup/technical.pstb
```

Set `PASSPORT_TECHNICAL_BACKUP_KEY_DIR`, `PASSPORT_TECHNICAL_BACKUP_KEY_FILE`, and `PASSPORT_TECHNICAL_BACKUP_DIR` to pre-existing private paths. The key directory and archive directory must be separate. The entrypoint copies the mounted key into a mode-`0600` file in its private `0700` tmpfs; it does not print the key. The backup includes technical identity, state, and idempotency metadata, but excludes photos, result payloads, OCR values, and paths. Caller-provided idempotency keys are included, so clients must use random opaque values and must never put personal, passport, or document text in them. Treat the encrypted archive as sensitive even though it contains no photo or OCR result bytes.

Restore only into a freshly migrated, empty target database. Stop every API/intake process first, set `PASSPORT_TECHNICAL_BACKUP_API_STOPPED=true` for the one-shot command, and retain that explicit acknowledgement in the command review. Do not set it as a Compose default:

```sh
docker compose stop api
PASSPORT_TECHNICAL_BACKUP_API_STOPPED=true \
  docker compose --profile ops run --rm technical-backup \
  technical-backup restore \
  --dsn-file /run/passport-ops/postgres_dsn \
  --key-file /run/passport-ops/backup_key \
  --input /run/backup/technical.pstb \
  --confirm-empty-target
```

Restore does not recover image or result payloads. Validate the target and its private image-root handling before restarting the API; never point the restore command at the live source database.
