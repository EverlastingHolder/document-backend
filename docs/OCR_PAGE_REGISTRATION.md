# Экспериментальная регистрация страниц OCR

Следующий этап уже подключён отдельным маршрутом: [шаблонные области полей](OCR_TEMPLATE_FIELDS.md). Описанный ниже registration-only pilot сохранён как история; новый свежий срез с field OCR дал 15 правильных предложений, 2 неверных и 3 пропуска/конфликта вместо прежних 15/3/2.

**Статус: `EXPERIMENTAL_PAIRED_PILOT_COMPLETED`.** На двух development-изображениях завершён ограниченный парный registration-first OCR/Go срез: один sample прошёл provisional registration, второй ушёл в fallback. Точность предложений не изменилась: baseline и registration-first оба дали 15/20 точных предложений по обязательным полям. Это не оценка на held-out выборке и не подтверждение production-готовности.

## Метод и ограничения

- Физический кандидат строится из выпуклых contour-квадов. Edge support оценивается вдоль каждой стороны; paired strips внутри/снаружи добавляют цветовой контраст и асимметрию текстуры. OCR-box geometry остаётся диагностикой и не требуется для физической границы.
- Низкопороговые line-intersection hypotheses помогают объяснить слабые стороны, но остаются диагностическими и не могут стать принятым quad. OCR envelope, `minAreaRect`, sample-specific corners и эталон не используются. Несколько вложенных или сравнимых физических границ приводят к `ambiguous` и fallback.
- После provisional rectification seam ищется по протяжённому edge/contrast профилю; красный цвет — дополнительный cue, а не обязательный. Midpoint fallback отсутствует. Поддержанный seam mapped обратно в source frame и задаёт две отдельные page homographies с inverse transforms.
- Каждая page homography предполагает планарную панель. Кривизна страниц полноценно не оценивается: её остаточная ошибка неизвестна. Пространственные имена `image_upper`/`image_lower` не определяют стороны паспорта; orientation, включая поворот на 180°, остаётся `orientation_unverified`.
- Сохранённый derived canonical raster имеет собственные ID, размер и SHA-256. Его observations используют координаты этого raster; sidecar содержит отдельные per-page homographies для обратной привязки к source photo. Один global transform не заявляется. Для Go anchor extraction используется текущий internal observations contract, без публичного API изменения.
- Ориентация изображения по-прежнему `orientation_unverified`, в том числе возможный поворот на 180°. Верхняя/нижняя страницы означают только физические панели, не semantic паспортные стороны. Field ROI passes не запускались; текущая field extraction использовала существующий Go parser.

## История geometry-only probe

**Предыдущий вариант (v1).** На обоих development samples граница была отклонена с `edge_support_insufficient`, кандидатов 0; seam и OCR не запускались. Это был fail-closed результат.

**Геометрический вариант v2.** На sample-01 найден один contour-кандидат, поддержан seam (`seam_ratio ≈ 0.499`), построены две page homographies и inverse transforms. На sample-02 найдены вложенные contour-гипотезы с IoU `0.858`; локализатор вернул `ambiguous` и не выбрал более крупную рамку. Geometry-only probe тогда завершился со статусом `GEOMETRY_PARTIAL_OCR_PENDING`: OCR ещё не запускался. Позже выполнен отдельный парный OCR/Go pilot, описанный ниже.

Geometry probe использовал неизменённые development images, не загружал OCR/ORT/model и не запускал inference. Это геометрическая диагностика на двух примерах, не оценка качества распознавания или обобщения. Public report содержит только анонимные IDs, числовые метрики и статусы; изображения, PII, исходные пути и SHA остаются в приватных артефактах.

## Парный registration-first OCR/Go pilot

Run завершился с exit 0. В обоих arms использовались те же pinned RapidOCR PP-OCRv5 CPU модели и те же параметры Go: `passes=all`, `normalize_names=true`, `mrz_policy=augment`. Верифицированные detector/recognizer sessions работали через `CPUExecutionProvider`. Labels сопоставлялись post-hoc по SHA-256 файлов из `source_image_path`; исходные и canonical image hashes проверялись отдельно.

| Sample | Registration route | Baseline exact suggestions | Registration-first | Δ | Wrong suggestions | Missing/abstained required |
|---|---|---:|---:|---:|---:|---:|
| sample-01 | accepted; two canonical page passes succeeded | 9/10 | 9/10 | 0 | 1 | 0 |
| sample-02 | ambiguous; source-image fallback | 6/10 | 6/10 | 0 | 2 | 2 |
| **Total** | 1 accepted, 1 fallback | **15/20** | **15/20** | **0** | **3** | **2** |

Все неверные предложения остались `uncertain`; 0/20 обязательных значений получили состояние `recognized`. В sample-01 совпадающее число правильных предложений скрывает небольшое изменение rule attribution: число правил для полей серии снизилось с 2 до 1, а для номера — с 3 до 1. В sample-02 fallback observation и Go extraction были повторно использованы для обоих arms, поэтому этот sample не измеряет влияние регистрации. Ни sample не дал измеренного прироста точности.

Подробная сводка без OCR-текста и значений полей: [registration-first comparison](../outputs/ocr-registration-first-summary.md). Машиночитаемый публичный отчёт: [paired metrics JSON](../outputs/ocr-registration-first-comparison/registration-first-comparison.json).

## Проверки и артефакты

22 теста geometry-only `test_page_registration` и `test_registration_probe` прошли ранее (`exit 0`). В текущем handoff пилоте 7 fake-engine tests прошли (`exit 0`); они покрывают порядок registration-before-OCR, canonical-to-source round-trip на перспективной геометрии, private lineage и fallback при ambiguity/отсутствующей provenance. Сам пилот завершился (`exit 0`) на двух изображениях; этот технический статус не означает качества на новых документах.

- [Public geometry-only probe](../outputs/ocr-registration-v2-probe/registration-probe.md)
- [Краткая сводка v2](../outputs/ocr-registration-v2-summary-history.md)
- [Registration-first OCR/Go pilot](../outputs/ocr-registration-first-summary.md)
- [Public paired comparison report](../outputs/ocr-registration-first-comparison/registration-first-comparison.md)

## Следующий шаг

Результат не разрешает автоматически назначать физическим страницам semantic names или привязывать поля к фиксированным ROI. Ambiguity и неподдержанный источник геометрии ведут к source-image fallback. На двух development-фото нельзя делать вывод об обобщаемой field accuracy или production-пригодности; единственный принятый registration case не улучшил точные предложения относительно парного baseline.
