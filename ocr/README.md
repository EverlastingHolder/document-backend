# Внутренний OCR runtime

Публичный Go API запускает единственный production provider — приватный RussianDocsOCR worker `runtime/russiandocs_worker.py`. Worker не публикует HTTP-порт; Go владеет upload lifecycle, cancellation, job workspace, canonical field mapper, operational logs и stats.

Worker возвращает фиксированные значения 11 паспортных полей. Go нормализует даты, пол и код подразделения, сохраняет строки серии и номера (включая ведущие нули) и отправляет результат в публичный mapper. Ручная проверка обязательна для каждого поля.

Старый PaddleOCR/PaddleOCR-VL production worker и Go extraction, layout, ROI и MRZ integration удалены. Документы в `docs/OCR_*` и локальные research harness могут описывать исторические эксперименты; они не подключены к API. Реальные паспортные изображения, сырые OCR-строки, эталоны, веса и model caches не добавляйте в Git.
