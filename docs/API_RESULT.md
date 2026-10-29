# Публичная модель результата API

Это выбранная публичная модель ответа для находящегося в разработке API. OpenAPI 0.4.0 в [`../api/openapi.yaml`](../api/openapi.yaml) — нормативный источник контракта; примеры ниже дублируют его named examples для чтения. Публичные DTO/mapper в `internal/api`, HTTP handler library в `internal/httpapi` и внутренняя server composition в `internal/server` реализованы. Composition требует явные runtime limits и обязательный внедрённый `Recognizer`; запускаемого `cmd/api` и production recognizer wiring нет. Публичная DTO содержит только разрешённую часть данных внутренней модели Go.

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

`422 image_unprocessable` возвращается только когда внедрённый `Recognizer` сообщает `ErrImageUnprocessable`; отсутствие заполненных полей само по себе не даёт `422`. HTTP handler проверяет multipart-структуру и заявленный MIME type (`image/jpeg`, `image/png`, `image/webp`), но не декодирует изображение. Фактическая декодируемость, обработка снимка, качество распознавания, задержки и очистка ресурсов будущего recognizer/runtime здесь не подтверждены.

Выбранные коды HTTP-ошибок: `400 invalid_request`, `403 access_denied`, `413 upload_too_large`, `415 unsupported_media_type`, `422 image_unprocessable`, `429 concurrency_limit_reached`, `500 processing_failed`, `503 ocr_unavailable`, `504 processing_timeout`. Handler дополнительно использует `404 not_found` и `405 method_not_allowed` как router fallback вне операции POST; отдельные endpoint для них нет. Все написанные handler-ом ошибки имеют `request_id`, `code` и статическое безопасное `message`, а `X-Request-ID` совпадает с телом. Валидный одиночный входной UUID отражается; отсутствующий ID генерируется; невалидный или повторённый ID даёт `400` с новым UUID без отражения входного значения. Ошибки транспорта, возникающие до вызова handler (например, `431` для слишком больших заголовков или закрытие соединения при таймауте чтения), не гарантируют этот JSON envelope или `X-Request-ID`.

## Mapper obligations и реализация

`internal/api.MapExtraction` реализует проверку публичных форматов, согласованности состояний, response envelope, непустого настроенного `pipeline_version`, безопасное преобразование issue codes и all-null warning. `internal/httpapi.NewHandler` реализует HTTP handler library с внедряемым `Recognizer`, multipart/MIME/upload checks, корреляционным UUID, timeout и concurrency guard. `AllowedClientNetworks` в этой отдельно используемой library может быть пустым для сохранения прежнего поведения; если задан, список валидируется, нормализуется и копируется. ACL проверяет только числовой адрес прямого TCP peer, игнорирует forwarded headers и при отказе пишет `403 access_denied` до чтения body или захвата concurrency slot. `internal/server.New` требует непустой список CIDR, конкретный числовой локальный bind IP и обязательный recognizer; `Run` владеет TCP listener, который открывает, а `Serve` проверяет фактический адрес и владеет переданным listener. CIDR полностью ограничены loopback, RFC1918, IPv6 ULA и link-local диапазонами; значения задаёт caller, production defaults нет. Server composition однократна и при parent cancellation выполняет graceful drain с независимым shutdown timeout, после которого закрывает соединения. Read/write/header/idle/shutdown timeouts и `MaxHeaderBytes` обязательны и положительны; `ReadHeaderTimeout <= ReadTimeout`, а `WriteTimeout > ReadTimeout + OCRTimeout`. Проверка суммы не допускает переполнения; верхняя граница для `MaxHeaderBytes` защищает арифметику внутреннего `net/http` read limit, но не задаёт production limit. Лимит concurrency охватывает загрузку файла и recognizer-вызов.

Если внутреннее состояние `recognized` приходит с issue, mapper понижает его до `uncertain`; если `uncertain` не имеет причины, добавляется обобщённый `manual_review_required`. Если есть несколько разных допустимых канонических значений, mapper не выбирает победителя: публичное значение становится `null` и добавляется `conflicting_field_candidates`. Значение не восстанавливается из private candidate list.

Внутренний extractor остаётся богаче public: `image_id`, `schema_version`, raw, candidates, rules, evidence, context и MRZ diagnostics остаются только внутри. `issues` внутри extractor — строки, поэтому их нельзя напрямую выдавать вместо публичных объектов. Не каждый внутренний issue получает отдельный публичный код. Handler потоково разбирает multipart из ограниченного request body и держит в памяти только bytes единственного принятого файла для вызова `Recognizer`; временные файлы не создаются. В `internal/server` реализована внутренняя server composition, но нет запускаемой команды; фактическое декодирование изображения и recognizer/OCR runtime к API не подключены; подключение реального OCR отложено. Неструктурированный стандартный `http.Server.ErrorLog` отключён, поскольку panic-значение recognizer-а может содержать частные данные; безопасная structured telemetry и поведение логирования самого recognizer/runtime пока не определены.

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
