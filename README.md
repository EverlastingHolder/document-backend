# Passport Backend

Backend для Android-приложения распознавания российского паспорта. Подготовлены локальный экспериментальный Python OCR preprocessing и Go field-extraction prototype; ограниченное сравнение на двух development-изображениях завершено.

## Статус

Добавлен экспериментальный маршрут с шаблонными областями после регистрации страниц. На двух development-фото сохранены 15 правильных предложений; неверных стало 2 вместо 3, ещё одно поле оставлено конфликтным. Правильный вариант места рождения теперь присутствует среди кандидатов. См. [метод и ограничения](docs/OCR_TEMPLATE_FIELDS.md) и [сводку](outputs/ocr-template-summary.md).

Добавлен отдельный offline source-image pipeline с Go-планом автоматических ROI и PaddleOCR-VL. Planner v5d на пяти development-изображениях планирует 28/30 ROI, включая все 15 строковых ролей ФИО; это покрытие, не точность OCR. В after-v3 comparison baseline дал 27/9/14, `union` — 26/5/19, экспериментальный `primary` — 31/9/10 correct/wrong/abstain из 50 обязательных полей. Относительно after-v2 в `primary` correct и wrong увеличились на одно каждый; поэтому результат не является однозначным улучшением. Все поля остаются review-required, а выборка не held-out. Детали и ограничения — в [OCR_AUTO_VL.md](docs/OCR_AUTO_VL.md).

Доступны внутренний offline OCR handoff и экспериментальные команды для анализа полей. В парном registration-first pilot на двух development-фото обе ветки дали 15/20 точных предложений по обязательным полям; один кадр прошёл provisional registration, второй использовал fallback. Измеренного прироста точности не было, все значения требуют проверки человеком. См. [сводку пилота](outputs/ocr-registration-first-summary.md) и [результаты алгоритмического OCR-прототипа](docs/OCR_ALGORITHM_RESULTS.md). Go DTO/mapper, HTTP handler library и внутренняя server composition `internal/server` реализованы и проверены синтетическими тестами. Composition требует явно заданные limits, непустые клиентские CIDR, конкретный числовой локальный bind IP и внедрённый `Recognizer`; фильтрует прямой TCP peer по caller-supplied CIDR. `cmd/api`, production recognizer/OCR wiring (отложенный шаг), Android integration и deploy/runtime packaging отсутствуют. Это не runnable backend и не проверка реального LAN-развёртывания.

## Структура

- [AGENTS.md](AGENTS.md) — рабочие инструкции.
- [api/openapi.yaml](api/openapi.yaml) — выбранный публичный контракт API версии 0.4.0 в разработке; внутренняя server composition реализована, но команды запуска и production recognizer wiring нет.
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
- [docs/OCR_AUTO_VL.md](docs/OCR_AUTO_VL.md) — automatic Go-planned crop regions, v5d preflight and after-v3 comparison, private evidence validation and limitations.
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

Android-клиент находится в соседнем репозитории [`../passport-android`](../passport-android), если он доступен в текущем checkout. Его контракт и поток данных следует сверять с этим backend-документированием.

## Принципы

Публичные HTTP endpoints и orchestration принадлежат Go. Python OCR runtime — отдельный внутренний компонент для подготовки изображения и распознавания; основной кандидат для пилота RapidOCR с ONNX Runtime CPU и готовой парой весов PP-OCRv5 описан в [выборе модели](docs/MODEL_SELECTION.md). Цель — запуск на большинстве обычных настольных компьютеров и ноутбуков; совместимость будет проверяться для матрицы Windows, Linux и macOS на x86_64 и arm64 и не означает универсальную поддержку любого устройства. Оба компонента работают на одном компьютере; контейнеры — необязательный способ упаковки после проверки совместимости, возможен и нативный запуск. Используются только готовые предварительно обученные модели, без обучения или fine-tuning. CPU — обязательная отправная точка; GPU — необязательный режим, только если его поддерживают выбранные runtime и оборудование. Минимальные версии ОС, CPU и объём RAM будут определены по результатам пилота. Mac с Apple Silicon M1 и PC — лишь примеры машин для проверки.

Результаты распознавания требуют проверки человеком. Backend не должен вести историю сканов в первой версии, а фотографии и персональные данные нельзя писать в журналы.

## Локальный OCR smoke

Локальный pipeline и Go extractor проверены в ограниченных offline экспериментах. Сводки методов, состояния качества и ограничения смотрите в [результатах алгоритмического OCR-прототипа](docs/OCR_ALGORITHM_RESULTS.md), [диагностике OCR отдельных полей](docs/OCR_FIELD_ROI_RESULTS.md), [автоматическом VL ROI pipeline](docs/OCR_AUTO_VL.md), [narrow Go candidate integration report](outputs/ocr-field-evidence-comparison/field-evidence-comparison.md), [плане улучшения OCR](docs/OCR_IMPROVEMENT_PLAN.md) и [исследовании следующей архитектурной итерации](docs/OCR_ARCHITECTURE_RESEARCH.md). Технические проверки сами по себе не подтверждают качество.

Подготовлен отдельный [нативный smoke harness](ocr/experiments/README.md). Повторный локальный batch `2026-10-28` прошёл на synthetic fixture и двух фото с исправленным Tesseract TSV-парсером; целостность TSV подтверждена. Ранее измеренный нижний baseline буквальных совпадений описан в [результатах OCR-пилота](docs/OCR_PILOT_RESULTS.md), а новый алгоритмический прототип — в [отдельном отчёте](docs/OCR_ALGORITHM_RESULTS.md) и [публичной сводке](outputs/ocr-algorithm-summary.md). Эти проверки не устанавливают production-качество или глобального победителя.
