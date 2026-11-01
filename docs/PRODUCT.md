# Продукт и первая версия

## Цель

Поддержать в Android-приложении распознавание внутреннего российского паспорта по фотографии разворота с основными реквизитами: верхняя страница содержит сведения о выдаче, нижняя — фотографию и основную текстовую зону. Результат помогает заполнить форму, но пользователь проверяет и подтверждает каждое поле.

## Сценарий

1. Пользователь снимает разворот камерой или выбирает изображение из галереи.
2. До отправки он просматривает снимок, вручную корректирует кадрирование и поворот.
3. В выбранном пользователем локальном сетевом окружении клиент отправляет снимок на явно настроенные оператором локальный IP-адрес и порт Go backend. Сервер принимает только явно настроенные CIDR-диапазоны TCP-клиентов; общедоступный Internet ingress не входит в этот объём. Команда запуска существует, но endpoint не публикуется автоматически и Android-подключение с другой машины ещё не проверено.
4. Go provider проверяет изображение, передаёт его приватному RussianDocsOCR worker и возвращает предполагаемые поля вместе с состоянием каждого. Python runtime не имеет публичного порта.
5. Клиент показывает редактируемую форму проверки. Пользователь может исправить, очистить или подтвердить значения; все поля требуют человеческой проверки.
6. Пользователь подтверждает результат и экспортирует его. Предполагаемый формат и способ доставки экспорта ещё не определены.

## Асинхронный сценарий (этапы 3–7 приняты)

Текущий `POST /v1/passport/recognize` остаётся синхронным и возвращает завершённый результат с `200`. Durable intake нового `POST /v1/passport/jobs` прошёл bounded PostgreSQL/filesystem/HTTP acceptance; stage 4 outbox delivery, worker lifecycle, retry/recovery и ACK-loss redelivery прошли controlled integration acceptance. При настроенном `queue_processing` и работающем consumer задания могут обрабатываться после `202`; если включён только `async_jobs`, задание надёжно сохранится, но останется `queued`. Owner-scoped `GET /v1/passport/jobs/{job_id}` прошёл bounded real PostgreSQL/API acceptance и возвращает сохранённый status/result/error только владельцу; malformed, отсутствующий и чужой ID скрыты одинаковым `404`. `status_url` ведёт на доступный endpoint при включённой async-конфигурации. Ранее Android enrollment → upload → polling прошло connected synthetic HTTP fixture tests, сборки и запуск debug APK, но эта версия включала persistent job/result recovery. Текущая RAM-only политика прошла 73 JVM tests, 19 instrumented tests, lint и debug/release builds; прежний GET-only recovery case остаётся только историческим evidence. Это не live Android → Go → Python OCR и не production rollout. Отдельный bounded Go→Python Stage 8 run на private model snapshot и 100-request burst завершён. В ограниченном gold subset осталось три mismatch в `issuer`; broader fault matrix этим run не проверялась. См. [Stage 8 report](STAGE8_ACCEPTANCE.md); общая accuracy-оценка отсутствует. Подробности нормативного контракта находятся в [OpenAPI](../api/openapi.yaml), этапов — в [ASYNC_MIGRATION_PLAN.md](ASYNC_MIGRATION_PLAN.md).

Каждый job принадлежит principal из проверенного bearer credential. Android автоматически создаёт индивидуальный opaque secret `pbt1_` + 32 случайных байта base64url, сохраняет его в encrypted private storage и только затем передаёт backend lowercase SHA-256 через `POST /v1/client-installations`. Сервер возвращает стабильный случайный `client_id` и хранит только hash; повторная регистрация того же hash возвращает прежний ID и время создания. Это identity установки, а не пользовательский аккаунт или подтверждённая личность: login и общий token в APK отсутствуют. Опциональный статический hash registry остаётся для ранее provisioned клиентов. Enrollment ограничен прямым peer CIDR ACL и положительными операторскими caps; release traffic требует TLS. При переустановке с потерей защищённого секрета создаётся новая installation identity. `401` означает отсутствующий/невалидный credential; чужой и несуществующий job неразличимы и дают одинаковый `404` в GET status. Для повтора клиент передаёт owner-scoped `Idempotency-Key`, который должен быть случайным opaque техническим идентификатором без персонального или документного текста; совпадающий ключ с тем же digest возвращает ту же job, другой digest — `409`. RAM-only capture/upload/job/result policy прошла 73 JVM tests, 19 instrumented tests, lint и debug/release builds. Operational photo/document bytes, job state и result держатся в RAM; CameraX full-resolution capture использует in-memory callback без capture-файла, а receipt отправляется после применения result к UI. После process death восстановление и автоматический re-upload не выполняются; нужна новая scan operation. Явный 7z собирается в RAM и передаётся через private ContentProvider stream; проверка export прошла, а выбранный получатель может сохранить копию. Сохраняются только connection settings: server URL и скрытый installation credential; credential защищён Android Keystore. Документы, jobs, результаты и экспорт остаются RAM-only. Минимальные технические idempotency/fencing записи остаются на сервере; зашифрованный technical backup может содержать caller-supplied key, поэтому metadata и archive считаются чувствительными.

