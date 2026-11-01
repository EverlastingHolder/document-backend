# Публичная модель результата API

Это описание текущего синхронного результата API, async job intake и status resource; OpenAPI в [`../api/openapi.yaml`](../api/openapi.yaml) — нормативный источник контракта, а примеры ниже дублируют named examples. DTO/mapper, HTTP handler, LAN server composition, `cmd/api` и internal RussianDocsOCR `Recognizer` реализованы. Результат sync provider и async consumer проходит `internal/api.MapExtraction`; публичная DTO содержит только разрешённые значения и статические issues. Stage 3 durable POST acceptance и stage 4 controlled delivery/recovery acceptance прошли bounded real PostgreSQL/filesystem/RabbitMQ/Go/Python harness. При настроенном `queue_processing` processing требует отдельно запущенного Python consumer; без него принятые jobs остаются queued. Stage 8 real-OCR/100-upload workload run завершён на выбранном private snapshot, но это не означает полного этапного acceptance: среди 14 проверенных gold fields три mismatch в `issuer`, а broader fault matrix не повторялась. Параметры и ограничения приведены в [Stage 8 report](STAGE8_ACCEPTANCE.md); общего accuracy/capacity claim нет. Stage 5 owner-scoped status read passed bounded real PostgreSQL/API acceptance, and stage 6 client polling passed the synthetic HTTP fixture gate. Stage 7 lifecycle/retention, receipt, revocation, readiness, snapshot, technical backup, and LAN cutover checks passed. Durable config promotion and local cutover to `192.168.1.91:8080` behind private Caddy passed start/status/stop; the prior listener is stopped. TLS configuration/test-certificate rehearsal passed, but production domain/certificate activation did not occur. Stage 8 bounded real-OCR/100-request workload run is recorded in [the report](STAGE8_ACCEPTANCE.md); full quality/fault acceptance remains open. Синхронный `POST /v1/passport/recognize` сохраняет прежний контракт.

## Успешный ответ

`POST /v1/passport/recognize` возвращает `200` после завершённой обработки, даже если ни одно поле не имеет ненулевого значения. Ответ всегда требует человеческой проверки. `warnings` всегда присутствует; без общих предупреждений это `[]`. Реализованный `MapExtraction` добавляет `no_fields_recognized` ровно тогда, когда после проверок все публичные `value` равны `null`. Эта условная связь с `fields` не кодируется одной только схемой OpenAPI.

Все 11 ключей `fields` обязательны: `surname`, `given_name`, `patronymic`, `gender`, `birth_date`, `birth_place`, `series`, `number`, `issue_date`, `issuer`, `department_code`. Дополнительные ключи запрещены. `patronymic` всегда присутствует, но `null` само по себе не устанавливает, присутствует ли отчество в исходном документе.

Ненулевые значения — непустые канонические строки. Реализованный Go mapper проверяет эти ограничения для `recognized` и `uncertain`: gender — `M` или `F`; даты — точный, календарно допустимый `YYYY-MM-DD`; серия и номер — ASCII-строки соответственно из 4 и 6 цифр с сохранением ведущих нулей; код подразделения — `000-000`. Имена, место рождения и орган выдачи не исправляются по словарю или догадке. Невалидное предложенное значение он скрывает и переводит поле в `missing` с безопасной общей причиной (для даты — `date_invalid_calendar_or_format`).

### Состояния полей

- `recognized`: непустой допустимый `value`, `issues: []`.
- `uncertain`: допустимая непустая строка-кандидат или `null`, и хотя бы один issue. При конфликте без единственного допустимого выбора значение `null`; случайный победитель не выбирается.
- `missing`: `value: null`; issues могут быть пустыми либо объяснять отбрасывание кандидата.

`recognized` не означает подтверждения пользователем. Состояния и issues не раскрывают внутренние OCR-данные.

### Публичная граница

