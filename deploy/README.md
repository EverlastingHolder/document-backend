# Локальный запуск и LAN-конфигурация

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

## Runtime boundaries

Все uploads обрабатываются в приватном временном workspace и удаляются после success, error или cancellation. Logs и cumulative stats не содержат request-level OCR text или passport values. Подробности config, process lifecycle, платформ и limits см. в [OCR API runtime](../docs/OCR_API_RUNTIME.md).
