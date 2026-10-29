# Нативный OCR smoke

## Новое: шаблонные поля на зарегистрированных страницах

`template_field_pipeline.build_template_passport` — внутренний reusable маршрут для нового фото: физическая регистрация, OCR страниц, опорные строки, field crops. Он не принимает эталон. Результат содержит observations, lineage и необязательный `template_evidence_path`; этот sidecar передаётся текущему Go CLI через `--template-evidence`, вместе с `--passes=all --normalize-names=true --mrz-policy=augment`. Неподтверждённая регистрация отключает шаблон и сохраняет прежний OCR fallback.

`run_template_field_comparison.py --fresh --image ... --image ... --cache-root ... --work-root ... --output-dir ... --gold ...` проверяет полный маршрут и сравнивает его с сохранённым исходным OCR, заново обработанным тем же Go. Метки читаются только после шести Go-результатов. Без `--fresh` field OCR проверяется на сохранённых выровненных страницах. Это offline эксперимент, не публичный HTTP API.

Метод, private schema, результат 15/2/3 и ограничения: [OCR_TEMPLATE_FIELDS.md](../../docs/OCR_TEMPLATE_FIELDS.md). Описанный ниже smoke — отдельная историческая проверка моделей.

## Новый opt-in эксперимент: source tiles

Source-frame tiles расширяют внутренние observations для fallback-маршрута; они выключены по умолчанию и не подключены к production/API. [Метод, агрегированные результаты и ограничения](../../docs/OCR_SOURCE_TILES.md); формат observations v3 описан в [схеме](../schema/observations-v3.schema.json).

Короткий локальный smoke для двух кандидатов: RapidOCR 3.10.0 + ONNX Runtime 1.22.0 на CPU с готовыми PP-OCRv5 mobile моделями и Tesseract 5 с `rus.traineddata` из `tessdata_fast` 4.1.0. Скрипты не обращаются к hosted inference, не запускают контейнеры и не пытаются извлекать паспортные поля.

## Локальные данные и кэши

Приватные исследовательские входы, рабочие файлы, отчёты и PaddleOCR-VL cache по умолчанию находятся под игнорируемым каталогом `.local/ocr-research/`. При необходимости корень можно перенести через `PASSPORT_OCR_RESEARCH_ROOT`. Каталог PaddleOCR-VL можно задать отдельно через `PASSPORT_VL_MODEL_DIR`; RapidOCR/Tesseract assets используют игнорируемый `ocr/experiments/models/`, переопределяемый через `PASSPORT_RAPIDOCR_MODEL_DIR`. Имена файлов внутри `.local/ocr-research/work/` и `.local/ocr-research/outputs/` сохраняют прежнюю структуру архивированных экспериментов. Эти defaults не запускают OCR и не загружают модели.

## Запуск на синтетической fixture

Потребуется Python 3.10–3.13 и Homebrew. Если Tesseract не установлен, bootstrap выполнит только `brew install tesseract` с отключённым auto-update. Python пакеты устанавливаются в игнорируемый проектный `.venv`; ONNX веса и `rus.traineddata` сохраняются в игнорируемый `ocr/experiments/models/`. JSON фиксирует `pip freeze` и версии runtime для точного локального повтора.

```sh
python3 ocr/experiments/run_native_smoke.py \
  --python /absolute/path/to/python3 \
  --work-dir /absolute/path/to/private/scratch \
  --report-md /absolute/path/to/outputs/ocr-smoke-report.md \
  --report-json /absolute/path/to/outputs/ocr-smoke-report.json
```

Fixture создаётся локально из нейтральных русских строк, без реалистичных паспортных данных. Для обеих систем сохраняются распознанные строки, координаты/оценки и время; скрипт отдельно проверяет, что RapidOCR использовал только `CPUExecutionProvider`. JSON-отчёт может содержать сырой OCR-текст. В синтетическом режиме он безопасен для публикации вместе с Markdown-отчётом.

## Позже: локальный снимок

Передайте путь к изображению через `--image /path/to/image.png`. Исходник не копируется. Сырой JSON с результатом принудительно записывается в `<work-dir>/private-image-report.json`; Markdown содержит только сводку без распознанных строк. Не указывайте публичный каталог как `--work-dir`.

## Последовательный локальный batch

`run_test_batch.py` запускает синтетическую fixture первой, затем обрабатывает оба явно указанных локальных изображения, если хотя бы один OCR-движок прошёл synthetic этап. Используйте отдельный приватный `--work-root`; изображения не копируются. Перед повтором предыдущий неуспешный синтетический отчёт сохраняется в `ocr-smoke-first-failure/`. Для локальных изображений JSON и сокращённые диагностические хвосты OCR остаются в приватных рабочих каталогах; публичные Markdown и batch-сводка содержат только статусы движков без распознанных значений. Обёртка требует уже установленный точный набор pinned Python-пакетов и не запускает pip. Итоговый batch имеет статус `PASS` только если оба движка прошли synthetic и оба изображения; частичный результат остаётся `PARTIAL` и возвращает код 1.

## Последний локальный результат

