# Passport Backend

Backend для Android-приложения распознавания российского паспорта. Go API владеет публичным контрактом и каноническим результатом. Синхронный endpoint использует приватный RussianDocsOCR child process; async jobs используют отдельный Python RabbitMQ consumer через private Go worker API. Python services не публикуют публичный порт и не имеют PostgreSQL credentials. Все 11 полей остаются предложениями для ручной проверки. Качество на независимых документах и Android-доступ с другой машины не подтверждены.

## Локальный Docker quickstart

Клонируйте код и из каталога portable Compose соберите и запустите сервисы одной командой:

```sh
git clone https://github.com/EverlastingHolder/document-backend.git
cd document-backend/deploy/portable
docker compose up -d --build
```

Образы Go API и OCR собираются локально; registry login не требуется. OCR build получает закреплённый RussianDocsOCR source/model snapshot из публичного upstream и проверяет модели по lock manifest. Первый build требует сети и скачивает модели. По умолчанию HTTP доступен локально по `http://127.0.0.1:8080`.

Чтобы открыть endpoint на LAN, скопируйте `.env.example` рядом с Compose-файлом и задайте в локальном, не добавляемом в Git `.env` адрес своего сервера:

```sh
cp .env.example .env
# Set PASSPORT_PUBLIC_BIND=192.168.1.100 in .env
docker compose up -d --build
```

Замените пример на частный IPv4-адрес сервера. Ограничьте входящий TCP 8080 межсетевым экраном только нужным LAN. Windows Firewall должен разрешать порт только из частной сети и доверенного диапазона клиентов. URL для debug APK будет `http://192.168.1.100:8080`; текущая release-сборка Android требует HTTPS. Compose/WSL2 LAN-forwarding пока отдельно не проверен; эти инструкции не заявляют его полной поддержку.

Полные параметры, stop/status команды и границы проверки: [portable deployment README](deploy/portable/README.md).

Приватные OCR research outputs, изображения, диагностические материалы и веса моделей не публикуются в Git; связанные локальные отчёты отсутствуют в этом репозитории.

`cmd/api` пишет privacy-filtered JSON operational logs и отдельно сохраняет bounded cumulative statistics в приватный локальный state file. `GET /v1/stats` использует ту же прямую peer CIDR ACL, что и распознавание. Snapshot содержит только агрегаты HTTP/OCR/stages, без request history, request IDs, images, OCR text или PII. HTTP request ID по-прежнему связывает operational log request, OCR job и child-stage события; statistics requests не добавляются в HTTP counters. Подробности и границы проверок: [logging и runtime runbook](docs/OCR_API_RUNTIME.md).

На 2026-10-30 текущий локальный процесс был переведён на OpenAPI 0.5.0 и проверен через `GET /v1/stats`: state создан заново при запуске, persistence healthy, синтетический невалидный JPEG дал один `422` и один OCR failure, а stats polls не изменили HTTP/OCR counters. Проба была same-host и не проверяла Android с другого устройства. Snapshot содержит cumulative значения с момента первого старта metrics state; старые запросы не восстанавливаются.

## Статус

Этапы 3–5 async flow приняты на bounded real PostgreSQL/filesystem/HTTP/RabbitMQ integration. Stage 4 подтвердил outbox delivery, fenced lifecycle, retry/recovery и manual ACK. В ACK-loss case consumer завершился с кодом 73 после durable completion; redelivered message получил terminal ACK без второго OCR, новой attempt/outbox row или изменения canonical result. Предыдущие Stage 6 Android tests (67 JVM/16 connected) проверяли прежнюю схему с сохранением job/result и GET-only recovery; они остаются историческими. Текущая RAM-only коррекция capture/upload/job/result прошла 73 JVM tests, 19 instrumented tests, lint и debug/release builds: CameraX передаёт full-resolution image через in-memory callback без capture-файла; picker/upload/preview/job/result session data остаются в RAM; process death не восстанавливает и не отправляет job повторно; receipt отправляется после применения результата к UI. Проверены manager/Ktor/receipt/UI/editor flow и cleanup legacy pending state. Проверены явный export в 7z, сборка архива в RAM и передача через private ContentProvider stream; получатель может отдельно сохранить копию. Физическая установка финального APK на телефон отложена пользователем; эта проверка не является live Android → Go → Python OCR или OCR quality на private weights. Для обработки нужны `async_jobs` и `queue_processing` в Go config, а также отдельно запущенный consumer. При одном `async_jobs` новые задания durable принимаются, но остаются `queued`; без `async_jobs` POST возвращает `503`. Текущий `POST /v1/passport/recognize` сохраняет синхронный ответ `200` и по умолчанию по-прежнему требует внутренний Python RussianDocsOCR worker; его можно отключить только для queue-only deployment явным `sync_ocr_enabled: false`. Compose содержит внутренний `queue` profile для контролируемой integration проверки; он не является production acceptance. Stage 5 owner-scoped status GET прошёл real PostgreSQL/API acceptance.

