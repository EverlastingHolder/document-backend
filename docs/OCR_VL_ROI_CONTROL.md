# PaddleOCR-VL: ограниченный контроль на четырёх field ROI

**Статус: четыре post-hoc случая завершены; только диагностика.** Контроль использовал PaddleOCR-VL-1.6-0.9B в native CPU, с выключенными layout detection и document preprocessor. Prompt был `ocr`, generation cap — 512 новых токенов. Конфигурация и фактические настройки были проверены по завершении каждой задачи; неизменность model tree, runner/helper, manifest, source/crop bindings и хэшей vendor/case результатов также прошла проверку.

## Метод и границы

Проверены четыре вручную выбранные области: `issuer` и `birth_place` на `sample01`, `given_name` на `sample02`, `issuer` на `sample04`. Эти четыре field/photo пары взяты из пяти известных development-фото, не являются четырьмя независимыми документами и не оценивают end-to-end точность по 50 обязательным полям.

Scoring сравнивал всё поле целиком после NFC, case-folding, нормализации пробелов и закрытого gender mapping. У VL все vendor text blocks сохранены в исходном порядке и соединены пробелом; не удалялись слова или Markdown, не выбирался «лучший» block по эталону. Хотя vendor schema содержит box metadata, при выключенном layout измерение описывает только синтетическую рамку всего crop; word geometry не утверждается.

В четырёх случаях serializer не предоставил фактическую длину generation, поэтому cap flag равен `null`. Это не позволяет сделать вывод, что ответ не достиг лимита или содержит полный текст поля. В отчётах нет распознанных значений, текста эталона, имён исходных файлов, путей, хэшей или изображений.

Суммарное время четырёх запусков составило 118,704 с. Каждый case запускал отдельную инициализацию модели; per-case время 16,9–44,6 с включает инициализацию и не является latency одного passport.

## Результаты

Counts указаны как `correct / wrong / abstain`. RapidOCR и Tesseract отражают сохранённые standalone raw reader outputs на тех же ROI. Baseline Go — выбранное значение существующего full pipeline, пересчитанное по закреплённым historical artifacts и тем же reference source hashes; это другая стадия обработки, не raw reader output.

| Sample | Field | PaddleOCR-VL | RapidOCR PP-OCRv5 raw | Tesseract `rus` raw | Baseline Go selected |
|---|---|---|---|---|---|
| sample01 | issuer | correct | wrong | wrong | correct |
| sample01 | birth_place | correct | wrong | wrong | abstain |
| sample02 | given_name | correct | correct | wrong | wrong |
| sample04 | issuer | wrong | wrong | wrong | abstain |
| **Итого** | | **3/1/0** | **1/3/0** | **0/4/0** | **1/1/2** |

На `sample01` оба длинных поля совпали у VL; на `sample04` `issuer` оказался неверным у всех трёх readers, а Go baseline abstained. Для `sample02` standalone RapidOCR и VL верно прочитали `given_name`, тогда как baseline Go выбрал неверное значение. Это различия в четырёх выбранных crop и конфигурациях; они не устанавливают превосходство модели или качество полного pipeline на других изображениях.

Baseline полного pipeline остаётся 27/50 правильных обязательных полей; этот четырёхслучайный контроль не пересчитывает и не изменяет общий baseline. Production defaults, Go selection и API не менялись. Контроль не был подключён к production и не подтверждает автоматическое заполнение; все значения требуют проверки человеком. Точность на новых документах, автоматическая локализация ROI и качество на независимом наборе остаются неоценёнными.
