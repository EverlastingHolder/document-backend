# Автоматические ROI и PaddleOCR-VL

**Статус на 2026-10-29: planner v5d и парное after-v3 сравнение завершены; сравнение прошло evaluation guards.** After-v1 sidecars имели ошибку сериализации reader и не использовались в scoring; after-v2 reader прошёл проверку, но сравнение after-v2 относится к предыдущей версии ROI planner.

## Поток обработки

`ocr/experiments/run_auto_vl_passport.py` принимает одно исходное изображение и локальные пути к двум готовым OCR-моделям, PaddleOCR-VL и Go evaluator. Он выполняет внутренний OCR pipeline с попыткой регистрации страниц, строит Go extraction и отдельный Go ROI plan, читает только запланированные crop-области через PaddleOCR-VL, затем передаёт частное evidence обратно в Go для независимой проверки и merge. При принятой регистрации геометрия плана относится к canonical raster; в fallback — к EXIF-нормализованному исходному изображению. SHA и размеры изображения привязывают каждое представление к выбранному кадру.

Go планирует до шести полей в фиксированном порядке: `surname`, `given_name`, `patronymic`, `gender`, `birth_place`, `issuer`. Если надёжной области для поля нет, оно пропускается отдельно. Для ФИО planner объединяет геометрически совпадающие наблюдения из кандидатных строк, убирает повторы одной и той же строки и назначает роли по вертикальному порядку только при наличии одного согласованного блока ровно из трёх строк с подходящим интервалом и размером. Шаг строк может меняться; две строки или неоднозначный блок не позволяют угадать роли. Дата рождения служит локальной границей, если она наблюдается: строка даты, пересекающая или расположенная внутри блока, мешает назначению ролей. Отсутствие даты само по себе не запрещает планировать геометрически согласованный блок из трёх строк, но делает его рольовую интерпретацию менее надёжной. Это не гарантия автозаполнения; результат остаётся review-required.

Для `issuer` planner строит область по строкам текста самого значения и расширяет её вниз с умеренным отступом, опираясь на исходную высоту строк. Отдельные строки заголовка, подписи, даты и серийного номера не становятся частью тела поля. Небольшое пересечение прямоугольников строк допустимо, если центры строк остаются раздельными. Эти правила используют наблюдаемую геометрию, не координаты конкретных документов и не OCR-текст как источник gold.

Sidecar PaddleOCR-VL привязан к image identity, hash Go-плана, выбранным областям и идентичности дерева модели. Go заново строит план и сверяет sidecar с ним перед использованием. Геометрия чтения обозначает целый crop с синтетической рамкой prompt, а не реальные word- или glyph-boxes. PaddleOCR-VL score не считается уверенностью и не выбирает победителя. Результат остаётся предложением для проверки человеком.

## Геометрическое покрытие и итоговое сравнение

В preflight v5d Go planner прошёл parity-проверку на пяти изображениях, Python plan validator — на пяти планах. План содержит `28/30` возможных ROI; покрытие по пяти изображениям — `6/6/6/4/6`. Запланированы все 15 строковых ролей ФИО (по три на изображение) с геометрически соответствующими строками. Для `issuer` на всех пяти изображениях область включает тело значения и исключает отдельные строки заголовка, подписи, даты и серийного номера. На одном source-fallback изображении `gender` и `birth_place` пропущены из-за неоднозначной геометрии даты рождения. Все 28 областей получили непустое чтение, но crop-геометрия не гарантирует точность распознавания: после чтения остаются ошибки символов и визуального сопоставления букв. Это геометрическое покрытие, а не точность чтения; знаменатель обязательных полей остаётся `50`.

Предыдущий preflight v4 имел покрытие `19/30` (`6/3/6/1/3`); эти числа не описывают v5d. After-v3 comparison выполнен на тех же пяти development-изображениях и замороженных PP-OCR observations. Это ограниченная парная оценка, не held-out проверка.

### After-v3 comparison на planner v5d

На пяти development-изображениях замороженный baseline дал `27/9/14` correct/wrong/abstain (`54%` correct); parity точна, кроме порядка в множествах `line_ids` и `pass_ids`. C/W/A считает выбранные значения; manual-review flags пересекаются с этими исходами, а наличие correct candidate считается отдельно:

| Вариант | До C/W/A | После C/W/A | Correct | Переходы и замечания |
|---|---:|---:|---:|---|
| Baseline | 27/9/14 | — | 54% | 50 обязательных полей |
| `union` | 27/9/14 | 26/5/19 | 52% | относительно after-v2: correct без изменения, wrong −1, abstain +1 |
| `primary` | 27/9/14 | 31/9/10 | 62% | относительно after-v2: correct +1 и wrong +1, abstain −2; есть новые неверные предложения |

Число случаев, где правильный кандидат доступен, выросло `32→33`; эта метрика не равна выбранным правильным значениям. В `primary` переход от after-v2 к after-v3 дал два gains, одну loss и три новых wrong: неверный `issuer` на sample-05 исправлен, при этом появились новые неверные `surname` на sample-02 и `given_name` на sample-02 и sample-04. C/W/A `primary` по sample-01…sample-05 после обработки: `10/0/0`, `6/1/3`, `6/1/3`, `2/1/7`, `7/5/0`; sample-03 повторяет уже представленный документ. Для необязательного `patronymic` (вне обязательных 50) `primary` дал `5/0/0` correct/wrong/abstain против `2/0/3` в after-v2.