Этап 6 backend enrollment прошёл реальную PostgreSQL/API проверку. Текущая RAM-only Android политика capture/upload/job/result проверена: 73 JVM tests, 19 instrumented tests, debug/release builds и lint прошли. CameraX full-resolution capture использует in-memory callback без capture-файла; picker/upload/preview/job/result state не сохраняется для восстановления после process death; receipt отправляется после применения результата к live UI. Явный 7z собирается в RAM и передаётся через private ContentProvider stream; получатель может сохранить экспортированную копию. Ранее принятые 67/16 tests с persistent job/result recovery относятся к прежней схеме и не используются как текущее доказательство. Финальная установка APK на телефон отложена пользователем. Сохраняется только connection profile: server URL и скрытый installation credential; credential хранится в encrypted Android Keystore. Документы, jobs, результаты и экспорт остаются RAM-only. Live Android → Go → Python OCR и проверка качества на private model snapshot остаются отдельными проверками. Документация контракта: [OpenAPI](api/openapi.yaml), [план миграции](docs/ASYNC_MIGRATION_PLAN.md) и [статус API](docs/API_STATUS.md).

Stage 7 implementation and local LAN acceptance are complete. Retention, receipt, revocation, readiness, and operational tooling passed bounded PostgreSQL/filesystem and Compose checks. The aggregate-only `passport-ops snapshot` passed in Compose; encrypted technical backup key generation/create/restore passed, including wrong-key and nonempty-target rejection. Worker startup and RabbitMQ connectivity were checked without image inference. TLS configuration and a trusted test-certificate rehearsal passed; a real production domain/certificate was not activated. The durable runtime was promoted under private `.local/async-runtime/candidate`; the API now runs at `192.168.1.91:8080` behind private Caddy, with the prior listener stopped. Start/status/stop and rollback preparation were checked; nine focused bridge tests passed. Candidate enrollment/replay/auth/revocation boundaries passed (`201`/`200`, hidden or unknown-owner access `404`, revoked credential `401`). Этап 8 затем прошёл ограниченный прогон реального Go→Python OCR и burst на 100 параллельных uploads; измерения и ограничения зафиксированы в [Stage 8 acceptance report](docs/STAGE8_ACCEPTANCE.md). Он не подтверждает общую точность или capacity вне этой конфигурации. Images are deleted after terminal OCR; canonical results are deleted on receipt ACK or after 15 minutes. Minimal owner/idempotency/attempt fencing rows remain and need aggregate DB-growth monitoring. Technical backups exclude image bytes, recognized fields/result payloads, raw bearer values, and image paths, but may contain caller-supplied idempotency keys; use random opaque keys without personal/document text and protect encrypted archives as sensitive. See [API status and receipt contract](docs/API_STATUS.md), [schema/retention notes](docs/POSTGRES_SCHEMA.md) and [deployment runbook](deploy/README.md).

Добавлен экспериментальный маршрут с шаблонными областями после регистрации страниц. На двух development-фото сохранены 15 правильных предложений; неверных стало 2 вместо 3, ещё одно поле оставлено конфликтным. Правильный вариант места рождения теперь присутствует среди кандидатов. См. [метод и ограничения](docs/OCR_TEMPLATE_FIELDS.md) и [сводку](outputs/ocr-template-summary.md).

Legacy synchronous provider запускает один приватный RussianDocsOCR worker при старте API и передаёт его структурированные поля через канонический Go mapper. Async consumer — отдельный Python process/service и использует тот же Go-owned mapper через private callback API. Старый Go layout/ROI/extraction pipeline и PaddleOCR-VL API worker удалены. Сохранённые сравнения OCR описывают исторические эксперименты и не являются исполняемой частью API. Детали runtime — в [OCR_API_RUNTIME.md](docs/OCR_API_RUNTIME.md) и [WORKER_RUNTIME.md](docs/WORKER_RUNTIME.md).

