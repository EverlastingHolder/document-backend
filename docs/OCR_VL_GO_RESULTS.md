# PaddleOCR-VL через Go field extractor

**Статус: ограниченное offline сравнение завершено.** На двух development-изображениях PaddleOCR-VL observations, адаптированные в строки Go handoff, дали меньше точных выбранных значений обязательных полей, чем incumbent observations при одинаковых настройках extractor. Это результат конкретной комбинации модели, adapter и текущих правил Go на двух примерах, а не универсальный рейтинг OCR-моделей.

## Объём и метод

Для обеих веток использован один Go extractor и одинаковые параметры: `--passes=all --normalize-names=true --mrz-policy=augment`. Incumbent observations были повторно обработаны как baseline; сохранённая семантика extraction совпала для обоих изображений. Все четыре Go запуска закончились до чтения gold. Использованы закэшированные native PaddleOCR-VL outputs: в этом сравнении модель и OCR повторно не запускались.

В каждом из двух документов учитывались 10 обязательных полей. Сравнивалось только точное совпадение выбранного значения после существующей канонизации `run_algorithm_comparison`; fuzzy matching и выбор кандидата по gold не применялись. Сырые OCR-строки, выбранные значения и эталонные значения остаются вне публичного отчёта.

| Ветка | Точные выбранные значения | Неверные предложения | Пропуск или abstain | False-recognized |
|---|---:|---:|---:|---:|
| Incumbent | 15/20 | 3 | 2 | 0 |
| PaddleOCR-VL + adapter | 9/20 | 4 | 7 | 0 |

По необязательному отчеству точное выбранное значение было в `0/2` incumbent cases и `1/2` PaddleOCR-VL cases. Оно не включено в знаменатель обязательных полей. Ноль false-recognized относится к этим запускам и не доказывает безопасность автоматического принятия полей. Все предложенные поля сохраняли состояния `uncertain` или `missing` и требуют человеческой проверки.

### Результаты по полям

В таблице ниже приведено число исходов по двум документам для каждого обязательного поля; сами значения не публикуются.

| Поле | Incumbent: верно / неверно / пропуск | PaddleOCR-VL: верно / неверно / пропуск |
|---|---:|---:|
| `surname` | 2 / 0 / 0 | 1 / 1 / 0 |
| `given_name` | 1 / 1 / 0 | 0 / 2 / 0 |
| `gender` | 1 / 0 / 1 | 0 / 0 / 2 |
| `birth_date` | 2 / 0 / 0 | 1 / 0 / 1 |
| `birth_place` | 0 / 1 / 1 | 0 / 1 / 1 |
| `series` | 2 / 0 / 0 | 1 / 0 / 1 |
| `number` | 2 / 0 / 0 | 2 / 0 / 0 |
| `issue_date` | 2 / 0 / 0 | 2 / 0 / 0 |
| `issuer` | 1 / 1 / 0 | 1 / 0 / 1 |
| `department_code` | 2 / 0 / 0 | 1 / 0 / 1 |

## Граница adapter и геометрии

- Каждый native VL text block передан как одна observation line. Adapter не делил block на придуманные слова или строки и сохранил исходный polygon.
- Передан один original-image pass; отдельного независимого MRZ pass не было. `mrz-policy=augment` работал по существующим правилам Go над переданными original-pass blocks.
- `score=0` — обязательный sentinel для недоступного значения, а не нулевая уверенность. Extractor этот score не использует.
- Document bounds — envelope принятых текстовых polygons, только кандидат OCR bbox envelope, а не физический контур документа. Seam остаётся `null`.

Следовательно, сравнение измеряет весь configured путь PaddleOCR-VL → adapter → Go extractor. Native whole-block text и геометрия грубее incumbent word/line observations; block granularity и отсутствие seam остаются слабостями сравнения. Прежние `5/10` на каждом изображении в [PaddleOCR-VL trial](OCR_VL_TRIAL.md) считали literal token presence внутри одного native layout block. Это другая метрика и не сопоставимо напрямую с точностью выбранных полей в этой таблице.

## Время и вывод

Это сравнение переиспользовало сохранённые PaddleOCR-VL outputs и не запускало OCR повторно. Исторический native `predict` занимал `65.443 с` для одного изображения и `60.505 с` для другого; sampled RSS worker-процесса составлял около `9.1` и `9.2 GiB`. Это замеры отдельного прежнего запуска на одном M1, не end-to-end время текущего Go сравнения. Текущие Go/adapter elapsed значения не включают OCR.

На этих двух development-изображениях текущие PaddleOCR-VL + adapter + Go дали `9/20` точных предложений против `15/20` у incumbent, на шесть меньше; пропусков было на пять больше. Поэтому текущий pipeline остаётся рабочим, а PaddleOCR-VL не становится default. Эти два случая не устанавливают held-out accuracy, production качество, преимущество или недостаток native recognizer вне этого adapter, а также общий рейтинг моделей.

Следующий диагностический шаг, если он будет нужен, — отдельно сравнить recognizer на заранее заданных ROI. Такое сравнение не проводилось. В этой работе не менялись Go code, schema, core или API.

## Артефакты

- [Публичный aggregate JSON](../outputs/ocr-vl-go-comparison/paddleocr-vl-go-comparison.json) и [читаемый aggregate report](../outputs/ocr-vl-go-comparison/paddleocr-vl-go-comparison.md).
- [Краткая сводка эксперимента](../outputs/ocr-vl-go-summary.md).

Публичные артефакты содержат агрегаты и per-field исходы, но не raw OCR, выбранные или gold values, source hashes либо private work paths.
