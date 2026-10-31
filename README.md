# Passport Backend

Backend для Android-приложения распознавания российского паспорта. Go API использует единственный внутренний Python worker RussianDocsOCR; Python не публикует порт. Все 11 полей остаются предложениями для ручной проверки. Качество на независимых документах и Android-доступ с другой машины не подтверждены.

`cmd/api` пишет privacy-filtered JSON operational logs и отдельно сохраняет bounded cumulative statistics в приватный локальный state file. `GET /v1/stats` использует ту же прямую peer CIDR ACL, что и распознавание. Snapshot содержит только агрегаты HTTP/OCR/stages, без request history, request IDs, images, OCR text или PII. HTTP request ID по-прежнему связывает operational log request, OCR job и child-stage события; statistics requests не добавляются в HTTP counters. Подробности и границы проверок: [logging и runtime runbook](docs/OCR_API_RUNTIME.md).

На 2026-10-30 текущий локальный процесс был переведён на OpenAPI 0.5.0 и проверен через `GET /v1/stats`: state создан заново при запуске, persistence healthy, синтетический невалидный JPEG дал один `422` и один OCR failure, а stats polls не изменили HTTP/OCR counters. Проба была same-host и не проверяла Android с другого устройства. Snapshot содержит cumulative значения с момента первого старта metrics state; старые запросы не восстанавливаются.

## Статус

Добавлен экспериментальный маршрут с шаблонными областями после регистрации страниц. На двух development-фото сохранены 15 правильных предложений; неверных стало 2 вместо 3, ещё одно поле оставлено конфликтным. Правильный вариант места рождения теперь присутствует среди кандидатов. См. [метод и ограничения](docs/OCR_TEMPLATE_FIELDS.md) и [сводку](outputs/ocr-template-summary.md).

Production provider запускает один приватный RussianDocsOCR worker при старте API и передаёт его структурированные поля через канонический Go mapper. Старый Go layout/ROI/extraction pipeline и PaddleOCR-VL API worker удалены. Сохранённые сравнения OCR описывают исторические эксперименты и не являются исполняемой частью API. Детали текущего worker и локального запуска — в [OCR_API_RUNTIME.md](docs/OCR_API_RUNTIME.md).

Доступны внутренний OCR handoff и экспериментальные команды для анализа полей. В парном registration-first pilot на двух development-фото обе ветки дали 15/20 точных предложений по обязательным полям; один кадр прошёл provisional registration, второй использовал fallback. Измеренного прироста точности не было, все значения требуют проверки человеком. См. [сводку пилота](outputs/ocr-registration-first-summary.md) и [результаты алгоритмического OCR-прототипа](docs/OCR_ALGORITHM_RESULTS.md). `cmd/api`, production recognizer composition и LAN ACL теперь реализованы; конфигурация требует явные лимиты, непустой client CIDR и конкретный числовой локальный bind IP, а handler проверяет прямой TCP peer. Локальные команды и границы проверки описаны в [runtime runbook](docs/OCR_API_RUNTIME.md). Android integration, общедоступное развёртывание и подтверждение качества не выполнены.

## Структура

- [AGENTS.md](AGENTS.md) — рабочие инструкции.
- [api/openapi.yaml](api/openapi.yaml) — публичный контракт API версии 0.5.0 с аддитивным `GET /v1/stats` и прежним контрактом распознавания.
- [docs/API_RESULT.md](docs/API_RESULT.md) — публичная модель ответа и реализованные правила DTO mapper.
- [docs/API_RESULT_PROPOSAL.md](docs/API_RESULT_PROPOSAL.md) — ссылка на закреплённую модель результата.
- [docs/PRODUCT.md](docs/PRODUCT.md) — первый пользовательский сценарий и требования.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — границы компонентов и поток данных.
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