Последний последовательный batch завершился `PASS` на macOS 27.0.1 arm64: synthetic fixture и два входа с идентификаторами `image-01` и `image-02` прошли для обоих движков с непустым результатом. RapidOCR 3.10.0 работал на ONNX Runtime 1.22.0 с `CPUExecutionProvider` для detector и recognizer; Tesseract 5.5.3 использовал `tessdata_fast` 4.1.0. SHA-256 закреплённых OCR-артефактов проверены. Времена и границы измерения записаны в [результатах OCR-пилота](../../docs/OCR_PILOT_RESULTS.md); per-image сводки без сырого текста доступны в [image-01](../../outputs/ocr-image-01-report.md) и [image-02](../../outputs/ocr-image-02-report.md). Это подтверждает выполнение движков, но не точность чтения паспортных полей.

Краткая история: первый запуск остановился на timeout загрузки модели; следующая проблема была в проверке выбранного RapidOCR provider; после этого Tesseract не формировал TSV в тестовой обёртке из-за конфигурации `tsv`. Причины исправлены и batch теперь проходит. Исторические сбои не являются текущим статусом harness. Для будущих ошибок формата или пустого TSV сокращённые хвосты stdout/stderr сохраняются только в `private-failure-diagnostics.json` приватного work-каталога.

```sh
python3 ocr/experiments/run_test_batch.py \
  --python /absolute/path/to/python3 \
  --work-root /absolute/path/to/private/work/ocr-passport-tests \
  --output-dir /absolute/path/to/outputs \
  --image /absolute/path/to/image-01.jpg \
  --image /absolute/path/to/image-02.jpg
```

Загрузка моделей ограничена тремя попытками по 120 секунд с короткой паузой; сначала используется apex `modelscope.cn`, затем проверенный `www.modelscope.cn` endpoint той же закреплённой ревизии. Проверка TLS и SHA-256 сохраняются. Повторы не гарантируют доступность удалённого источника.

## Что фиксировано

RapidOCR берёт только эти ModelScope v3.10.0 артефакты и проверяет SHA-256 до запуска:

- `ch_PP-OCRv5_det_mobile.onnx` — `4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae`.
- `eslav_PP-OCRv5_rec_mobile.onnx` — `08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab`.
- `rus.traineddata` загружается из неизменяемого тега `tessdata_fast` 4.1.0; фактический SHA-256 записывается в JSON.

В RapidOCR задаются явные local `Det.model_path` и `Rec.model_path`, версии `PP-OCRv5`, языки detector `ch` и recognizer `eslav`, `Global.use_cls=False` и выключаются CUDA, DirectML, CANN и CoreML. Так исключается выбор китайского recognizer по default config и случайная загрузка orientation classifier.

## Приватное сравнение экспериментов OCR и сборки полей

`run_algorithm_comparison.py` — отдельный локальный экспериментальный runner. В обычном режиме он вызывает уже установленный `region_pipeline` с одним CPU RapidOCR engine и сохраняет подробные observations и результаты Go extractor только в приватный `--work-root`; в `--output-dir` пишет агрегаты без OCR/gold значений. Go extractor запускается напрямую как собранный бинарник. Runner не устанавливает пакеты, не загружает модели, не отправляет изображения по сети и принимает ровно два явно переданных файла. Ground truth читается только для локальной оценки и не передаётся Go.

Для повторной проверки алгоритма Go можно передать `--reuse-observations`. До Go test/build runner проверяет для обоих файлов в `--work-root` соответствие opaque sample ID, SHA-256 исходного изображения, версии observations schema и закреплённых SHA-256 detector/recognizer моделей. При отсутствующем или несовпадающем кеше запуск завершается с ошибкой; OCR builder и модельный engine в этом режиме не загружаются. В отчёте отмечается, что observations переиспользованы, а время OCR не измерено в текущем запуске.

Перед запуском должны быть готовы Python pipeline и локальная команда `cmd/ocr-eval`. Используйте закреплённые Python 3.12 зависимости и уже кэшированные модели. Пути ниже намеренно задаются явно; замените их на доступные приватные пути машины:

```sh
/absolute/path/to/project/.venv/bin/python ocr/experiments/run_algorithm_comparison.py \\
  --python /absolute/path/to/project/.venv/bin/python \\
  --go /opt/homebrew/bin/go \\
  --work-root /absolute/path/to/private/ocr-algorithm-work \\
  --output-dir /absolute/path/to/outputs \\
  --image /absolute/path/to/image-01.jpg \\
  --image /absolute/path/to/image-02.jpg \\
  --gold /absolute/path/to/private/ground-truth.json \\
  --model-dir /absolute/path/to/project/ocr/experiments/models
```

Внутренний handoff описан в [observations-v1.schema.json](../schema/observations-v1.schema.json); координаты всех bbox/crop/quad задаются в пикселях исходного EXIF-нормализованного изображения. Сравниваются три режима: исходный pass без нормализации имён, исходный pass с отдельным предложением нормализации имён и все подготовленные passes с этой нормализацией. Raw text сохраняется в приватных файлах вместе с отдельным предложением, правилами и review state Go extractor. Эталон сравнивается без подмены смешанных кириллических/латинских символов и без fuzzy match.

Markdown/JSON отчёты содержат только имя варианта, анонимный ID изображения, статусы/счётчики и длительности. Полные observations, значения полей и детальные строки extractor остаются в каталоге 0700 с файлами 0600. Два фото — только development-примеры; их дельты не являются accuracy на отложенной выборке или production acceptance. До фактического запуска этот runner и схема не подтверждают результат эксперимента.

## Граница результата

Успешный smoke означает только, что обе системы обработали одну локальную синтетическую картинку и вернули строки; значение CER выводится как диагностическая метрика fixture. Это не тест точности на развороте российского паспорта, не оценка готовности к production и не сравнение качества на фотографиях.