Публичный ответ не содержит confidence percentages, raw OCR, кандидатов, evidence, context или MRZ-диагностики. Field issue codes: `conflicting_field_candidates`, `manual_review_required`, `date_invalid_calendar_or_format`. Recognition warning code: `no_fields_recognized`. Mapper сводит неизвестные внутренние причины к `manual_review_required`, используя статические публичные сообщения; внутренний текст, включая возможный PII, не копируется. Сообщения предназначены для показа пользователю и не разбираются клиентом как формат данных.

`422 image_unprocessable` возвращается только когда `Recognizer` сообщает `ErrImageUnprocessable`; отсутствие заполненных полей само по себе не даёт `422`. HTTP handler проверяет multipart-структуру и заявленный MIME type (`image/jpeg`, `image/png`, `image/webp`), а подключённый provider отдельно декодирует bytes, проверяет фактический формат и ограничивает размер изображения в пикселях. Full pipeline и cleanup проверены на одном native macOS HTTP integration case; это не held-out оценка качества или Android acceptance.

Для синхронного `POST /v1/passport/recognize` действуют коды `400 invalid_request`, `403 access_denied`, `413 upload_too_large`, `415 unsupported_media_type`, `422 image_unprocessable`, `429 concurrency_limit_reached`, `500 processing_failed`, `503 ocr_unavailable`, `504 processing_timeout`. Router использует `404 not_found` и `405 method_not_allowed` для неподдерживаемых routes/methods. Async `POST /v1/passport/jobs` добавляет `401 unauthorized`, `409 idempotency_conflict` и `503 job_acceptance_unavailable`; ошибки запроса и network ACL остаются `400`/`403`, upload MIME/size — `415`/`413`. Для GET status предусмотрены `401`, `403`, одинаковый `404` для malformed/unknown/foreign ID, `405 method_not_allowed` с `Allow: GET` и `503 job_status_unavailable`; все matched GET responses используют `Cache-Control: no-store`. Async intake ожидает свой semaphore с отменой по request context и не возвращает sync OCR `429`. Все JSON ошибки handler-а имеют `request_id`, `code` и статическое безопасное `message`, а `X-Request-ID` совпадает с телом. Транспортные ошибки до handler (например, `431` или закрытие соединения по read timeout) могут не иметь этого envelope.

## Async job intake и status resource

Async `POST /v1/passport/jobs` принимает ту же multipart-загрузку `file` и требует `Idempotency-Key` из 1–128 видимых ASCII символов. Клиент должен генерировать случайный opaque технический идентификатор и не включать в него персональный, паспортный или иной текст документа. Ключ уникален в пределах владельца: тот же ключ и тот же request digest возвращают тот же `job_id` и текущее сохранённое состояние; тот же ключ с другим digest даёт `409 idempotency_conflict`. Digest связывает точные image bytes и нормализованный MIME type. Подтверждённый `202` означает, что изображение сохранено, а запись задания и outbox зафиксированы одной транзакцией; это durable acceptance, не завершение OCR. Ответ содержит opaque UUID `job_id`, текущее на момент ответа состояние, `status_url` и `created_at`. При настроенном `queue_processing` Go relay публикует outbox и отдельно запущенный Python consumer может начать OCR; без этой конфигурации job остаётся `queued`. Stage 4 controlled delivery/recovery acceptance прошла, включая ACK-loss redelivery без второго OCR и изменения canonical result.

### Status resource

Реализованный `GET /v1/passport/jobs/{job_id}` читает только job, принадлежащую authenticated bearer principal, и возвращает `200` для каждого известного состояния. Детали validation сохранённого результата описаны в [API_STATUS.md](API_STATUS.md). Нормальный путь строится по одному owner-scoped read без блокировки или изменения job. В `completed` ответ включает исходный сохранённый `RecognitionResponse` после проверки его структуры и согласованности с job; в `failed` — только стабильный код ошибки и статическое безопасное сообщение. Для `queued`/`processing` результат отсутствует. Некорректный UUID, неизвестная job и job другого владельца дают одинаковый `404 not_found`; это не раскрывает существование чужих заданий. Ответы GET используют `Cache-Control: no-store`; неподдерживаемый метод даёт `405 method_not_allowed` с `Allow: GET`, а недоступность конфигурации/БД или timeout чтения — статический `503 job_status_unavailable`. Result удаляется после owner receipt ACK либо через фиксированные 15 минут после terminal state; image deletion starts after terminal OCR and is retried on cleanup failures. Технический owner/idempotency tombstone остаётся, чтобы повтор не создавал новое задание и не восстанавливал удалённый payload. `POST /v1/passport/jobs/{job_id}/receipt` возвращает `204` для первого и повторного ACK, `409 job_not_terminal` для nonterminal job и одинаковый `404 not_found` для malformed/missing/foreign IDs. После durable result purge GET отвечает `410 job_result_gone`, включая период до выполнения фоновой уборки. Физическое secure wipe при отказе носителя не обещается. Список/история заданий и cancel не предоставляются; произвольный polling interval и processing deadline не объявляются.

