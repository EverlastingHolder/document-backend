"""Fast synthetic tests for the automatic VL crop reader; no Paddle import."""
from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from PIL import Image

import auto_vl_pipeline as auto


class FakePrediction:
    def __init__(self, value):
        self.value = value

    def save_to_json(self, save_path):
        Path(save_path).write_text(json.dumps(self.value), encoding="utf-8")


class DictVendorPrediction(dict):
    """Model the Paddle result object: dict-like but with a native serializer."""

    def __init__(self):
        super().__init__({"not_stdlib_json": object()})
        self.serializer_called = False

    def save_to_json(self, save_path):
        self.serializer_called = True
        Path(save_path).write_text(json.dumps(good_result([{"block_content": "synthetic"}])), encoding="utf-8")


class FakePipeline:
    def __init__(self, results):
        self.results = iter(results)
        self.calls = []

    def predict(self, path, **kwargs):
        self.calls.append((Path(path), kwargs))
        return [FakePrediction(next(self.results))]


def model_fixture(root: Path) -> Path:
    model = root / "model"
    model.mkdir()
    (model / "inference.yml").write_text(f"Global:\n  model_name: {auto.MODEL_ID}\n", encoding="utf-8")
    (model / "config.json").write_text("{}\n", encoding="utf-8")
    (model / "weights.safetensors").write_bytes(b"synthetic-fixed-model")
    return model


def make_plan(image_path: Path, width=4, height=3, regions=None):
    regions = regions if regions is not None else [{
        "id": "vl-given_name", "field_id": "given_name", "recipe": "baseline_candidate_rows/v1",
        "crop": [1, 0, 3, 2], "anchor_line_ids": ["line-1"],
    }]
    return {
        "schema_version": auto.SCHEMA_PLAN,
        "image": {
            "id": "sample-01:registered-spread", "sha256": hashlib.sha256(image_path.read_bytes()).hexdigest(),
            "width": width, "height": height, "coordinate_frame": "exif_normalized_pixels",
        },
        "baseline_sha256": "a" * 64,
        "plan_sha256": "b" * 64,
        "regions": regions,
        "skipped_fields": [
            {"field_id": field, "reason": "no_safe_crop"}
            for field in sorted(auto.ALLOWED_FIELDS - {region["field_id"] for region in regions})
        ],
    }


def good_result(blocks=None, length=None):
    result = {
        "res": {
            "model_settings": {"use_layout_detection": False, "use_doc_preprocessor": False},
            "parsing_res_list": blocks or [],
        }
    }
    if length is not None:
        result["res"]["generated_length"] = length
    return result


