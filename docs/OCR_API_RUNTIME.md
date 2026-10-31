# OCR runtime API

## Текущий provider

Публичный HTTP API и response mapper принадлежат Go. `cmd/api -config <file>` запускает сервер, а `internal/ocr.Recognizer` при старте создаёт один приватный RussianDocsOCR worker. Worker не открывает сетевой порт и принимает запросы по newline-delimited JSON через stdin/stdout. Публичный OpenAPI сохраняет маршрут распознавания и `GET /v1/stats`.

Для каждой загрузки Go проверяет размер и заявленный MIME type, создаёт приватный job workspace с правами `0700`, записывает исходное изображение с правами `0600` и передаёт worker только локальный путь и request/image IDs. Worker проверяет изображение и возвращает значения фиксированного набора из 11 полей. Go нормализует пол, даты и код подразделения, передаёт результат через `internal/api.MapExtraction` и удаляет workspace после завершения. В HTTP response попадают только значения, состояния и безопасные issues. Каждое поле требует ручной проверки.

На macOS и Linux отмена запроса останавливает принадлежащую worker process group; при отмене worker запускается заново для следующих запросов. `Close(ctx)` блокирует новые jobs, отменяет активные jobs, закрывает worker и дожидается завершения в пределах shutdown deadline. Windows provider fail-closed с `503 ocr_unavailable`; cross-build не означает поддержку runtime.

Старый production provider на PaddleOCR-VL, его API worker, Go extraction/layout/ROI/MRZ integration и отдельные Go CLI для старого pipeline удалены. Сохранённые отчёты и OCR экспериментальные harness могут описывать прежние алгоритмы, но больше не являются запускаемым API runtime.

## Конфигурация

Текущий JSON config имеет один OCR provider, без поля `ocr.engine`. Обязательные `ocr` поля:

- `russian_docs_python` — исполняемый Python из подготовленного private environment;
- `russian_docs_worker` — `ocr/runtime/russiandocs_worker.py`;
- `russian_docs_repo` — локальный clone RussianDocsOCR;
- `russian_docs_models_root` — каталог `document_processing/models` с локальными весами;
- `russian_docs_startup_timeout` — предел загрузки worker и модели при старте;
- `work_root` — приватный каталог для временных job workspace.

Старые Paddle/VL ключи и `ocr.engine` являются неизвестными JSON полями и отклоняются. После перехода используйте новый `pipeline_version` и отдельный `metrics.state_path`, поскольку старый metrics state привязан к прежней версии pipeline. Не добавляйте clone, веса, Python environment, фотографии или config с локальными абсолютными путями в Git.

Сборка и запуск из корня репозитория:

```sh
go build -o .local/bin/passport-api ./cmd/api
./.local/bin/passport-api -config deploy/api.local.example.json
```

Пути в example разрешаются относительно каталога JSON config. Example предназначен для loopback и не принимает соединения с Android или других устройств. Для LAN настройки укажите конкретный локальный bind IP и разрешённый client CIDR в собственной приватной копии config.

## Logs, stats и privacy

`cmd/api` сохраняет privacy-filtered JSONL operational logs и cumulative `GET /v1/stats`. HTTP request ID связывает HTTP request и OCR job. Логи не содержат изображение, image hash, распознанный текст, персональные значения, filename, клиентский адрес или exception text. Worker stdout/stderr не сохраняются как сырой вывод.

Stats содержит накопительные HTTP/OCR счётчики, ошибки и фиксированные stage histogram buckets; RussianDocsOCR worker не передаёт отдельные события внутренних этапов, поэтому stage buckets остаются без наблюдений. Snapshot не является временным рядом и не хранит per-request историю. Logs и metrics state следует держать вне Git в приватных каталогах.

Ограничения ACL, response error mapping, log rotation, file permissions и stats persistence описаны в [архитектуре](ARCHITECTURE.md) и [testing notes](TESTING.md). Production deployment, identity authentication, TLS, remote Android integration и held-out OCR quality не подтверждены.
