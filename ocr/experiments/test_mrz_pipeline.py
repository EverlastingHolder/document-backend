"""Pure-Python checks for MRZ localization and Tesseract TSV handling."""

from __future__ import annotations

import unittest
from unittest.mock import patch

import numpy as np
import mrz_pipeline
from PIL import Image


def _line(text: str, y: int, *, pass_id: str = "original", x: int = 120, width: int = 760) -> dict[str, object]:
    return {
        "id": f"{pass_id}-{y}", "pass_id": pass_id, "text_raw": text, "score": 0.8,
        "quad": [[x, y], [x + width, y], [x + width, y + 18], [x, y + 18]],
    }


class MrzPipelineTests(unittest.TestCase):
    def test_anchored_pair_uses_union_padding_and_contains_both_rows(self) -> None:
        rows = [
            _line("PNRUS<<<<<<<<<<<<<<<<<<<<", 710),
            _line("1234567890<<<<<<<<<<<<<<", 742),
        ]
        crop = mrz_pipeline.locate_mrz_roi(
            image_size=(1000, 1000), document_bounds=(80, 100, 920, 900),
            lines=rows, document_method="contour",
        )
        x0, y0, x1, y1 = crop
        self.assertLessEqual(x0, 120)
        self.assertGreaterEqual(x1, 880)
        self.assertLessEqual(y0, 710)
        self.assertGreaterEqual(y1, 760)

    def test_anchoring_requires_pair_from_same_pass_and_lower_zone(self) -> None:
        rows = [
            _line("PNRUS<<<<<<<<<<<<<<<<<<<<", 710, pass_id="pass-a"),
            _line("1234567890<<<<<<<<<<<<<<", 742, pass_id="pass-b"),
        ]
        crop = mrz_pipeline.locate_mrz_roi(
            image_size=(1000, 1000), document_bounds=(80, 100, 920, 900),
            lines=rows, document_method="contour",
        )
        self.assertEqual(crop, (80, 628, 920, 900))

    def test_ocr_envelope_without_anchor_uses_wide_image_fallback(self) -> None:
        crop = mrz_pipeline.locate_mrz_roi(
            image_size=(1000, 1200), document_bounds=(200, 300, 760, 800),
            lines=[], document_method="ocr_bbox_envelope",
        )
        self.assertEqual(crop, (0, 696, 1000, 1200))
        self.assertLess(crop[0], 200)
        self.assertGreater(crop[2], 760)

    def test_anchored_roi_clamps_to_integer_image_bounds_without_cutting_rows(self) -> None:
        rows = [
            _line("PNRUS<<<<<<<<<<<<<<<<<<<<", 1144, x=900, width=100),
            _line("1234567890<<<<<<<<<<<<<<", 1176, x=900, width=100),
        ]
        crop = mrz_pipeline.locate_mrz_roi(
            image_size=(1000, 1200), document_bounds=(0, 0, 1000, 1200),
            lines=rows, document_method="contour",
        )
        x0, y0, x1, y1 = crop
        self.assertTrue(all(isinstance(value, int) for value in crop))
        self.assertGreaterEqual(x0, 0)
        self.assertGreaterEqual(y0, 0)
        self.assertLessEqual(x1, 1000)
        self.assertLessEqual(y1, 1200)
        for row in rows:
            xs = [point[0] for point in row["quad"]]
            ys = [point[1] for point in row["quad"]]
            self.assertGreaterEqual(min(xs), x0)
            self.assertGreaterEqual(min(ys), y0)
            self.assertLessEqual(max(xs), x1)
            self.assertLessEqual(max(ys), y1)

    def test_tsv_parser_keeps_raw_word_text_and_source_crop_geometry(self) -> None:
        header = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext"
        stdout = header + "\n5\t1\t1\t1\t1\t1\t12\t20\t44\t16\t87.5\tPNRUS<\n"
        rows = mrz_pipeline.parse_tesseract_tsv(stdout)
        self.assertEqual(rows[0]["text_raw"], "PNRUS<")
        self.assertEqual(rows[0]["score"], 0.875)
        self.assertEqual(rows[0]["quad"], [[12, 20], [56, 20], [56, 36], [12, 36]])

    def test_tsv_parser_rejects_wrong_column_count_without_echoing_text(self) -> None:
        with self.assertRaisesRegex(ValueError, "tesseract_tsv_invalid"):
            mrz_pipeline.parse_tesseract_tsv("level\ttext\n5\tPRIVATE DATA\n")

    def test_tesseract_upscale_bbox_is_mapped_back_to_original_crop(self) -> None:
        header = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext"
        stdout = header + "\n5\t1\t1\t1\t1\t1\t24\t40\t88\t32\t87.5\tPNRUS<\n"

        def fake_run(command: list[str], **kwargs: object) -> object:
            self.assertFalse(kwargs["shell"])
            self.assertEqual(kwargs["timeout"], 12.0)
            self.assertIn("-l", command)
            self.assertIn("eng", command)
            with Image.open(command[1]) as input_image:
                self.assertEqual(input_image.size, (80, 40))
            return type("Completed", (), {"returncode": 0, "stdout": stdout})()

        with patch.object(mrz_pipeline.subprocess, "run", side_effect=fake_run):
            rows, issue, _ = mrz_pipeline.run_tesseract_crop(
                "/installed/tesseract", np.zeros((20, 40, 3), dtype=np.uint8)
            )
        self.assertIsNone(issue)
        self.assertEqual(rows[0]["quad"], [[12.0, 20.0], [56.0, 20.0], [56.0, 36.0], [12.0, 36.0]])


if __name__ == "__main__":
    unittest.main()
