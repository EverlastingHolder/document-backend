from __future__ import annotations

import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path

_CODE_ROOT = Path(__file__).resolve().parents[4]
if str(_CODE_ROOT) not in sys.path:
    sys.path.insert(0, str(_CODE_ROOT))
from ocr.experiments.local_paths import (
    ARCHIVE_OUTPUT_ROOT,
    ARCHIVE_WORK_ROOT,
    PRIVATE_RESEARCH_ROOT,
    RAPIDOCR_MODEL_DIR,
    REPO_ROOT,
    VL_MODEL_DIR,
)
from unittest.mock import patch

from PIL import Image
import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import run_roi_diagnostic as runner


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _entry(root: Path, sample: str, field: str) -> dict:
    source = root / f"{sample}.png"
    crop = root / f"{sample}-{field}.png"
    Image.new("RGB", (100, 80), (70, 80, 90)).save(source)
    Image.new("RGB", (20, 10), (70, 80, 90)).save(crop)
    return {
        "sample_id": sample,
        "field_id": field,
        "source_sha256": _sha(source),
        "source_image_path": source.name,
        "source_width": 100,
        "source_height": 80,
        "crop": [10, 5, 30, 15],
        "crop_image_path": crop.name,
        "crop_sha256": _sha(crop),
        "geometry_provenance": "manual_visual_review",
    }


def _manifest(root: Path) -> dict:
    entries = []
    fields_by_sample = {
        "sample01": ("issuer", "birth_place"),
        "sample02": ("issuer", "birth_place", "given_name", "gender"),
        "sample03": ("issuer", "birth_place"),
        "sample04": ("issuer", "birth_place", "given_name", "gender"),
        "sample05": ("issuer", "birth_place"),
    }
    for sample, fields in fields_by_sample.items():
        for field in fields:
            entries.append(_entry(root, sample, field))
    return {"schema_version": runner.SCHEMA, "purpose": runner.PURPOSE, "crops": entries}


