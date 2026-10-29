# Диагностика field ROI и интеграция кандидата

**Дата: 2026-10-29.** В документе разделены два ограниченных результата: raw OCR diagnostic на field ROI и последующее offline сравнение интеграции одного поддержанного name-кандидата в Go extractor.

## Raw OCR diagnostic

Статус: завершён. Проверено семь ROI на двух локальных development-фотографиях; этот этап предназначался для анализа OCR-кандидатов и сам по себе не менял алгоритм Go или выбранные значения.

- Для каждой фотографии разрешалось не более четырёх ROI и восьми вызовов readers. Фактически выполнено 7 ROI и 14 вызовов: 9 завершились с результатом `ok`, 5 вернули `empty`; ошибок и таймаутов не было.
- Использовались RapidOCR с закреплённой парой весов PP-OCRv5 и проверенным фактическим CPU provider для detector и recognizer, а также Tesseract с языком `rus`.
- Время CPJ-процесса составило 3,556 с. Это длительность задания, приведённая отдельно от времени вызовов readers; она не является сопоставимым benchmark распознавания полного изображения или end-to-end обработки.
- Эталон загружался после выполнения readers и использовался только для post-hoc диагностики. Эталон не участвовал в выборе кандидатов.

## Результаты по полям

`Role quality` описывает найденную поддержку строки/роли в ROI, а не точность значения. `Exact` означает точное совпадение после разрешённой нормализации всей группы кандидатов. `Token` означает наличие непрерывной последовательности целых токенов в разрешённой группе и не равен точному совпадению поля.

| Фото | Поле | Role quality | RapidOCR | Tesseract `rus` | Post-hoc exact | Post-hoc whole-token |
|---|---|---|---|---|---:|---:|
| sample-01 | given_name | supported_row | ok | empty | RapidOCR: да | RapidOCR: да |
| sample-01 | birth_place | weak | ok | ok | нет | нет |
| sample-01 | issuer | weak | ok | ok | нет | Tesseract: да |
| sample-02 | given_name | supported_row | ok | empty | RapidOCR: да | RapidOCR: да |
| sample-02 | gender | weak | empty | ok | нет | Tesseract: да |
| sample-02 | birth_place | weak | empty | empty | нет | нет |
| sample-02 | issuer | weak | ok | ok | нет | нет |

В обеих фотографиях точное whole-candidate совпадение найдено у RapidOCR только для `given_name` (2 reader-field диагностики). На sample-01 baseline `given_name` уже был правильным; на sample-02 он был неверным, а post-hoc проверка нашла точный кандидат RapidOCR. Это свидетельство о кандидате в raw ROI-срезе, не изменение baseline Go extraction. Остальные exact-проверки не совпали. Whole-token присутствие отмечено в четырёх диагностиках: оба `given_name` у RapidOCR, `issuer` на sample-01 у Tesseract и `gender` на sample-02 у Tesseract. Token-присутствие не подтверждает точное значение всего поля.

В sample-01 поле `gender` не получило ROI: гипотеза отклонена, потому что проверенная окрестность даты рождения недоступна. В sample-02 для `gender` Tesseract выдал три кандидатные группы при baseline-состоянии `missing`, но exact-сопоставления нет. Для `birth_place` в sample-02 оба readers вернули `empty`.

Большинство проверенных ролей получили оценку `weak`; `supported_row` получили только строки `given_name`. Поэтому совпадения кандидатов следует трактовать как диагностическое свидетельство для этих ROI, не как подтверждение точности или готовности к автозаполнению.

## Связь с предыдущим baseline

Предыдущая оценка baseline на двух development-фотографиях остаётся 15 правильных, 3 неверных и 2 отвергнутых/пропущенных предложения среди 20 обязательных полей. Этот raw ROI-прогон не пересчитывал выбранные Go-предложения и не менял число 15/20.

## Offline Go candidate integration

Для этого отдельного сравнения Python adapter собрал sidecar из уже сохранённых RapidOCR `rec_only` результатов. Новый OCR/model runtime не запускался. Go CLI принял по одному `supported_row` кандидату `given_name` на каждую фотографию и сравнил обычное cached extraction с extraction при необязательном sidecar.

**Ограничение provenance:** исторические `reader-results.json` не записывают фактический reader mode в каждой записи. Adapter проверяет `rec_only` по сохранённому плану, а также `entire_crop` geometry и source IDs; поэтому сам cache не аттестует фактический mode независимо. Cache не переписывался. Следующий live OCR запуск должен записывать mode в каждую reader-result запись. Это ограничение offline сравнения: кандидат остаётся `uncertain` и не выбирается автоматически.

| Фото | Baseline | С sidecar | Вывод |
|---|---|---|---|
| sample-01 | 1 кандидат; exact post-hoc candidate присутствует; `uncertain` | 1 кандидат; exact post-hoc candidate присутствует; `uncertain` | Та же одна правильная кандидатная запись, ручная проверка сохраняется. |
| sample-02 | 1 кандидат; exact post-hoc candidate отсутствует; `uncertain` | 2 кандидата; один exact candidate присутствует; `value: nil`; конфликт; `uncertain` | Точный кандидат появился, но конфликт оставляет поле без выбранного значения и требует ручной проверки. |

Таким образом, на sample-02 sidecar добавил exact-кандидат для ранее неверного baseline `given_name`, но не увеличил число правильно выбранных значений: итоговое поле осталось без `value` из-за конфликта. На sample-01 сохранилась одна правильная candidate group и соответствующее значение осталось `uncertain`. Ни одно поле не стало `recognized`; автозаполнение и выбор по эталону не выполнялись. Runner загружает gold только после построения sidecar и обоих Go extraction; exact-флаги являются post-hoc оценкой.

Baseline semantic JSON parity проверяется до сравнения: runner требует canonical JSON equality Go baseline extraction и сохранённого reference. Завершённый comparison report содержит оба sample, поэтому parity прошёл для обеих фотографий.

Этот narrow comparison охватывает только `given_name` на двух development-фотографиях. Он не пересчитывает общую метрику по 20 обязательным полям; утверждать новый суммарный счёт `15/2/3` нельзя. Предыдущий baseline остаётся 15/3/2. Число пройденных тестов в этот отчёт не включено: отдельные test logs здесь не проверялись.

В Go это экспериментальный offline CLI путь с необязательным private sidecar (`cmd/ocr-eval --field-evidence`), ограниченный поддержанной строкой имени. Он не меняет contract observations v1/v2 и не добавляет API v1/v2 или иной public API. Go сохраняет `uncertain` и не разрешает ROI-кандидату становиться признанным автоматически. В этом документе нет персональных значений, сырых OCR-строк или MRZ.
