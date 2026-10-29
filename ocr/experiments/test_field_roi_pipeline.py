from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import numpy as np
from PIL import Image

from ocr.experiments import field_roi_pipeline as roi
from ocr.experiments import run_field_roi_comparison as comparison


def _line(line_id: str, pass_id: str, text: str, x: int, y: int, width: int = 220, height: int = 32) -> dict:
    return {
        "id": line_id, "pass_id": pass_id, "text_raw": text, "score": 0.8,
        "quad": [[x, y], [x + width, y], [x + width, y + height], [x, y + height]],
    }


def _candidate(line_ids: list[str]) -> dict:
    return {"line_ids": line_ids, "pass_ids": [], "raw": "private", "value": "private"}


def fixture() -> tuple[dict, dict]:
    lines = [
        _line("n1", "original", "ALPHA", 500, 1120),
        _line("n2", "original", "BRAVO", 505, 1200),
        _line("n3", "original", "CHARLIE", 510, 1280),
        _line("dob", "original", "12.03.2000", 650, 1370, 300),
        _line("gender", "original", "F", 520, 1382, 80),
        _line("place1", "original", "CITY", 505, 1450),
        _line("place2", "original", "REGION", 505, 1500),
        _line("issue", "original", "01.02.2020", 300, 420, 260),
        _line("dept", "original", "123-456", 700, 425, 180),
        _line("issuer1", "original", "AUTHORITY", 300, 320, 400),
        _line("issuer2", "original", "OFFICE", 300, 365, 300),
        _line("vizmrz1", "original", "ABC123<<<<<<<<<<<<<<<<", 250, 1700, 700, 45),
        _line("mrz1", "mrz_rapid", "ABC123<<<<<<<<<<<<<<<<", 250, 1700, 700, 45),
    ]
    obs = {
        "schema_version": "passport-ocr-observations/v2",
        "image": {"id": "sample-01", "width": 1200, "height": 2000, "coordinate_frame": roi.COORDINATE_FRAME, "sha256": "a" * 64},
        "document": {"bounds": [100, 100, 1100, 1900], "method": "contour", "quality": "candidate", "seam_y": 1000},
        "passes": [
            {"id": "original", "kind": "original", "crop": [0, 0, 1200, 2000], "rotation_degrees": 0, "status": "ok"},
            {"id": "mrz_rapid", "kind": "mrz", "crop": [200, 1500, 1000, 1900], "rotation_degrees": 0, "status": "ok"},
        ],
        "lines": lines,
    }
    fields = {
        "surname": {"state": "uncertain", "candidates": [_candidate(["n1"])]},
        "given_name": {"state": "uncertain", "candidates": [_candidate(["n2"])]},
        "patronymic": {"state": "uncertain", "candidates": [_candidate(["n3"])]},
        "gender": {"state": "uncertain", "candidates": [_candidate(["gender"])]},
        "birth_date": {"state": "uncertain", "candidates": [_candidate(["dob"])]},
        "birth_place": {"state": "uncertain", "candidates": [_candidate(["place1", "place2"])]},
        "issue_date": {"state": "uncertain", "candidates": [_candidate(["issue"])]},
        "department_code": {"state": "uncertain", "candidates": [_candidate(["dept"])]},
        "issuer": {"state": "uncertain", "candidates": [_candidate(["issuer1", "issuer2"])]},
    }
    extraction = {"schema_version": "passport-extraction/v1", "image_id": "sample-01", "fields": fields}
    return obs, extraction


