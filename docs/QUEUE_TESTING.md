# RabbitMQ очередь и OCR consumer: проверки

Stage 4 acceptance использует реальный PostgreSQL и RabbitMQ в выделенном локальном Docker context. Harness не публикует порты, генерирует тестовые secrets в каталоге `0700` вне checkout, создаёт отдельный Compose project и удаляет только принадлежащие ему контейнеры и volumes. Не запускайте harness на удалённом TCP/SSH Docker endpoint: он передаёт file-backed secrets.

Для повторного запуска задайте явный локальный Unix-socket context и собственный каталог `0700` вне checkout:

```sh
PASSPORT_TEST_DOCKER_CONTEXT=<local-unix-socket-context> \
PASSPORT_TEST_SECRETS_ROOT=<owned-0700-directory-outside-checkout> \
  ./scripts/test-postgres-integration.sh
```

Публикация проверяется записью job и outbox через настоящий PostgreSQL repository, запуском настоящего Go relay и чтением сообщения из настоящей RabbitMQ quorum queue. Синтетическая фикстура не запускает OCR. Для проверки unroutable сценария harness сначала снимает единственный binding после того, как relay объявил topology, проверяет безопасный `publish_failed` и отсутствие `published_at`, затем восстанавливает binding и дожидается broker confirm. Повтор после искусственно потерянного DB acknowledgment выполняется только после фактического Rabbit publisher confirm и истечения lease; это fault-injection сценарий, а не симуляция сетевого отказа RabbitMQ.

RabbitMQ body должен иметь ровно `schema_version`, `event_id`, `job_id`, `attempt_no`; версия на wire — integer `1`, хотя сохранённая строка PostgreSQL остаётся `v1`. Проверяются persistent delivery mode, `application/json`, `MessageId == event_id`, отсутствие owner/image/path/OCR/token полей и связь сообщения с committed outbox row. Broker URL, пароль и DSN никогда не выводятся; ошибки relay сохраняют только фиксированную категорию.

Финальный harness прошёл 2026-10-31 на Docker Engine 29.5.2/aarch64, PostgreSQL `18.6-alpine3.24` и RabbitMQ `4.4.0-management-alpine`. Помимо relay публикации, confirm, unroutable, lease expiry и restart/reconnect, он связал реальный HTTP `POST /v1/passport/jobs` с PostgreSQL, Go relay, RabbitMQ, production Python consumer classes и Go private worker API. В контролируемом сценарии синтетический recognizer вернул все поля `null`; job сохранился как completed с одной attempt и canonical result, RabbitMQ очередь имела `messages_ready=0` и `messages_unacknowledged=0` после ACK.

Отдельный тест покрывает crash window после commit, но до AMQP ACK: test-only Go worker client получает реальный успешный ответ `/complete`, затем Python consumer процесс завершается через `os._exit(73)`, прежде чем Pika планирует `basic_ack`. Replacement consumer получает `redelivered=true`, запрашивает Go claim, получает ACK по terminal state и очищает очередь. DB проверка подтверждает неизменный canonical result hash, одну attempt и одну outbox row; guard processor доказывает отсутствие повторного recognition. Это не подменяет брокер или Go worker callback и не включает production stub/config.

Финальная проверка также запустила 20 последовательных повторов семи PostgreSQL execution lifecycle subtests на одной выделенной БД — все 20 прошли. Регрессия PostgreSQL `jobs_check5` выявила ненадёжное сравнение двух отдельных `clock_timestamp()`; terminal job SQL исправлен на один materialized DB timestamp, и direct future-timestamp regression для completed/failed/poison состояний прошёл.

Эти проверки не являются OCR semantic acceptance, accuracy оценкой, production load/SLO, multi-node broker HA, strict FIFO или deployment acceptance для существующего image root. Синтетический recognizer не проверяет распознавание документов и model assets; реальные документы и модели не передавались в test image или Git. GET job status отдельно проверен в Stage 5; RabbitMQ и OCR consumer для status-only запуска не требуются. Команда и фактические границы этого gate описаны в [TESTING.md](TESTING.md#get-статуса-задания-этап-5).