Доступны внутренний OCR handoff и экспериментальные команды для анализа полей. В парном registration-first pilot на двух development-фото обе ветки дали 15/20 точных предложений по обязательным полям; один кадр прошёл provisional registration, второй использовал fallback. Измеренного прироста точности не было, все значения требуют проверки человеком. См. [сводку пилота](outputs/ocr-registration-first-summary.md) и [результаты алгоритмического OCR-прототипа](docs/OCR_ALGORITHM_RESULTS.md). `cmd/api`, production recognizer composition и LAN ACL теперь реализованы; конфигурация требует явные лимиты, непустой client CIDR и конкретный числовой локальный bind IP, а handler проверяет прямой TCP peer. Локальные команды и границы проверки описаны в [runtime runbook](docs/OCR_API_RUNTIME.md). Android synthetic-fixture integration принята; live Android-to-backend OCR, общедоступное развёртывание и подтверждение качества не выполнены.

## Структура

- [AGENTS.md](AGENTS.md) — рабочие инструкции.
- [api/openapi.yaml](api/openapi.yaml) — публичный контракт API: sync recognition, cumulative `GET /v1/stats`, hash-only `POST /v1/client-installations`, async `POST /v1/passport/jobs`, owner-scoped status GET и receipt ACK (stages 3–7 accepted; the Stage 8 real-OCR/burst workload run is recorded in [report](docs/STAGE8_ACCEPTANCE.md), while OCR-quality and broader fault acceptance remain open).
- [docs/API_RESULT.md](docs/API_RESULT.md) — публичная модель ответа и реализованные правила DTO mapper.
- [docs/API_STATUS.md](docs/API_STATUS.md) — hash-only installation enrollment и чтение сохранённого статуса job/result snapshot.
- [docs/API_RESULT_PROPOSAL.md](docs/API_RESULT_PROPOSAL.md) — ссылка на закреплённую модель результата.
- [docs/PRODUCT.md](docs/PRODUCT.md) — первый пользовательский сценарий и требования.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — границы компонентов и поток данных.
- [docs/POSTGRES_SCHEMA.md](docs/POSTGRES_SCHEMA.md) — PostgreSQL installation/job/attempt/outbox schema, read-only readiness check и атомарная запись job+outbox для intake.
- [docs/WORKER_PROTOCOL.md](docs/WORKER_PROTOCOL.md) — private Go worker API and RabbitMQ envelope contract.
- [docs/WORKER_RUNTIME.md](docs/WORKER_RUNTIME.md) — Go/Python async worker configuration, timing, and runtime boundary.
- [docs/QUEUE_TESTING.md](docs/QUEUE_TESTING.md) — bounded real PostgreSQL/RabbitMQ delivery checks and their limits.
- [internal/server/server.go](internal/server/server.go) — внутренняя композиция HTTP handler, TCP listener и bounded shutdown.
- [docs/OCR.md](docs/OCR.md) — гипотезы по OCR, калибровка и вопросы пилота.
- [docs/OCR_TEMPLATE_FIELDS.md](docs/OCR_TEMPLATE_FIELDS.md) — шаблонные поля, локальные опоры, private Go evidence и свежий парный пилот.
- [docs/MODEL_SELECTION.md](docs/MODEL_SELECTION.md) — кандидаты готовой OCR-модели, runtime, источники и условия пилотной проверки.
- [docs/OCR_VL_TRIAL.md](docs/OCR_VL_TRIAL.md) — completed PaddleOCR-VL-1.6 CPU trial observed on one Apple M1; methodology, metrics, and limits.
- [PaddleOCR-VL trial summary](outputs/ocr-vl-trial.md) — observed results and resource costs; no model-quality verdict.
- [docs/OCR_VL_GO_RESULTS.md](docs/OCR_VL_GO_RESULTS.md) — cached PaddleOCR-VL blocks through the same Go field extractor as incumbent observations; limited two-image comparison and adapter boundaries.
- [PaddleOCR-VL + Go comparison summary](outputs/ocr-vl-go-summary.md) — aggregate selected-field results; no raw OCR or field values.
- [docs/OCR_AUTO_VL.md](docs/OCR_AUTO_VL.md) — historical automatic Go-planned crop comparison; its runtime and evaluator source are removed.
- [docs/OCR_VL_ROI_CONTROL.md](docs/OCR_VL_ROI_CONTROL.md) — separate post-hoc diagnostic on four manually selected ROI cases; not automatic-ROI coverage.
- [docs/TESTING.md](docs/TESTING.md) — будущая стратегия проверки.
- [docs/OCR_ALGORITHM_RESULTS.md](docs/OCR_ALGORITHM_RESULTS.md) — методы, прототипные изменения и статус ограниченного алгоритмического сравнения.
- [docs/OCR_FIELD_ROI_RESULTS.md](docs/OCR_FIELD_ROI_RESULTS.md) — raw ROI OCR diagnostic и отдельная offline интеграция supported-name кандидата в Go CLI.
- [docs/OCR_PAGE_REGISTRATION.md](docs/OCR_PAGE_REGISTRATION.md) — история geometry-only v2 и завершённый ограниченный registration-first OCR/Go pilot.
- [Geometry-only registration v2 summary](outputs/ocr-registration-v2-summary.md) — один provisional contour accepted, второй ambiguous; OCR не запускался.
- [Registration-first OCR/Go pilot summary](outputs/ocr-registration-first-summary.md) — paired result 15/20 для обоих arms; accuracy не улучшилась на двух development-фото.
- [Aggregate comparison of Go field-evidence candidates](outputs/ocr-field-evidence-comparison/field-evidence-comparison.md) — narrow offline comparison for `given_name`; optional CLI sidecar, no public API change.
- [docs/OCR_ARCHITECTURE_RESEARCH.md](docs/OCR_ARCHITECTURE_RESEARCH.md) — факты и предлагаемые следующие этапы MRZ/layout/ROI; не описание реализованных функций.
- [docs/PLAN.md](docs/PLAN.md) — этапы и текущий статус.
- [docs/ASYNC_MIGRATION_PLAN.md](docs/ASYNC_MIGRATION_PLAN.md) — этапы перехода к durable async jobs, текущий gate и оставшиеся границы.
- [docs/IMAGE_STORAGE.md](docs/IMAGE_STORAGE.md) — фактические границы и гарантии private `internal/imagestore`, включая квоту intake и single-writer ограничение.
- [docs/decisions/0001-repositories-and-go-api.md](docs/decisions/0001-repositories-and-go-api.md) — решение о репозиториях и публичном API.
- [ocr/README.md](ocr/README.md) — граница внутреннего OCR runtime.
- [ocr/experiments/README.md](ocr/experiments/README.md) — локальный нативный smoke RapidOCR/Tesseract на синтетической строковой fixture.
- [ocr/schema/observations-v1.schema.json](ocr/schema/observations-v1.schema.json) — внутренний handoff от OCR preprocessing к Go field assembler.
- [deploy/README.md](deploy/README.md) — предполагаемое развёртывание.
- [docs/OCR_API_RUNTIME.md](docs/OCR_API_RUNTIME.md) — текущий RussianDocsOCR provider, команды запуска, локальные ограничения и границы проверки.
- `internal/telemetry` — privacy-filtered JSON logs, file rotation и request correlation.

