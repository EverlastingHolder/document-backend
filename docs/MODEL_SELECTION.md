# Выбор модели и runtime для OCR-пилота

**Предложение от 2026-10-28:** основной кандидат для пилота — готовая пара PaddleOCR `PP-OCRv5_mobile_det` + `eslav_PP-OCRv5_mobile_rec`. Это выбор кандидата для сравнения на пилоте, а не production-решение и не подтверждение качества на развороте российского паспорта. Обучение и fine-tuning не планируются.

## Кандидаты

| Роль | Кандидат | Назначение и статус |
|---|---|---|
| Основная пара весов | PaddleOCR `PP-OCRv5_mobile_det` + `eslav_PP-OCRv5_mobile_rec` | Предварительно обученные детектор и русская модель распознавания текста. Карточки Hugging Face указывают Apache-2.0 для соответствующих моделей. Проверить лицензию и атрибуцию именно поставляемых файлов, совместимость и качество на пилоте. |
| Предпочтительный runtime для первой проверки | RapidOCR + ONNX Runtime CPU | RapidOCR предоставляет готовые ONNX-артефакты под алиасами `ch_PP-OCRv5_det_mobile` и `eslav_PP-OCRv5_rec_mobile`. Это упаковка RapidAI преобразованных весов PaddleOCR, а не независимая ONNX-модель, авторизованная PaddlePaddle. ONNX Runtime CPU — исходный режим; GPU не является требованием. |
| Независимый baseline | Tesseract 5 + `rus.traineddata` из `tessdata_fast` | Отдельный класс OCR для сравнительного baseline. `tessdata_best` можно отдельно проверить как вариант качества. Используются готовые обученные данные; переобучение не требуется. |

Не выбирать `server_rec` как русскую модель распознавания: для этого пилота указана `eslav_PP-OCRv5_mobile_rec`. Не считать RapidOCR runtime и модельные веса одной лицензируемой сущностью: фиксировать происхождение и лицензию wrapper/runtime, ONNX-артефактов и исходных model cards отдельно.

## Источники

