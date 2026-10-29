# Локальные OCR research-инструменты

Эти runner’ы и scorer’ы сохраняют исторические offline-сравнения. Они не входят в runtime API и не являются production OCR pipeline. Их фиксированные sample-наборы, пороги и parity gates относятся к сохранённым исследованиям; запуск на другом наборе требует отдельного манифеста и не эквивалентен историческому сравнению.

Исходный код находится здесь, в репозитории. Исторические входы, манифесты, эталонные данные, приватные результаты и model cache хранятся отдельно в игнорируемом Git каталоге `.local/ocr-research/`. Код импортирует pipeline-модули из `ocr/experiments`; он никогда не импортируется из архивных файлов. По умолчанию рабочие данные ищутся под `.local/ocr-research/work/`, а приватные результаты записываются под `.local/ocr-research/outputs/`.

Для другого приватного каталога задайте `PASSPORT_OCR_RESEARCH_ROOT`. По умолчанию PaddleOCR-VL weights ожидаются в архиве; путь можно переопределить через `PASSPORT_VL_MODEL_DIR`. Пути к RapidOCR-моделям, Go, Tesseract и Python можно задать через `PASSPORT_RAPIDOCR_MODEL_DIR`, `PASSPORT_GO_BIN`, `PASSPORT_TESSERACT_BIN` и `PASSPORT_OCR_PYTHON`. Для VL-окружения предусмотрены `PASSPORT_OCR_VL_PYTHON` и `PASSPORT_OCR_VL_ROOT`.

Runner’ы выводят приватные результаты только в локальный архив. Перенос агрегированного отчёта в `outputs/` backend выполняется отдельно после проверки на персональные данные и локальные пути. Не запускайте исторические scorer’ы с произвольными входами: они предназначены для воспроизведения сохранённых сравнений, а не для универсального измерения качества.

## Инструменты

- `work/ocr-auto-vl-eval/` — bounded after-v3 automatic ROI comparison, scorer и guards.
- `work/ocr-five-image-eval/` — исторический five-image baseline runner/scorer и проверки scorer.
- `work/ocr-roi-diagnostic/` — ограниченный ручной ROI diagnostic и scorer.
- `work/ocr-roi-vl-control/` — отдельный VL control и scorer для ручных ROI случаев.
- `work/ocr-source-tiles/` — source-tile parity gate, runner, scorer и проверки scorer.

Все директории с исследованиями имеют приватные входы и результаты только в `.local/ocr-research/`; структура кода в `ocr/research/tools/` отделена от приватной структуры данных.