class AutoVLReaderTests(unittest.TestCase):
    def test_vendor_native_serializer_precedes_dict_mapping_fallback(self):
        with tempfile.TemporaryDirectory() as temporary:
            result_path = Path(temporary) / "vendor-result.json"
            prediction = DictVendorPrediction()
            value = auto._vendor_result(prediction, result_path)
            self.assertTrue(prediction.serializer_called)
            self.assertEqual(value["res"]["parsing_res_list"][0]["block_content"], "synthetic")
            self.assertEqual(json.loads(result_path.read_text()), value)

    def test_exif_normalized_crop_exact_pixels_and_all_blocks_are_preserved(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "oriented.jpg"
            # PNG carries EXIF orientation too; lossless pixels make crop assertion exact.
            pixels = Image.new("RGB", (3, 2))
            pixels.putdata([(255, 0, 0), (0, 255, 0), (0, 0, 255),
                            (255, 255, 0), (0, 255, 255), (255, 0, 255)])
            exif = Image.Exif()
            exif[274] = 6
            pixels.save(image_path, format="PNG", exif=exif)
            model = model_fixture(root)
            plan = make_plan(image_path, width=2, height=3, regions=[{
                "id": "vl-given_name", "field_id": "given_name", "recipe": "registered_template_crop/v1",
                "crop": [0, 1, 2, 3], "anchor_line_ids": ["line:1"],
            }])
            before_image = image_path.read_bytes()
            before_plan = json.loads(json.dumps(plan))
            all_blocks = [
                {"block_content": "line one\nline two"},
                {"block_content": "  raw third line  "},
            ]
            pipeline = FakePipeline([good_result(all_blocks)])
            evidence = auto.read_auto_vl_fields(
                image_path, plan, private_output_dir=root / "private", model_dir=model, pipeline=pipeline,
            )
            self.assertEqual(image_path.read_bytes(), before_image)
            self.assertEqual(plan, before_plan)
            self.assertEqual(evidence["regions"][0]["status"], "ok")
            self.assertEqual(evidence["regions"][0]["blocks"], [
                {"text_raw": "line one\nline two"}, {"text_raw": "  raw third line  "},
            ])
            self.assertEqual(evidence["regions"][0]["geometry_scope"], auto.GEOMETRY_SCOPE)
            self.assertEqual(evidence["regions"][0]["region"], plan["regions"][0])
            self.assertEqual(len(pipeline.calls), 1)
            crop = Image.open(root / "private/vl-given_name/crop.png")
            # EXIF orientation 6 rotates stored pixels clockwise. The crop
            # covers the final two normalized rows, in row-major order.
            self.assertEqual(list(crop.getdata()), [
                (0, 255, 255), (0, 255, 0), (255, 0, 255), (0, 0, 255),
            ])
            self.assertEqual(pipeline.calls[0][1], {
                "use_layout_detection": False, "prompt_label": "ocr", "max_new_tokens": 512,
            })

    def test_plan_identity_mismatch_fails_before_model_or_reader(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            model = model_fixture(root)
            plan = make_plan(image_path)
            plan["image"]["sha256"] = "0" * 64
            pipeline = FakePipeline([])
            with self.assertRaisesRegex(auto.AutoVLFailure, "source_image_sha_mismatch"):
                auto.read_auto_vl_fields(image_path, plan, private_output_dir=root / "private", model_dir=model, pipeline=pipeline)
            self.assertEqual(pipeline.calls, [])

    def test_empty_plan_does_not_initialize_or_call_pipeline_and_emits_metadata(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            model = model_fixture(root)
            plan = make_plan(image_path, regions=[])
            plan["skipped_fields"] = [
                {"field_id": field, "reason": "no_safe_crop"} for field in sorted(auto.ALLOWED_FIELDS)
            ]
            evidence = auto.read_auto_vl_fields(image_path, plan, private_output_dir=root / "private", model_dir=model)
            self.assertEqual(evidence["regions"], [])
            self.assertEqual(evidence["reader"]["model_tree_sha256"], auto.model_tree_snapshot(model)["tree_sha256"])
            self.assertEqual(json.loads((root / "private/evidence.json").read_text()), evidence)

    def test_cap_is_unknown_unless_vendor_exposes_length_and_reached_cap_is_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            model = model_fixture(root)
            plan = make_plan(image_path)
            pipeline = FakePipeline([good_result([{"block_content": "raw partial"}], 512)])
            evidence = auto.read_auto_vl_fields(image_path, plan, private_output_dir=root / "private", model_dir=model, pipeline=pipeline)
            region = evidence["regions"][0]
            self.assertEqual(region["status"], "error")
            self.assertEqual(region["blocks"], [])
            self.assertIs(region["cap_reached"], True)
            self.assertEqual(json.loads((root / "private/vl-given_name/failure.json").read_text())["error_code"], "generation_cap_reached")

    def test_unknown_field_duplicate_region_and_crop_outside_image_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            image_path = Path(temporary) / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            base = make_plan(image_path)
            variants = []
            unknown = json.loads(json.dumps(base)); unknown["regions"][0]["field_id"] = "passport_number"
            variants.append(unknown)
            duplicate = json.loads(json.dumps(base)); duplicate["regions"].append(dict(duplicate["regions"][0]))
            variants.append(duplicate)
            outside = json.loads(json.dumps(base)); outside["regions"][0]["crop"] = [0, 0, 5, 2]
            variants.append(outside)
            too_many = json.loads(json.dumps(base)); too_many["regions"] *= 7
            variants.append(too_many)
            for plan in variants:
                with self.subTest(plan=plan):
                    with self.assertRaises(auto.AutoVLFailure):
                        auto.validate_plan(plan)

    def test_layout_and_preprocessor_setting_guard_records_region_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            model = model_fixture(root)
            plan = make_plan(image_path)
            bad = good_result([{"block_content": "must not leak"}])
            bad["res"]["model_settings"]["use_layout_detection"] = True
            pipeline = FakePipeline([bad])
            evidence = auto.read_auto_vl_fields(image_path, plan, private_output_dir=root / "private", model_dir=model, pipeline=pipeline)
            self.assertEqual(evidence["regions"][0]["status"], "error")
            self.assertEqual(evidence["regions"][0]["blocks"], [])
            self.assertEqual(json.loads((root / "private/vl-given_name/failure.json").read_text())["error_code"], "runtime_layout_detection_not_disabled")

    def test_empty_vendor_blocks_do_not_become_candidate_blocks(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path = root / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            model = model_fixture(root)
            plan = make_plan(image_path)
            empty_private = root / "empty-private"
            empty = auto.read_auto_vl_fields(
                image_path, plan, private_output_dir=empty_private, model_dir=model,
                pipeline=FakePipeline([good_result([{"block_content": "  \n"}])]),
            )
            self.assertEqual(empty["regions"][0]["status"], "empty")
            self.assertEqual(empty["regions"][0]["blocks"], [])
            stored = json.loads((empty_private / "vl-given_name/vendor-result.json").read_text())
            self.assertEqual(stored["res"]["parsing_res_list"][0]["block_content"], "  \n")

            mixed_private = root / "mixed-private"
            mixed = auto.read_auto_vl_fields(
                image_path, plan, private_output_dir=mixed_private, model_dir=model,
                pipeline=FakePipeline([good_result([
                    {"block_content": ""}, {"block_content": "  raw text  "}, {"block_content": "\n"},
                ])]),
            )
            self.assertEqual(mixed["regions"][0]["status"], "ok")
            self.assertEqual(mixed["regions"][0]["blocks"], [{"text_raw": "  raw text  "}])

    def test_colon_ids_are_allowed_but_paths_are_not(self):
        with tempfile.TemporaryDirectory() as temporary:
            image_path = Path(temporary) / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            plan = make_plan(image_path)
            self.assertEqual(auto.validate_plan(plan)["image"]["id"], "sample-01:registered-spread")
            plan["image"]["id"] = "../unsafe"
            with self.assertRaisesRegex(auto.AutoVLFailure, "plan_image_id_invalid"):
                auto.validate_plan(plan)

    def test_plan_must_cover_every_allowed_field_once_and_anchors_cannot_be_empty(self):
        with tempfile.TemporaryDirectory() as temporary:
            image_path = Path(temporary) / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            plan = make_plan(image_path)
            plan["skipped_fields"].pop()
            with self.assertRaisesRegex(auto.AutoVLFailure, "plan_field_coverage_invalid"):
                auto.validate_plan(plan)
            plan = make_plan(image_path)
            plan["regions"][0]["anchor_line_ids"] = []
            with self.assertRaisesRegex(auto.AutoVLFailure, "plan_anchors_invalid"):
                auto.validate_plan(plan)

    def test_coherent_name_rows_recipe_is_accepted_with_source_anchors(self):
        with tempfile.TemporaryDirectory() as temporary:
            image_path = Path(temporary) / "image.png"
            Image.new("RGB", (4, 3), "white").save(image_path)
            plan = make_plan(image_path, regions=[{
                "id": "vl-patronymic", "field_id": "patronymic",
                "recipe": "coherent_name_rows/v1", "crop": [0, 0, 4, 2],
                "anchor_line_ids": ["name-row:01", "name-row:02", "name-row:03"],
            }])
            self.assertEqual(auto.validate_plan(plan)["regions"][0]["recipe"], "coherent_name_rows/v1")


if __name__ == "__main__":
    unittest.main()