Все результаты остаются `review-required`. Переход `primary` к v5d не является однозначным улучшением: correct стало на одно больше, но wrong тоже стало на одно больше. Визуальная проверка ROI подтвердила правильное положение всех 15 геометрических строк ФИО, однако это не исключило ошибки чтения. Это ограниченное сравнение пяти изображений, не held-out качество или общее преимущество модели.

В after-v2 generic fresh-image smoke обработал один новый JPEG за `57.70` секунды: шесть reader regions были `ok`, три предложения прошли Go eligibility, результат остался review-required. Это один запуск, а не benchmark или latency estimate; он не является end-to-end оценкой нового planner. Парное сравнение пяти изображений использовало замороженные PP-OCR observations.

Четыре вручную выбранных ROI, описанных в [отдельном ROI-контроле](OCR_VL_ROI_CONTROL.md), — это постфактум диагностические случаи. Они не являются автоматическим планом и не объединяются с его покрытием или метриками. Предыдущее сравнение native PaddleOCR-VL blocks через Go extractor на двух development-изображениях описано отдельно в [Go results](OCR_VL_GO_RESULTS.md).

## Локальный запуск

Подготовьте локальные зависимости, модельные файлы и private output directory. Runner не принимает эталон или ручные координаты crop. Пример использует явные локальные пути и задаёт `union` вместо runner default `primary`:

```sh
go build -o /absolute/private/bin/ocr-eval ./cmd/ocr-eval

python3 ocr/experiments/run_auto_vl_passport.py \
  --image /absolute/private/input/passport-spread.jpg \
  --image-id local-document-01 \
  --output-dir /absolute/private/work/auto-vl-local-01 \
  --evaluator /absolute/private/bin/ocr-eval \
  --det-model /absolute/private/models/detector.onnx \
  --rec-model /absolute/private/models/recognizer.onnx \
  --vl-model-dir /absolute/private/models/PaddleOCR-VL-1.6 \
  --python-ocr /absolute/private/venv-ocr/bin/python \
  --python-vl /absolute/private/venv-vl/bin/python \
  --vl-policy union
```

Обязательные аргументы runner: `--image`, `--image-id`, `--output-dir`, `--evaluator`, `--det-model`, `--rec-model`, `--vl-model-dir`. Python paths имеют локальные defaults, которые при необходимости можно переопределить. Допустимы `--vl-policy primary|union`: это политика объединения VL и baseline в Go. Runner по умолчанию выбирает экспериментальный `primary`, а Go CLI/library — `union`. Выбор source image или registered canonical raster определяется результатом регистрации независимо от `--vl-policy`. Для текущих итоговых чисел `primary` — измеренный experimental arm, а не рекомендация для production.

Низкоуровневый CLI поддерживает раздельное создание плана и применение готового private evidence. Для применения sidecar нужны те же observations, параметры extraction и template evidence, а также ожидаемый SHA-256 дерева модели:

```sh
go run ./cmd/ocr-eval \
  --input /absolute/private/observations.json \
  --output /absolute/private/baseline.json \
  --template-evidence /absolute/private/template-evidence.json \
  --vl-plan-output /absolute/private/vl-plan.json \
  --passes=all --normalize-names=true --mrz-policy=augment

# После отдельного локального чтения crop-областей и создания sidecar:
go run ./cmd/ocr-eval \
  --input /absolute/private/observations.json \
  --output /absolute/private/final-extraction.json \
  --template-evidence /absolute/private/template-evidence.json \
  --vl-evidence /absolute/private/vl-evidence.json \
  --vl-model-sha256 "$MODEL_TREE_SHA256" \
  --vl-policy union \
  --passes=all --normalize-names=true --mrz-policy=augment
```

Если template evidence отсутствует, опустите этот флаг в обоих вызовах. Evidence reader должен использовать именно Go-план, созданный для тех же входных данных и настроек; произвольные crop-координаты CLI не принимает.

## Границы

Pipeline и sidecars предназначены для локального эксперимента. Они не подключены к публичному HTTP handler, не меняют OpenAPI или public response, не создают production recognizer и не являются командой запуска backend API. Использовалась готовая модель; обучение и fine-tuning не проводились. Пять development-изображений не заменяют held-out проверку на независимых документах; один образец повторяет уже представленное фото, а два source-fallback образца не имеют MRZ.

Для after-v3 прошли полный Go test suite и семь evaluation guards; Go plan прошёл parity на пяти изображениях, Python plan validator принял все пять планов, а все 28 запланированных областей получили непустое чтение. Ранее также прошли 11 reader и 5 generic-runner Python tests. Это проверки реализации и условий сравнения, а не доказательство качества на независимых документах.

OCR-derived sidecars и extraction могут содержать персональные данные. Храните work/output directories приватно; runner создаёт каталог с правами `0700` и файлы `0600`. Не добавляйте изображения, OCR-текст, значения полей или reference data в Git и публичные отчёты.