- [PaddlePaddle/eslav_PP-OCRv5_mobile_rec — карточка модели и лицензия](https://huggingface.co/PaddlePaddle/eslav_PP-OCRv5_mobile_rec)
- [PaddlePaddle/PP-OCRv5_mobile_det — карточка модели и лицензия](https://huggingface.co/PaddlePaddle/PP-OCRv5_mobile_det)
- [Официальное описание поддержки языков PP-OCRv5](https://www.paddleocr.ai/latest/en/version3.x/algorithm/PP-OCRv5/PP-OCRv5_multi_languages.html)
- [Реестр RapidOCR с ONNX-алиасами и ожидаемыми SHA-256](https://github.com/RapidAI/RapidOCR/blob/main/python/rapidocr/default_models.yaml)
- [RapidOCR ONNX inference engine и чтение metadata](https://github.com/RapidAI/RapidOCR/tree/main/python/rapidocr/inference_engine/onnxruntime)
- [Детектор RapidOCR v3.10.0](https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/det/ch_PP-OCRv5_det_mobile.onnx)
- [Русский recognizer RapidOCR v3.10.0](https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/rec/eslav_PP-OCRv5_rec_mobile.onnx)
- [Документация RapidOCR: список моделей](https://github.com/RapidAI/RapidOCRDocs/blob/main/docs/model_list.md)
- [ONNX Runtime: установка Python-пакета](https://onnxruntime.ai/docs/get-started/with-python.html)
- [ONNX Runtime 1.22.0: опубликованные platform wheels](https://pypi.org/project/onnxruntime/1.22.0/)
- [ONNX Runtime 1.30.0: опубликованные platform wheels](https://pypi.org/project/onnxruntime/1.30.0/)
- [Tesseract `rus.traineddata` из tessdata_fast](https://github.com/tesseract-ocr/tessdata_fast/blob/main/rus.traineddata)
- [Tesseract: наборы traineddata и их различия](https://github.com/tesseract-ocr/tessdoc/blob/main/Data-Files.md)

Реестр RapidOCR документирует ожидаемые SHA-256 для `ch_PP-OCRv5_det_mobile` (`4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae`) и `eslav_PP-OCRv5_rec_mobile` (`08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab`). Это значения из реестра, а не результаты локальной загрузки или проверки; сами веса не загружались. ONNX-путь RapidOCR читает `character` из metadata; отдельный `.txt` словарь для этой связки не требуется. На пилоте всё равно нужно проверить наличие metadata и соответствие charset выходным logits.

## Ограничения и неизвестные

- Ни одна из перечисленных конфигураций не имеет доказанного production-качества на полном развороте российского паспорта. Консервативный baseline буквальных совпадений завершён на двух фото; метод и результаты описаны в [OCR_PILOT_RESULTS.md](OCR_PILOT_RESULTS.md). Это нижняя граница, не accuracy и не проверка автозаполнения. Широкая оценка ложных заполнений, пропусков, ручных исправлений и поведения на большем наборе не проводилась.
- Локальный runtime smoke подтверждён на одном macOS 27.0.1 arm64 хосте: RapidOCR 3.10.0 + ONNX Runtime 1.22.0 с CPU provider и Tesseract 5.5.3 с `tessdata_fast` 4.1.0 дали непустой вывод на synthetic fixture и двух входах. Диагностические времена приведены в [результатах OCR-пилота](OCR_PILOT_RESULTS.md); это не сравнение качества или production benchmark.
- Для следующего прототипа рекомендовано продолжить RapidOCR + ONNX Runtime CPU как основной кандидат, а текущий Tesseract PSM 6 оставить контрольным baseline. Это рекомендация для ограниченного пилота, а не глобальный выбор модели: PSM 3/11 и crop-обработка не проверялись, field association не подтверждена.
- Не выводить требования к RAM, минимальному CPU, задержке или необходимости GPU из размера весов либо описаний моделей. Их предстоит измерить на целевой матрице Windows/Linux/macOS и x86_64/arm64. Закреплять runtime-версии и конкретные выпуски ОС отдельно по платформам: колёса ONNX Runtime для каждой ОС и архитектуры различаются, нет основания предполагать один одинаковый пакет для всей матрицы. Поддержка старых ОС и Intel Mac автоматически не предполагается.
- Проверить точный preprocessing, декодирование и соответствие charset metadata выходам выбранной связки. Способ чтения metadata описан RapidOCR, но корректность конкретных артефактов и logits остаётся проверкой пилота.
- До распространения проверить лицензии и требуемую атрибуцию для runtime, преобразованных ONNX-файлов и исходных весов; карточки указанных PaddleOCR моделей сообщают Apache-2.0, но это не заменяет проверку всей поставляемой цепочки.
- GPU и другие ускорители остаются необязательным отдельным экспериментом только при поддержке выбранных runtime и оборудования. Преимущества ускорения не заявляются заранее.

## Условия решения после пилота

1. Расширить ограниченный двухфотографийный baseline на разрешённый набор, разделённый по документам на настройку и held-out оценку, и сравнить RapidOCR-кандидат с Tesseract PSM 6 baseline.
2. Проверить корректность детекции, preprocessing, charset metadata и декодирования; считать качество по каждому обязательному полю и по документу, включая ложные заполнения, пропуски и ручные исправления.
3. Измерить задержку и пиковую RAM на CPU для поддерживаемых сочетаний ОС/архитектуры. Отдельно измерять GPU только если runtime его поддерживает.
4. Проверить воспроизводимость по зафиксированным версиям пакетов, артефактов и хешам; сопоставить ожидаемые хеши RapidOCR с фактически полученными файлами при последующей загрузке.
5. После пилота принять или отклонить кандидата для production, закрепить точные версии, совместимость, лицензионные атрибуции и ресурсные минимумы. До этого он остаётся только кандидатом.