Stage 7 retention, receipt, revocation/caps, readiness, aggregate snapshot, and technical-backup checks passed in bounded PostgreSQL/filesystem/Compose runs. TLS configuration and trusted test-certificate rehearsal passed; a production domain/certificate was not activated. Durable configuration was promoted and the local API cut over to `192.168.1.91:8080` behind private Caddy; start/status/stop passed and the old listener is stopped. A bounded real-OCR 100-request burst was subsequently measured on one test VM; the small development sample and repeated-image workload do not establish general OCR accuracy or capacity. See [Stage 8 report](STAGE8_ACCEPTANCE.md). The asynchronous queue has no promised processing deadline; when no consumer is available, accepted jobs can remain queued. Android RAM-only behavior passed 73 JVM/19 instrumented tests, lint and debug/release builds; final APK installation on the user’s phone is deferred, so no physical-device install is claimed.

## Поля

В каноническом результате первого релиза присутствуют ровно эти 11 полей:

| Поле | Требование |
| --- | --- |
| `surname` | Фамилия |
| `given_name` | Имя |
| `patronymic` | Отчество, может отсутствовать |
| `gender` | Канонический код `M` или `F` |
| `birth_date` | Дата рождения |
| `birth_place` | Место рождения |
| `series` | Серия из 4 цифр, строка |
| `number` | Номер из 6 цифр, строка |
| `issue_date` | Дата выдачи |
| `issuer` | Кем выдан |
| `department_code` | Код подразделения в виде `000-000` |

Серия и номер остаются строками, чтобы не терять ведущие нули. Даты в API передаются как календарно допустимый `YYYY-MM-DD`. Имена нельзя автоматически переписывать по словарю или догадке. Неизвестное значение возвращается как `null`; сомнительное либо конфликтующее значение помечается для проверки. Для состояний `recognized`, `uncertain` и `missing`, а также формата публичных issues см. [модель ответа API](API_RESULT.md). Даже `recognized` требует проверки человеком; при конфликте без единственного допустимого кандидата backend возвращает `uncertain` с `value: null`, не выбирая произвольное значение.

## Результат и ответственность клиента

Backend возвращает кандидаты и состояния полей, а не подтверждённые паспортные данные. В первой версии нет endpoint для записи пользовательских исправлений. Исправленная клиентом каноническая копия с номером ревизии управляет показом, полнотой формы и экспортом. Поздний ответ распознавания игнорируется, если не совпадают идентификатор операции, снимок или ревизия.

Уверенность OCR не переносится на исправленное пользователем значение. Детали локальной модели доверия клиента и формата экспорта должны быть согласованы с Android-репозиторием.

## Открытые продуктовые вопросы

- Как определить дедлайн обработки и поведение для застрявшего job; текущий sync OCR timeout не задаёт async queue deadline.
- Локальный IP-адрес, порт и клиентские CIDR для конкретной установки.
- TLS/защищённый transport до передачи bearer token по недоверенной сети; CIDR ACL не шифрует трафик.
- Точный формат экспорта и способ доставки/передачи файла.
- Минимальная поддерживаемая версия Android.
- Ожидаемая нагрузка и целевые SLO.
- Ограничения размера и числа изображений, выбранные после тестирования.
- Согласие, хранение и доступ к тестовой коллекции паспортов; целевые job results не должны превращаться в общую историю сканов.
- Калибровка порогов неопределённости для конкретных полей.