Stage 5 GET implementation and acceptance are complete. При включённой async-конфигурации `status_url` указывает на доступный polling endpoint. GET не меняет существующие синхронные `RecognitionResponse` и `ErrorResponse` contracts.

Целевые состояния — `queued`, `processing`, `completed`, `failed`. GET status возвращает `200` для любого принадлежащего клиенту состояния, включая `failed`; результат при `completed` повторно использует неизменённую `RecognitionResponse`, а `failed` содержит только безопасную стабильную категорию ошибки. Bearer token индивидуален для клиента/установки: `pbt1_` + 32 случайных байта в base64url, всего 48 символов; JWT и публичной регистрации нет. Android автоматически создаёт token при установке и отправляет только SHA-256 при enrollment; обычный flow не требует ручного token artifact. Опциональный legacy static registry хранит только token SHA-256 и owner; ротация может временно сохранить несколько token hashes для одного owner. Владелец берётся из проверенного auth principal, не из body, CIDR или forwarding header. Когда async конфигурация включена, `401` означает отсутствующий/невалидный credential; в выключенной конфигурации новый POST возвращает `503`. Storage capacity/DB/commit ambiguity также дают `503`; неизвестный commit не удаляет image. Malformed UUID, чужое и несуществующее задание дают одинаковый `404 not_found`. Результат удаляется после receipt ACK либо через 15 минут после terminal state; terminal images очищаются автоматически с повтором при filesystem failure. Stage 7 owner/privacy retention, receipts, revocation/caps, readiness, Compose snapshot, encrypted technical backup and local LAN cutover gates passed. Durable runtime config was promoted and the API cut over to `192.168.1.91:8080` behind private Caddy; start/status/stop passed and the old listener is stopped. TLS configuration/test-certificate rehearsal passed; production domain/certificate activation did not occur. Stage 8 bounded real OCR and 100-request burst passed for one test configuration; this is not a general OCR quality or capacity guarantee. See [Stage 8 report](STAGE8_ACCEPTANCE.md) for the measured scope and limits. There is no queued-job deadline/SLA. Подробный план перехода: [ASYNC_MIGRATION_PLAN.md](ASYNC_MIGRATION_PLAN.md).

## Mapper obligations и реализация

`internal/api.MapExtraction` реализует проверку публичных форматов, согласованности состояний, response envelope, непустого настроенного `pipeline_version`, безопасное преобразование issue codes и all-null warning. `internal/httpapi.NewHandler` реализует HTTP handler library с внедряемым `Recognizer`, multipart/MIME/upload checks, корреляционным UUID, timeout и concurrency guard. `AllowedClientNetworks` в этой отдельно используемой library может быть пустым для сохранения прежнего поведения; если задан, список валидируется, нормализуется и копируется. ACL проверяет только числовой адрес прямого TCP peer, игнорирует forwarded headers и при отказе пишет `403 access_denied` до чтения body или захвата concurrency slot. `internal/server.New` требует непустой список CIDR, конкретный числовой локальный bind IP и обязательный recognizer; `Run` владеет TCP listener, который открывает, а `Serve` проверяет фактический адрес и владеет переданным listener. CIDR полностью ограничены loopback, RFC1918, IPv6 ULA и link-local диапазонами; значения задаёт caller, production defaults нет. Server composition однократна и при parent cancellation выполняет graceful drain с независимым shutdown timeout, после которого закрывает соединения. Read/write/header/idle/shutdown timeouts и `MaxHeaderBytes` обязательны и положительны; `ReadHeaderTimeout <= ReadTimeout`, а `WriteTimeout > ReadTimeout + OCRTimeout`. Проверка суммы не допускает переполнения; верхняя граница для `MaxHeaderBytes` защищает арифметику внутреннего `net/http` read limit, но не задаёт production limit. Лимит concurrency охватывает загрузку файла и recognizer-вызов.

