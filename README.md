# Document Backend

Сервис извлекает структурированные данные из изображений документов через HTTP API и фоновые обработчики. Полученные значения являются кандидатами и требуют проверки человеком; результат может быть неполным или неточным.

## Запуск через Docker Compose

Нужны Docker Engine и плагин Docker Compose. Для первой сборки требуется доступ в сеть, чтобы скачать закреплённые runtime и модельные файлы. Go и Python на хосте устанавливать не требуется.

```sh
git clone https://github.com/EverlastingHolder/document-backend.git
cd document-backend/deploy/portable
docker compose up -d --build
```

HTTP endpoint публикуется по адресу `http://127.0.0.1:8080`. Состояние контейнеров и их health можно проверить командой:

```sh
docker compose ps
```

Для доступа из локальной сети скопируйте `deploy/portable/.env.example` в
`deploy/portable/.env` и задайте `PASSPORT_PUBLIC_BIND` равным частному IPv4
адресу Linux-хоста. Разрешите TCP 8080 в firewall только для доверенной
частной сети. Текущий debug APK можно направить на `http://<адрес-хоста>:8080`;
release APK требует HTTPS. LAN forwarding через WSL2 отдельно не проверен.

Остановить сервисы, сохранив данные в именованных volumes:

```sh
docker compose down
```

Не добавляйте `--volumes`, если нужно сохранить базу данных, credentials и хранимые данные.

Настройка LAN и другие параметры описаны в [руководстве по portable Compose](deploy/portable/README.md). Список API операций приведён в [контракте API](api/openapi.yaml).
