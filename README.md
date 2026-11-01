# Document Backend

Сервис извлекает структурированные данные из изображений документов через HTTP API и фоновые обработчики. Полученные значения являются кандидатами и требуют проверки человеком; результат может быть неполным или неточным.

## Запуск через Docker Compose

Нужны Docker Engine и плагин Docker Compose. Первая сборка требует доступа в сеть для загрузки необходимых компонентов. Go и Python на хосте устанавливать не требуется.

```sh
git clone https://github.com/EverlastingHolder/document-backend.git
cd document-backend/deploy/portable
docker compose up -d --build
```

HTTP endpoint по умолчанию доступен на `http://127.0.0.1:8080`. Состояние сервисов и health можно проверить командой:

```sh
docker compose ps
```

Остановить сервисы, сохранив данные в именованных volumes:

```sh
docker compose down
```

Не добавляйте `--volumes`, если нужно сохранить данные. Настройка доступа из локальной сети описана в [руководстве по portable Compose](deploy/portable/README.md). Список API операций приведён в [контракте API](api/openapi.yaml).