class FieldROIPlannerTests(unittest.TestCase):
    def test_supported_name_row_and_weak_blocks_are_proposed_without_mutation(self) -> None:
        obs, extraction = fixture()
        before_obs, before_extraction = copy.deepcopy(obs), copy.deepcopy(extraction)
        plan = roi.plan_field_rois(obs, extraction)
        self.assertEqual(obs, before_obs)
        self.assertEqual(extraction, before_extraction)
        self.assertEqual(["given_name", "gender", "birth_place", "issuer"], [item["field_id"] for item in plan["hypotheses"]])
        by_field = {item["field_id"]: item for item in plan["hypotheses"]}
        self.assertEqual("supported_row", by_field["given_name"]["role_quality"]["state"])
        self.assertEqual(["n2"], by_field["given_name"]["source_line_ids"])
        self.assertGreater(by_field["given_name"]["crop_bounds"][2], 730)
        for field_id in ("gender", "birth_place", "issuer"):
            self.assertEqual("weak", by_field[field_id]["role_quality"]["state"])
        self.assertTrue(all(item["coordinate_frame"] == roi.COORDINATE_FRAME for item in plan["hypotheses"]))
        self.assertFalse(any("mrz1" in item["source_line_ids"] for item in plan["hypotheses"]))

    def test_missing_gender_and_place_evidence_still_only_emits_weak_diagnostics(self) -> None:
        obs, extraction = fixture()
        for field_id in ("gender", "birth_place"):
            extraction["fields"][field_id] = {"state": "missing", "candidates": [], "evidence": {"line_ids": []}}
        plan = roi.plan_field_rois(obs, extraction)
        by_field = {item["field_id"]: item for item in plan["hypotheses"]}
        self.assertEqual("weak", by_field["gender"]["role_quality"]["state"])
        self.assertIn("gender_neighborhood_from_name_date_anchors", by_field["gender"]["role_quality"]["reason_codes"])
        self.assertEqual([], by_field["gender"]["source_line_ids"])
        self.assertLessEqual(by_field["gender"]["crop_bounds"][2], 650)
        self.assertEqual("given_name", by_field["gender"]["adjacent_anchors"][1]["role"])
        self.assertEqual("weak", by_field["birth_place"]["role_quality"]["state"])
        self.assertIn("field_evidence_missing_bounded_block_only", by_field["birth_place"]["role_quality"]["reason_codes"])

    def test_missing_gender_with_dob_but_no_supported_name_group_is_rejected(self) -> None:
        obs, extraction = fixture()
        extraction["fields"]["gender"] = {"state": "missing", "candidates": [], "evidence": {"line_ids": []}}
        extraction["fields"]["surname"]["candidates"] = []
        extraction["fields"]["given_name"]["candidates"] = []
        extraction["fields"]["patronymic"]["candidates"] = []
        plan = roi.plan_field_rois(obs, extraction)
        self.assertNotIn("gender", [item["field_id"] for item in plan["hypotheses"]])
        self.assertIn({"field_id": "gender", "reason_code": "validated_dob_neighborhood_unavailable"}, plan["rejections"])

    def test_supported_name_date_allows_only_left_band_gender_probe(self) -> None:
        obs, extraction = fixture()
        extraction["fields"]["gender"] = {"state": "missing", "candidates": [], "evidence": {"line_ids": []}}
        plan = roi.plan_field_rois(obs, extraction)
        hypotheses = {item["field_id"]: item for item in plan["hypotheses"]}
        gender = hypotheses["gender"]
        self.assertEqual("weak", gender["role_quality"]["state"])
        self.assertEqual([], gender["source_line_ids"])
        self.assertLessEqual(gender["crop_bounds"][2], 650)
        self.assertNotIn("field_evidence_missing_date_neighborhood_only", gender["role_quality"]["reason_codes"])
        self.assertEqual({"given_name", "gender", "birth_place", "issuer"}, set(hypotheses))
        self.assertFalse(any(item.get("autofill") for item in hypotheses.values()))

    def test_missing_gender_band_expands_left_when_supported_name_column_is_shifted(self) -> None:
        obs, extraction = fixture()
        extraction["fields"]["gender"] = {"state": "missing", "candidates": [], "evidence": {"line_ids": []}}
        for line in obs["lines"]:
            if line["id"] in {"n1", "n2", "n3"}:
                line["quad"] = [[x + 250, y] for x, y in line["quad"]]
        plan = roi.plan_field_rois(obs, extraction)
        gender = next(item for item in plan["hypotheses"] if item["field_id"] == "gender")
        self.assertEqual("weak", gender["role_quality"]["state"])
        self.assertEqual([], gender["source_line_ids"])
        self.assertLessEqual(gender["crop_bounds"][0], 300)
        self.assertLessEqual(gender["crop_bounds"][2], 650)

    def test_distant_explicit_gender_candidate_is_rejected_and_crop_stays_bounded(self) -> None:
        obs, extraction = fixture()
        for line in obs["lines"]:
            if line["id"] == "gender":
                line["quad"] = [[520, 1560], [600, 1560], [600, 1592], [520, 1592]]
        plan = roi.plan_field_rois(obs, extraction)
        self.assertNotIn("gender", [item["field_id"] for item in plan["hypotheses"]])
        self.assertIn({"field_id": "gender", "reason_code": "validated_dob_neighborhood_unavailable"}, plan["rejections"])

    def test_explicit_gender_roi_ends_before_observed_same_pass_mrz(self) -> None:
        obs, extraction = fixture()
        plan = roi.plan_field_rois(obs, extraction)
        gender = next(item for item in plan["hypotheses"] if item["field_id"] == "gender")
        self.assertLessEqual(gender["crop_bounds"][3], 1700)

    def test_second_dob_candidate_cannot_be_reused_as_gender_evidence(self) -> None:
        obs, extraction = fixture()
        obs["lines"].append(_line("dob_nearby", "original", "12.03.2000", 655, 1372, 300))
        extraction["fields"]["birth_date"]["candidates"].append(_candidate(["dob_nearby"]))
        extraction["fields"]["gender"] = {"state": "uncertain", "candidates": [_candidate(["dob_nearby"]) ]}
        plan = roi.plan_field_rois(obs, extraction)
        gender = next(item for item in plan["hypotheses"] if item["field_id"] == "gender")
        self.assertEqual("weak", gender["role_quality"]["state"])
        self.assertEqual([], gender["source_line_ids"])
        self.assertIn("gender_neighborhood_from_name_date_anchors", gender["role_quality"]["reason_codes"])

    def test_missing_date_or_mrz_boundary_fails_closed(self) -> None:
        obs, extraction = fixture()
        extraction["fields"]["birth_date"] = {"state": "missing", "candidates": []}
        plan = roi.plan_field_rois(obs, extraction)
        self.assertNotIn("birth_name", [item["field_id"] for item in plan["hypotheses"]])
        self.assertNotIn("gender", [item["field_id"] for item in plan["hypotheses"]])
        self.assertNotIn("birth_place", [item["field_id"] for item in plan["hypotheses"]])
        obs, extraction = fixture()
        obs["lines"] = [line for line in obs["lines"] if line["id"] not in {"mrz1", "vizmrz1"}]
        plan = roi.plan_field_rois(obs, extraction)
        self.assertNotIn("birth_place", [item["field_id"] for item in plan["hypotheses"]])
        self.assertIn({"field_id": "birth_place", "reason_code": "dob_or_observed_mrz_boundary_unavailable"}, plan["rejections"])

    def test_ambiguous_dob_in_same_pass_rejects_dependent_rois(self) -> None:
        obs, extraction = fixture()
        obs["lines"].append(_line("dob_conflict", "original", "13.04.2001", 650, 1460, 300))
        extraction["fields"]["birth_date"]["candidates"].append(_candidate(["dob_conflict"]))
        plan = roi.plan_field_rois(obs, extraction)
        fields = {item["field_id"] for item in plan["hypotheses"]}
        self.assertNotIn("given_name", fields)
        self.assertNotIn("gender", fields)
        self.assertNotIn("birth_place", fields)

    def test_observation_extraction_image_identity_must_match(self) -> None:
        obs, extraction = fixture()
        extraction["image_id"] = "different-image"
        plan = roi.plan_field_rois(obs, extraction)
        self.assertEqual([], plan["hypotheses"])
        self.assertEqual(4, len(plan["rejections"]))

    def test_given_name_row_requires_three_field_evidence_rows(self) -> None:
        obs, extraction = fixture()
        extraction["fields"]["surname"] = {"state": "missing", "candidates": []}
        plan = roi.plan_field_rois(obs, extraction)
        self.assertNotIn("given_name", [item["field_id"] for item in plan["hypotheses"]])
        self.assertIn({"field_id": "given_name", "reason_code": "three_aligned_name_rows_or_middle_row_unverified"}, plan["rejections"])

    def test_mrz_looking_original_line_is_never_source_evidence(self) -> None:
        obs, extraction = fixture()
        obs["lines"].append(_line("original_mrz", "original", "ABC123<<<<<<<<<<<<<<<<", 250, 1700, 700, 45))
        extraction["fields"]["issuer"]["candidates"].append(_candidate(["original_mrz"]))
        plan = roi.plan_field_rois(obs, extraction)
        for hypothesis in plan["hypotheses"]:
            self.assertNotIn("original_mrz", hypothesis["source_line_ids"])

    def test_coordinate_validation_and_crop_mapping(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            image_path = Path(temp) / "tiny.png"
            Image.fromarray(np.zeros((8, 8, 3), dtype=np.uint8)).save(image_path)
            with self.assertRaises(roi.FieldROIError):
                roi.read_image_crop(image_path, [-1, 0, 5, 5])
        box = roi.map_box_to_source([10, 20, 30, 60], [100, 200, 500, 800], (2.0, 2.0))
        self.assertEqual([105.0, 210.0, 115.0, 230.0], box)
        image = np.zeros((40, 100, 3), dtype=np.uint8)
        scaled, scales = roi.resize_crop(image, max_side=1600)
        self.assertEqual((80, 200, 3), scaled.shape)
        self.assertEqual((2.0, 2.0), scales)
        huge = np.zeros((1000, 2000, 3), dtype=np.uint8)
        scaled, scales = roi.resize_crop(huge, max_side=1600)
        self.assertEqual((800, 1600, 3), scaled.shape)
        self.assertAlmostEqual(0.8, scales[0])

    def test_tsv_parser_quotes_and_nonfinite_confidence(self) -> None:
        header = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext"
        rows = [
            "5\t1\t1\t1\t1\t1\t2\t3\t10\t8\t92.5\t\"TOKEN\"",
            "5\t1\t1\t1\t1\t2\t13\t3\t8\t8\tinf\tOTHER",
        ]
        result = roi.parse_tesseract_tsv(header + "\n" + "\n".join(rows))
        self.assertEqual('"TOKEN"', result[0]["text_raw"])
        self.assertEqual([2, 3, 12, 11], result[0]["box_crop"])
        self.assertEqual(92.5, result[0]["raw_confidence"])
        self.assertEqual((1, 1, 1, 1, 1), tuple(result[0][key] for key in ("page_num", "block_num", "par_num", "line_num", "word_num")))
        with self.assertRaises(roi.FieldROIError):
            roi.parse_tesseract_tsv(header + "\n5\tbad")

    def test_tesseract_timeout_is_private_and_returns_only_safe_code(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            binary = root / "tesseract"
            binary.write_text("stub", encoding="utf-8")
            tessdata = root / "tessdata"
            tessdata.mkdir()
            (tessdata / roi.TESSDATA_FILENAME).write_bytes(b"pinned-model-stub")
            image = np.zeros((20, 40, 3), dtype=np.uint8)
            with patch.object(roi, "sha256_file", return_value=roi.TESSDATA_SHA256), patch(
                "ocr.experiments.field_roi_pipeline.subprocess.run",
                side_effect=__import__("subprocess").TimeoutExpired(["tesseract"], 8, stderr=b"PRIVATE_OCR_PATH_AND_TEXT"),
            ):
                result = roi.run_tesseract_reader(binary, tessdata, image, mode="psm7")
            self.assertEqual("tesseract_timeout", result["reason_code"])
            self.assertNotIn("PRIVATE_OCR_PATH_AND_TEXT", result["reason_code"])
            self.assertIn("PRIVATE_OCR_PATH_AND_TEXT", result["private_stderr"])
            self.assertLessEqual(result["elapsed_seconds"], 1.0)

    def test_rapid_rec_only_uses_entire_crop_geometry_without_fabricated_word_box(self) -> None:
        class FakeEngine:
            def __call__(self, image, **kwargs):
                self.kwargs = kwargs
                self.image_shape = image.shape
                return SimpleNamespace(txts=["sample words"], boxes=None, scores=[0.7])

        engine = FakeEngine()
        crop = np.zeros((40, 100, 3), dtype=np.uint8)
        result = roi.rapidocr_reader(engine, crop, mode="rec_only")
        self.assertEqual({"use_det": False, "use_cls": False, "use_rec": True}, engine.kwargs)
        self.assertEqual("entire_crop", result["outputs"][0]["geometry_kind"])
        self.assertIsNone(result["outputs"][0]["box_source"])

    def test_fake_reader_runner_caps_two_calls_and_maps_crop_boxes_to_original(self) -> None:
        obs, extraction = fixture()
        hypothesis = roi.plan_field_rois(obs, extraction)["hypotheses"][0]
        with tempfile.TemporaryDirectory() as temp:
            image_path = Path(temp) / "image.png"
            Image.fromarray(np.zeros((2000, 1200, 3), dtype=np.uint8)).save(image_path)
            calls = []

            class FakeEngine:
                def __call__(self, image, **kwargs):
                    calls.append(kwargs)
                    return SimpleNamespace(txts=["token"], boxes=None, scores=[0.8])

            def fake_tesseract(*args, **kwargs):
                calls.append({"reader": "tesseract"})
                return {"status": "ok", "words": [{"text_raw": "word", "box_source": [1, 2, 5, 8]}]}

            with patch.object(roi, "run_tesseract_reader", side_effect=fake_tesseract):
                result = roi.run_hypothesis_readers(hypothesis, image_path, engine=FakeEngine(), tesseract="unused", tessdata_dir=temp)
            self.assertEqual(2, len(result["readers"]))
            rapid = result["readers"][0]
            self.assertEqual(hypothesis["crop_bounds"], rapid["outputs"][0]["box_source"])
            word = result["readers"][1]["words"][0]
            x0, y0, _, _ = hypothesis["crop_bounds"]
            self.assertEqual([x0 + 1, y0 + 2, x0 + 5, y0 + 8], word["box_source"])
            self.assertEqual(2, len(calls))

    def test_extreme_detector_crop_is_skipped_with_budget_code(self) -> None:
        obs, extraction = fixture()
        hypothesis = next(item for item in roi.plan_field_rois(obs, extraction)["hypotheses"] if item["field_id"] == "birth_place")
        with tempfile.TemporaryDirectory() as temp:
            image_path = Path(temp) / "image.png"
            Image.fromarray(np.zeros((2000, 1200, 3), dtype=np.uint8)).save(image_path)
            engine = SimpleNamespace()
            with patch("ocr.experiments.run_resolution_comparison.estimate_detector_pixels", return_value=roi.MAX_DETECTOR_PIXELS + 1), patch.object(
                roi, "run_tesseract_reader", return_value={"status": "skipped", "reason_code": "test", "words": []}
            ):
                result = roi.run_hypothesis_readers(hypothesis, image_path, engine=engine, tesseract="unused", tessdata_dir=temp)
            rapid = next(item for item in result["readers"] if item["reader"] == "rapidocr")
            self.assertEqual("detector_pixel_budget_exceeded", rapid["reason_code"])
            self.assertEqual("skipped", rapid["status"])


class FieldROIDiagnosticScoringTests(unittest.TestCase):
    def test_exact_and_contiguous_whole_token_metrics_do_not_match_prefixes(self) -> None:
        prefix = {"outputs": [{"text_raw": "ALPHABET"}]}
        exact = {"outputs": [{"text_raw": "alpha"}]}
        wrapped = {"outputs": [{"text_raw": "x alpha beta y"}]}
        self.assertNotEqual(comparison.normalize_candidate("ALPHABET"), comparison.normalize_candidate("ALPHA"))
        self.assertEqual({"candidate_group_count": 1, "exact_normalized_whole_candidate_group_match": False, "contiguous_whole_token_sequence_in_allowed_group": False}, comparison.score_reader_diagnostics(prefix, "alpha", "given_name"))
        self.assertTrue(comparison.score_reader_diagnostics(exact, "ALPHA", "given_name")["exact_normalized_whole_candidate_group_match"])
        self.assertTrue(comparison.score_reader_diagnostics(wrapped, "alpha beta", "birth_place")["contiguous_whole_token_sequence_in_allowed_group"])


if __name__ == "__main__":
    unittest.main()
