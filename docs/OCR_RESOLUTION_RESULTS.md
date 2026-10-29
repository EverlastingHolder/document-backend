# Сравнение разрешения detector

## Объём и результат

Сравнены native RapidOCR `Det.limit_side_len=736, limit_type=min` и экспериментальный primary profile `1280/min` на двух development-фотографиях. Модельные веса и остальные настройки оставались закреплёнными. Это не held-out оценка и не обобщаемая accuracy.

| Профиль | sample-01: верно / неверно / abstain из 10 | sample-02: верно / неверно / abstain из 10 | Итого из 20 |
|---|---:|---:|---:|
| Native 736/min | 9 / 1 / 0 | 6 / 2 / 2 | 15 / 3 / 2 |
| Primary 1280/min | 9 / 1 / 0 | 6 / 1 / 3 | 15 / 2 / 3 |

Отчество — abstain/missing во всех четырёх запусках. Новых правильных обязательных полей при 1280 нет. На sample-01 выбор полей не изменился. На sample-02 изменилось только предложение `given_name`: при 736 оно опиралось на строки `lower_page-line-3` и `original-line-10` как один кандидат; при 1280 это стали два конфликтующих кандидата, `lower_page-line-3` из `lower_page` и `original-line-10` из `original`. Go extractor поставил `conflicting_field_candidates` и воздержался от выбора.

Отдельная post-hoc оценка сравнила каждый из этих двух кандидатов с эталоном теми же разрешёнными canonical rules: совпадающих кандидатов **0 из 2**. Это только анализ происхождения ошибки; эталон не участвует в production selection. Правило остаётся консервативным отказом при конфликте, без blind threshold fix.

Счёт правильных предложений не означает принятие результата: все поля имеют `uncertain` или `missing` и требуют ручной проверки. Таблица разделяет правильные предложения, неверные предложения и отказ/пропуск, не долю автоматически принятых значений.

## Профили, модели и проходы

Active ONNX sessions detector и recognizer использовали только `CPUExecutionProvider`. Native профиль — 736/min. В экспериментальном варианте primary 1280/min использовался на `original`, `upper_page` и `lower_page`; auxiliary 736/min — на правых margin-проходах и RapidOCR MRZ. У этих образцов отдельного OCR-прохода `document` нет: document bounds сегментированы в верхнюю и нижнюю страницы. Дополнительный `mrz_tesseract` — Tesseract, без ONNX detector profile. Было 7 настроенных проходов для sample-01 и 5 для sample-02.

Запуск завершился с exit 0 примерно за 20.5 с. Ранее проверенный pure suite из 33 тестов здесь повторно не запускался. Веса были проверены по закреплённым SHA-256:

- Detector: `4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae`
- Recognizer: `08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab`

Хеши используемых файлов `ocr/experiments/region_pipeline.py`, `ocr/experiments/run_resolution_comparison.py` и `ocr/experiments/run_algorithm_comparison.py` совпали с текущим checkout. После прогона создан отдельный private source snapshot из 16 явно относящихся Python/Go/schema файлов; его manifest содержит SHA-256 каждого файла. Хеши этих трёх источников совпадают между snapshot manifest и resolution report; snapshot не содержит результатов scoring, моделей, изображений или gold. Source image SHA совпали с observations и private input manifest; оба sample однозначно сопоставлены разным gold records. Scorer сравнивает уже выбранное значение extraction с gold после Go extraction; gold не передаётся в selection path.

## Время и размер detector input

Это единичные наблюдения. Время OCR включает чтение изображения и проходы, но не инициализацию движка/моделей; движки использовались повторно.

| Профиль | sample-01 OCR | sample-02 OCR | sample-01 Go process | sample-02 Go process |
|---|---:|---:|---:|---:|
| Native 736/min | 3.369 с | 3.874 с | 1.037 с | 0.007 с |
| Primary 1280/min | 4.610 с | 3.775 с | 0.008 с | 0.008 с |

Go-время замерено вокруг запуска evaluator процесса и включает startup и JSON I/O; это не extractor microbenchmark. Эти четыре наблюдения не обосновывают speed claim. Максимальные оценки размера detector input по профилям: native — 2.45 MP и 2.98 MP; 1280 с auxiliary 736 — 2.54 MP и 2.98 MP. Это оценка пикселей, не измерение peak RSS или RAM.

## Следующий шаг и границы

Сохранить 736/min как default. 1280/min оставить только experimental primary profile; auxiliary проходы остаются на 736/min. В двух development images 15/20 не изменилось; улучшение через одну меньшую неверную подсказку сопровождается одним дополнительным abstain, а не новыми правильными полями.

Следующая работа начинается с анализа ошибок на held-out реальных фотографиях, разрешённых для обработки, с разбиением по документам и разметкой нужных полей. После этого можно исследовать supervised-by-label local field regions/ROIs и оценивать неверные предложения, abstain и ручные исправления. Не выводить значения из словарей фамилий, имён или номеров. У геометрического probe на этих двух образцах оба результата были `rejected / edge_support_insufficient`, кандидатов 0 и OCR `not_run`; он не предоставляет ROI или основания для autofill.

Подробные агрегаты, тайминги и оценки detector pixel counts доступны в [публичном отчёте сравнения](../outputs/ocr-resolution-comparison/ocr-resolution-comparison.md). Raw OCR, extraction values, gold literals и input paths остаются в private work directory.