Если внутреннее состояние `recognized` приходит с issue, mapper понижает его до `uncertain`; если `uncertain` не имеет причины, добавляется обобщённый `manual_review_required`. Если есть несколько разных допустимых канонических значений, mapper не выбирает победителя: публичное значение становится `null` и добавляется `conflicting_field_candidates`. Значение не восстанавливается из private candidate list.

Внутренняя каноническая модель содержит только `image_id`, `schema_version`, поля, состояния, issues и допустимые кандидаты для разрешения конфликтов. `issues` остаются внутренними строками, поэтому mapper переводит их в безопасные публичные объекты. Handler потоково разбирает multipart из ограниченного request body и держит bytes принятого файла для вызова `Recognizer`; provider хранит изображение только в приватном workspace, запускает RussianDocsOCR worker и удаляет workspace при успехе, ошибке или отмене. Worker stdout/stderr не захватывается, а наружу передаются только публичные error categories.

CIDR фильтрация задаёт сетевое ограничение доступа для LAN-композиции, но не устанавливает identity пользователя; дополнительная identity-аутентификация, rate limiting, политика повторов, качество OCR и интеграция Android этой моделью не определяются. Handler ограничивает конкурентные запросы только заданной конфигурацией; выбранной QPS/rate-limit политики нет.

## Синтетические примеры

Все примеры используют вымышленные значения. `pipeline_version: "synthetic-draft-1"` — условный маркер, не версия реально развёрнутого pipeline. Их JSON-содержимое должно совпадать с `components.examples` в OpenAPI.

### Все поля имеют предложение

```json
{
  "request_id": "94b86c5c-2520-4c0f-b91e-6d4a948fc88a",
  "status": "completed",
  "review_required": true,
  "pipeline_version": "synthetic-draft-1",
  "fields": {
    "surname": {"value": "ИВАНОВ", "state": "recognized", "issues": []},
    "given_name": {"value": "АЛЕКСЕЙ", "state": "recognized", "issues": []},
    "patronymic": {"value": "ПЕТРОВИЧ", "state": "recognized", "issues": []},
    "gender": {"value": "M", "state": "recognized", "issues": []},
    "birth_date": {"value": "1990-04-12", "state": "recognized", "issues": []},
    "birth_place": {"value": "ГОРОД ПРИМЕР", "state": "recognized", "issues": []},
    "series": {"value": "0012", "state": "recognized", "issues": []},
    "number": {"value": "000345", "state": "recognized", "issues": []},
    "issue_date": {"value": "2010-06-18", "state": "recognized", "issues": []},
    "issuer": {"value": "ОТДЕЛ ПРИМЕРА", "state": "recognized", "issues": []},
    "department_code": {"value": "123-456", "state": "recognized", "issues": []}
  },
  "warnings": []
}
```

### Частичный результат

