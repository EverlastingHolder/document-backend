# Результаты локального PaddleOCR-VL-1.6 trial

**Дата:** 2026-10-29 · **Статус:** `COMPLETED_OBSERVED_ON_THIS_M1`

Trial завершил все зафиксированные этапы на одном Apple M1 host. Это наблюдение подтверждает запуск на этой конкретной машине, но не меняет статус vendor compatibility для Apple Silicon в целом: официальное руководство подтверждает M4, а M1 остаётся неподтверждённым.

## Среда и запуск

- `.venv/bin/python` 3.12.14 arm64 использовался только как launcher; зависимости установлены в отдельный `.venv-vl`.
- Фактические версии: `paddlepaddle==3.2.1`, `paddleocr[doc-parser]==3.7.0`, PaddleX 3.7.2, Python 3.12.14 arm64. PaddlePaddle CPU package использовался без CUDA (`cuda_compiled=false`).
- Выполнен локальный `PaddleOCRVL(pipeline_version="v1.6", device="cpu")` с layout detection по умолчанию (`true`). Remote inference service и передача изображений не использовались.
- Resource preflight, virtual environment, model-free checks, оба install stages, runtime probe, обе inference tasks и evaluation/report stage завершились успешно. Каждый из двух worker вызовов завершился с return code 0.
- Go/VLM adapter не запускался; trial не изменяет Go extraction, API или autofill.

## Входы и метрика

Использованы два существующих неизменяемых изображения (`sample-01`, `sample-02`). Сохранённые input SHA до и после trial совпали для обоих файлов. Gold был прочитан после сохранения обоих model outputs.

Метрика — наличие literal NFKC/casefold whole-token sequence внутри одного layout `block_content`. Токены между блоками не объединялись, confusable correction не применялся. Результат: **sample-01 — 5/10 обязательных полей; sample-02 — 5/10**.

Это мера наличия текста в отдельном документном блоке. Она не проверяет Go field selection и не является selected-field accuracy или качеством автозаполнения. Исторический `15/20` — отдельная метрика точных выбранных предложений полного конвейера OCR + Go field assembly; прямое сравнение с ним некорректно. Качество фактического прочтения отдельно оценивается `vl_text_quality_check`; по этому пилоту нельзя заключить, что модель лучше или хуже текущего OCR.

## Время и память

| Вход | Cold init, включая download/cache (с) | `predict` (с) | Worker wall (с) | Sampled worker RSS peak (KiB) |
|---|---:|---:|---:|---:|
| sample-01 | 149.243 | 65.443 | 218.729 | 9,555,888 |
| sample-02 | 9.226 | 60.505 | 73.780 | 9,666,592 |

Первичная инициализация включает холодное скачивание/заполнение кэша; второй вход использовал уже заполненный кэш. Worker wall включает import, init и predict. Внешний CPJ интервал составил 523.856 с (07:29:19.473–07:38:03.329 UTC), включая установку и загрузки; агрегированный отчёт записал собственный `total_wall_seconds` 523.744 с.

RSS снимался раз в 0.5 с только для worker процесса: около 9.1 GiB и 9.2 GiB соответственно. Это не полная системная память. На этом 16 GiB M1 профиль оказался тяжёлым по памяти и времени.

## Артефакты и границы

- Public aggregate: `../outputs/ocr-vl-comparison/paddleocr-vl-comparison.json` и `.md`; проверка ключей не обнаружила PII, raw OCR, gold values или block contents.
- Private manifests, model outputs, logs и cache находятся в `.local/ocr-research/work/ocr-vl-work/private/`; все каталоги имеют режим `0700`. Отчётные raw/log/manifest files имеют режим `0600`; Hugging Face cache оставил `.lock` files с режимом `0664` внутри каталогов `0700`.
- Два обязательных входа дали одинаковое значение первой метрики, но сам trial не устанавливает сравнительную точность. Отдельное ограниченное сравнение выбранных Go-полей приведено ниже; текущий pipeline остаётся default до оценки на более широкой и held-out выборке.

Отдельное сравнение cached PaddleOCR-VL blocks через текущий Go field extractor завершено; это уже метрика выбранных полей и она не меняет literal-presence результаты выше. См. [результаты Go comparison](OCR_VL_GO_RESULTS.md) и [краткую сводку](../outputs/ocr-vl-go-summary.md).

## Официальные источники

- [PaddleOCR-VL Apple Silicon guide, v3.7.0](https://www.paddleocr.ai/v3.7.0/en/version3.x/pipeline_usage/PaddleOCR-VL-Apple-Silicon.html) — официальный статус M4/M1 и CPU setup.
- [PaddleOCR-VL usage guide](https://www.paddleocr.ai/latest/en/version3.x/pipeline_usage/PaddleOCR-VL.html) — Python API, CPU invocation, pipeline version и layout defaults.
