#!/usr/bin/env python3
"""Regression checks for parsing Tesseract's unquoted TSV output."""

from __future__ import annotations

import unittest

from smoke_worker import TESSERACT_TSV_COLUMNS, TesseractOutputFormatError, parse_tesseract_tsv


def tsv_row(text: str, left: int) -> str:
    values = ["5", "1", "1", "1", "1", "1", str(left), str(left), "20", "8", "91.5", text]
    return "\t".join(values)


class TesseractTsvParserTests(unittest.TestCase):
    def test_literal_quotes_do_not_merge_following_cyrillic_rows(self) -> None:
        output = "\t".join(TESSERACT_TSV_COLUMNS) + "\n"
        output += "\n".join(
            [
                tsv_row('"слово"', 10),
                tsv_row('сло"во', 30),
                tsv_row("второе", 50),
                tsv_row("третье", 70),
            ]
        )

        lines, metadata = parse_tesseract_tsv(output)

        self.assertEqual([line["text"] for line in lines], ['"слово"', 'сло"во', "второе", "третье"])
        self.assertEqual([line["box"] for line in lines], [[10, 10, 20, 8], [30, 30, 20, 8], [50, 50, 20, 8], [70, 70, 20, 8]])
        self.assertEqual(metadata["reader"], "csv.QUOTE_NONE")
        self.assertEqual(metadata["column_count"], 12)
        self.assertEqual(metadata["validated_data_rows"], 4)
        self.assertEqual(metadata["nonempty_text_rows"], 4)
        self.assertEqual(metadata["embedded_tsv_columns_in_text"], 0)

    def test_row_with_wrong_column_count_is_rejected(self) -> None:
        output = "\t".join(TESSERACT_TSV_COLUMNS) + "\n"
        output += tsv_row("слово", 10) + "\textra"

        with self.assertRaises(TesseractOutputFormatError):
            parse_tesseract_tsv(output)

    def test_header_with_wrong_column_count_is_rejected(self) -> None:
        output = "\t".join(TESSERACT_TSV_COLUMNS[:-1]) + "\n"

        with self.assertRaises(TesseractOutputFormatError):
            parse_tesseract_tsv(output)


if __name__ == "__main__":
    unittest.main()