Android-клиент находится в соседнем репозитории [`../passport-android`](../passport-android), если он доступен в текущем checkout. Его контракт и поток данных следует сверять с этим backend-документированием.

## Принципы

Публичные HTTP endpoints и orchestration принадлежат Go. Python runtime доступен только через приватный worker bridge без отдельного публичного порта. В production используется только RussianDocsOCR. Старые PaddleOCR/VL и Go extraction сравнения сохранены как исторические отчёты и не подключаются к API. Текущая матрица платформ и границы проверки описаны в [runtime runbook](docs/OCR_API_RUNTIME.md).

Результаты распознавания требуют проверки человеком. Backend не должен вести историю сканов в первой версии, а фотографии и персональные данные нельзя писать в журналы.

## Локальный OCR smoke

Исторические offline сравнения и их ограничения сохранены в [архивных результатах OCR](docs/OCR_ALGORITHM_RESULTS.md), [диагностике OCR отдельных полей](docs/OCR_FIELD_ROI_RESULTS.md), [отчёте старого VL ROI pipeline](docs/OCR_AUTO_VL.md), [Go candidate comparison](outputs/ocr-field-evidence-comparison/field-evidence-comparison.md), [плане улучшения OCR](docs/OCR_IMPROVEMENT_PLAN.md) и [исследовании архитектуры](docs/OCR_ARCHITECTURE_RESEARCH.md). Старые Go evaluator команды и API route удалены; эти материалы не являются текущей проверкой RussianDocsOCR.

Подготовлен отдельный [нативный smoke harness](ocr/experiments/README.md). Повторный локальный batch `2026-10-28` прошёл на synthetic fixture и двух фото с исправленным Tesseract TSV-парсером; целостность TSV подтверждена. Ранее измеренный нижний baseline буквальных совпадений описан в [результатах OCR-пилота](docs/OCR_PILOT_RESULTS.md), а новый алгоритмический прототип — в [отдельном отчёте](docs/OCR_ALGORITHM_RESULTS.md) и [публичной сводке](outputs/ocr-algorithm-summary.md). Эти проверки не устанавливают production-качество или глобального победителя.