class ROIDiagnosticRunnerTests(unittest.TestCase):
    def test_manifest_contract_accepts_fourteen_unique_tasks(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            manifest_path = root / "crops.json"
            manifest_path.write_text(json.dumps(_manifest(root)), encoding="utf-8")
            entries = runner._load_manifest(manifest_path)
            self.assertEqual(14, len(entries))
            self.assertEqual([10, 5, 30, 15], entries[0]["crop"])

    def test_manifest_rejects_duplicate_sample_field_pair(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            manifest = _manifest(root)
            manifest["crops"][-1] = dict(manifest["crops"][0])
            manifest_path = root / "crops.json"
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaises(runner.DiagnosticError) as error:
                runner._load_manifest(manifest_path)
            self.assertEqual("manifest_task_duplicate", error.exception.code)

    def test_input_binds_raw_source_crop_hashes_and_exif_dimensions(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            entry = _entry(root, "sample01", "issuer")
            manifest_path = root / "crops.json"
            _, _, rgb, binding = runner._validate_input(entry, manifest_path)
            self.assertEqual((10, 20, 3), rgb.shape)
            self.assertEqual(entry["source_sha256"], binding["source_sha256"])
            self.assertEqual([100, 80], binding["source_dimensions_exif_normalized"])
            self.assertEqual([10, 5, 30, 15], binding["crop_box_source_xyxy"])

    def test_changed_crop_is_rejected_before_reader(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            entry = _entry(root, "sample01", "issuer")
            (root / entry["crop_image_path"]).write_bytes(b"changed")
            with self.assertRaises(runner.DiagnosticError) as error:
                runner._validate_input(entry, root / "crops.json")
            self.assertEqual("crop_sha_mismatch", error.exception.code)

    def test_crop_pixels_must_match_selected_source_rectangle(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            entry = _entry(root, "sample01", "issuer")
            crop_path = root / entry["crop_image_path"]
            Image.new("RGB", (20, 10), (1, 2, 3)).save(crop_path)
            entry["crop_sha256"] = _sha(crop_path)
            with self.assertRaises(runner.DiagnosticError) as error:
                runner._validate_input(entry, root / "crops.json")
            self.assertEqual("crop_pixels_do_not_match_source_rectangle", error.exception.code)

    def test_tesseract_tsv_groups_rows_and_preserves_empty_confidence(self) -> None:
        tsv = (
            "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n"
            "5\t1\t1\t1\t2\t1\t10\t40\t70\t12\t-1\tПЕТЕРБУРГ\n"
            "5\t1\t1\t1\t1\t1\t8\t10\t55\t12\t81.25\tСАНКТ\n"
            "5\t1\t1\t1\t2\t2\t90\t40\t20\t12\t0\t-\n"
            "5\t1\t1\t1\t2\t3\t120\t40\t20\t12\t\tСЛОВО\n"
        )
        result = runner._parse_tesseract_tsv(tsv)
        self.assertEqual("САНКТ ПЕТЕРБУРГ - СЛОВО", result["natural_full_field_text_raw"])
        self.assertEqual(["САНКТ", "ПЕТЕРБУРГ - СЛОВО"], [line["text_raw"] for line in result["line_groups"]])
        self.assertEqual(-1.0, result["words"][0]["confidence_raw"])
        self.assertEqual(0.0, result["words"][2]["confidence_raw"])
        self.assertIsNone(result["words"][3]["confidence_raw"])

    def test_plain_text_stdout_is_not_mistaken_for_empty_tsv_and_stderr_stays_private(self) -> None:
        plain_text = b"recognized words, but not TSV\n"
        fake = runner.subprocess.CompletedProcess(["tesseract"], 0, plain_text, b"config warning\n")
        with patch.object(runner.subprocess, "run", return_value=fake) as run:
            result = runner._tesseract_read(np.zeros((4, 8, 3), dtype=np.uint8), single_line=False, tessdata=Path("/pinned/tessdata/rus.traineddata"))
        self.assertEqual("error", result["status"])
        self.assertEqual("tesseract_tsv_header_invalid", result["reason_code"])
        self.assertEqual(plain_text.decode(), result["stdout_raw_unparsed"])
        self.assertEqual("config warning\n", result["stderr_raw"])
        self.assertIn(["-c", "tessedit_create_tsv=1"], [run.call_args.args[0][i:i + 2] for i in range(len(run.call_args.args[0]) - 1)])

    def test_header_only_tsv_is_legitimate_empty_but_words_are_ok(self) -> None:
        header = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n"
        empty = runner._parse_tesseract_tsv(header)
        self.assertEqual("empty", empty["status"])
        self.assertEqual([], empty["words"])
        row = "5\t1\t1\t1\t1\t1\t2\t3\t20\t10\t-1\tПОЛ\n"
        parsed = runner._parse_tesseract_tsv(header + row)
        self.assertEqual("ok", parsed["status"])
        self.assertEqual("ПОЛ", parsed["natural_full_field_text_raw"])

    def test_rapid_natural_rows_only_groups_vertical_neighbors(self) -> None:
        outputs = [
            {"text_raw": "ПЕТЕРБУРГ", "box_source": [110, 10, 210, 30]},
            {"text_raw": "САНКТ", "box_source": [10, 11, 100, 31]},
            {"text_raw": "ОБЛАСТЬ", "box_source": [12, 50, 100, 70]},
        ]
        rows, full = runner._rapid_natural_rows(outputs, [300, 100])
        self.assertEqual(["САНКТ ПЕТЕРБУРГ", "ОБЛАСТЬ"], [row["text_raw"] for row in rows])
        self.assertEqual("САНКТ ПЕТЕРБУРГ ОБЛАСТЬ", full)
        self.assertEqual([10.0, 10.0, 210.0, 31.0], rows[0]["box_crop_xyxy"])


if __name__ == "__main__":
    unittest.main()
