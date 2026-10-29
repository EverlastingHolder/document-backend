"""Fast synthetic checks; they never import Paddle or run OCR."""
from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import run_vl_roi_control as control


class RoiVlControlTests(unittest.TestCase):
    def test_only_the_four_predeclared_cases_are_accepted(self):
        items = [
            {"sample_id": sample, "field_id": field}
            for sample, field in control.EXPECTED_CASES
        ]
        selected = control._select_cases(items)
        self.assertEqual(list(selected), list(control.EXPECTED_CASES))
        with self.assertRaises(control.ControlError):
            control._select_cases(items[:-1])
        with self.assertRaises(control.ControlError):
            control._select_cases(items + [items[0]])

    def test_model_tree_missing_fails_closed_and_fingerprint_detects_change(self):
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaisesRegex(control.ControlError, "local_model_directory_missing"):
                control._model_tree_snapshot(Path(temporary) / "absent")
            model = Path(temporary)
            (model / "inference.yml").write_text(
                f"Global:\n  model_name: {control.MODEL_NAME}\n", encoding="utf-8"
            )
            (model / "config.json").write_text("{}\n", encoding="utf-8")
            weights = model / "weights.safetensors"
            weights.write_bytes(b"synthetic-model-bytes")
            before = control._model_tree_snapshot(model)
            weights.write_bytes(b"changed-synthetic-model-bytes")
            after = control._model_tree_snapshot(model)
            self.assertNotEqual(before["tree_sha256"], after["tree_sha256"])
            self.assertNotEqual(before["files_sha256"], after["files_sha256"])

    def test_candidate_text_bbox_and_explicit_generation_cap_are_preserved(self):
        raw_bbox = [[1, 2], [3, 4], [5, 6], [7, 8]]
        result = {
            "res": {
                "parsing_res_list": [
                    {"block_content": "first line\nsecond line", "block_label": "ocr", "block_bbox": raw_bbox},
                    {"block_content": "third line", "block_label": "ocr"},
                ]
            }
        }
        blocks = control._candidate_blocks(result)
        self.assertEqual(blocks[0]["block_content"], "first line\nsecond line")
        self.assertEqual(blocks[0]["block_bbox"], raw_bbox)
        self.assertNotIn("block_bbox", blocks[1])
        self.assertEqual(control.GEOMETRY_SCOPE, "whole_crop_synthetic_prompt_box")
        self.assertIsNone(control._generated_length_cap_status({"block_content": "x" * 5000}))
        self.assertFalse(control._generated_length_cap_status({"generated_length": 511}))
        self.assertTrue(control._generated_length_cap_status({"res": {"generated_length": 512}}))

    def test_result_hash_pair_detects_postwrite_change(self):
        with tempfile.TemporaryDirectory() as temporary:
            case_dir = Path(temporary)
            vendor = case_dir / "vendor-result.json"
            case = case_dir / "case-result.json"
            vendor.write_text("{}\n", encoding="utf-8")
            case.write_text("{}\n", encoding="utf-8")
            status = {"result_sha256": {
                "vendor_result_sha256": control._sha_file(vendor),
                "case_result_sha256": control._sha_file(case),
            }}
            self.assertEqual(control._verify_result_hashes(case_dir, status), status["result_sha256"])
            case.write_text("changed\n", encoding="utf-8")
            with self.assertRaisesRegex(control.ControlError, "result_hash_mismatch"):
                control._verify_result_hashes(case_dir, status)


if __name__ == "__main__":
    unittest.main()