```json
{
  "request_id": "1a2ec415-66c5-4af1-ae13-9eb60e0fd827",
  "status": "completed",
  "review_required": true,
  "pipeline_version": "synthetic-draft-1",
  "fields": {
    "surname": {"value": "СИДОРОВ", "state": "recognized", "issues": []},
    "given_name": {"value": "МИХАИЛ", "state": "recognized", "issues": []},
    "patronymic": {"value": null, "state": "missing", "issues": []},
    "gender": {
      "value": "M",
      "state": "uncertain",
      "issues": [{"code": "manual_review_required", "message": "Проверьте распознанный пол."}]
    },
    "birth_date": {
      "value": null,
      "state": "uncertain",
      "issues": [{"code": "conflicting_field_candidates", "message": "Есть несколько вариантов даты рождения."}]
    },
    "birth_place": {"value": null, "state": "missing", "issues": []},
    "series": {"value": "0042", "state": "recognized", "issues": []},
    "number": {"value": "000781", "state": "recognized", "issues": []},
    "issue_date": {
      "value": null,
      "state": "missing",
      "issues": [{"code": "date_invalid_calendar_or_format", "message": "Не удалось получить допустимую дату выдачи."}]
    },
    "issuer": {
      "value": "ОТДЕЛЬНЫЙ УЧАСТОК ПРИМЕРА",
      "state": "uncertain",
      "issues": [{"code": "manual_review_required", "message": "Проверьте, кем выдан паспорт."}]
    },
    "department_code": {"value": "456-789", "state": "recognized", "issues": []}
  },
  "warnings": []
}
```

### Обработка завершена, ненулевых значений нет

```json
{
  "request_id": "8d83e8ea-aefc-4f01-b0b6-7dc8579bbef3",
  "status": "completed",
  "review_required": true,
  "pipeline_version": "synthetic-draft-1",
  "fields": {
    "surname": {"value": null, "state": "missing", "issues": []},
    "given_name": {"value": null, "state": "missing", "issues": []},
    "patronymic": {"value": null, "state": "missing", "issues": []},
    "gender": {"value": null, "state": "missing", "issues": []},
    "birth_date": {"value": null, "state": "missing", "issues": []},
    "birth_place": {"value": null, "state": "missing", "issues": []},
    "series": {"value": null, "state": "missing", "issues": []},
    "number": {"value": null, "state": "missing", "issues": []},
    "issue_date": {"value": null, "state": "missing", "issues": []},
    "issuer": {"value": null, "state": "missing", "issues": []},
    "department_code": {"value": null, "state": "missing", "issues": []}
  },
  "warnings": [
    {"code": "no_fields_recognized", "message": "Не удалось распознать поля. Попробуйте сделать новый снимок."}
  ]
}
```

### Ошибка обработки изображения

```json
{
  "request_id": "0bc5454f-4a34-4477-858d-49dd471cd3d4",
  "code": "image_unprocessable",
  "message": "Изображение не удалось обработать. Проверьте файл и попробуйте ещё раз."
}
```


### Receipt и автоматическая очистка (Stage 7)

По выбранной клиентской privacy policy operational фото/документ, job state и result не сохраняются на устройстве: они остаются в памяти текущего процесса. RAM-only capture/upload/job/result behavior прошёл 73 JVM tests, 19 instrumented tests, lint и debug/release builds. CameraX передаёт full-resolution image через in-memory callback без capture-файла; process death не восстанавливает и не отправляет job повторно. Явный 7z собирается в RAM и передаётся через private ContentProvider stream; получатель может отдельно сохранить копию. Клиент отправляет пустой `POST /v1/passport/jobs/{job_id}/receipt` после применения terminal response к живому UI state. Предыдущие tests с GET-only восстановлением после перезапуска относятся к superseded client policy. Backend receipt endpoint использует bearer owner principal, не принимает ID владельца, body, idempotency key или секрет. `204` подтверждает первый/повторный receipt; `409 job_not_terminal` сообщает, что результат ещё нельзя подтвердить; `404 not_found` скрывает отсутствующий, malformed и принадлежащий другому owner job. Все matched receipt/status ответы имеют `Cache-Control: no-store`. После receipt сервер очищает canonical result и status GET возвращает `410 job_result_gone`; без receipt результат удаляется спустя фиксированные 15 минут от terminal time. Изображение удаляется сразу после terminal решения OCR и при сбое filesystem удаление повторяет maintenance loop. Долговечные job/idempotency/fencing rows остаются как минимальное техническое состояние. Технический backup не содержит image bytes, canonical result payloads, raw bearer tokens или image paths, но может включать caller-supplied `Idempotency-Key`; архив шифруется, однако metadata следует считать чувствительными. Клиент обязан использовать случайный ключ без персонального или документного текста. Backup не защищает от потери единственного хоста и не восстанавливает payloads.
