from __future__ import annotations

import copy
import hashlib
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import numpy as np
from PIL import Image, ImageOps

from ocr.experiments import region_pipeline, source_tile_pipeline as tiles


class _SyntheticResult:
    def __init__(self) -> None:
        self.txts = ["synthetic"]
        self.scores = [0.5]
        self.boxes = [np.asarray([[10, 20], [30, 20], [30, 40], [10, 40]], dtype=np.float32)]


class _SyntheticEngine:
    def __call__(self, image: np.ndarray, **kwargs):
        assert kwargs == {"use_det": True, "use_cls": False, "use_rec": True}
        return _SyntheticResult()


class SourceTilePlanTests(unittest.TestCase):
    def _assert_axis_coverage(self, tiles: dict, axis: int, extent: int) -> None:
        boxes = [tile["crop"] for tile in tiles["tiles"]]
        intervals = sorted((box[axis], box[axis + 2]) for box in boxes)
        self.assertEqual(intervals[0][0], 0)
        self.assertEqual(intervals[-1][1], extent)
        for previous, current in zip(intervals, intervals[1:]):
            self.assertLessEqual(current[0], previous[1])
            if len(intervals) > 1:
                overlap = previous[1] - current[0]
                self.assertGreaterEqual(overlap, 0.18 * min(previous[1] - previous[0], current[1] - current[0]))

    def test_expected_grids_cover_source_with_overlap_and_bound(self) -> None:
        sample_02 = tiles.plan_source_tiles(1920, 2560)
        self.assertEqual(sample_02["grid"], {"columns": 1, "rows": 2})
        self.assertEqual([item["crop"] for item in sample_02["tiles"]], [
            [0, 0, 1920, 1440], [0, 1120, 1920, 2560],
        ])
        self._assert_axis_coverage(sample_02, 1, 2560)

        sample_04 = tiles.plan_source_tiles(3024, 4032)
        self.assertEqual(sample_04["grid"], {"columns": 2, "rows": 3})
        self.assertEqual(len(sample_04["tiles"]), 6)
        self.assertEqual(
            [tile["crop"][:2] for tile in sample_04["tiles"]],
            [[0, 0], [1328, 0], [0, 1232], [1328, 1232], [0, 2464], [1328, 2464]],
        )
        for tile in sample_04["tiles"]:
            x0, y0, x1, y1 = tile["crop"]
            self.assertGreaterEqual(x0, 0)
            self.assertGreaterEqual(y0, 0)
            self.assertLessEqual(x1, 3024)
            self.assertLessEqual(y1, 4032)
            self.assertLessEqual(max(x1 - x0, y1 - y0), tiles.MAX_TILE_SIDE)
        self._assert_axis_coverage(sample_04, 0, 3024)
        self._assert_axis_coverage(sample_04, 1, 4032)

        overlap_aware = tiles.plan_source_tiles(3900, 3000)
        self.assertEqual(overlap_aware["grid"], {"columns": 3, "rows": 2})
        self.assertLessEqual(len(overlap_aware["tiles"]), 6)
        self._assert_axis_coverage(overlap_aware, 0, 3900)
        self._assert_axis_coverage(overlap_aware, 1, 3000)

    def test_grid_rejects_more_than_six_tiles(self) -> None:
        with self.assertRaisesRegex(tiles.SourceTileError, "tile_count_limit_exceeded"):
            tiles.plan_source_tiles(9000, 9000)


class SourceTileAugmentationTests(unittest.TestCase):
    def _source_and_baseline(self, root: Path, size=(1920, 2560), *, orientation=None):
        width, height = size
        image_path = root / "synthetic.jpg" if orientation else root / "synthetic.png"
        raw_size = (height, width) if orientation in (6, 8) else size
        synthetic_image = Image.new("RGB", raw_size, color=(127, 127, 127))
        if orientation:
            exif = Image.Exif()
            exif[274] = orientation
            synthetic_image.save(image_path, format="JPEG", quality=95, exif=exif)
        else:
            synthetic_image.save(image_path)
        raw = image_path.read_bytes()
        with Image.open(image_path) as opened:
            normalized_size = ImageOps.exif_transpose(opened).size
        detector, recognizer = root / "detector.onnx", root / "recognizer.onnx"
        detector.write_bytes(b"synthetic detector model")
        recognizer.write_bytes(b"synthetic recognizer model")
        baseline = {
            "schema_version": "passport-ocr-observations/v2",
            "image": {
                "id": "synthetic",
                "width": normalized_size[0],
                "height": normalized_size[1],
                "sha256": hashlib.sha256(raw).hexdigest(),
                "coordinate_frame": "exif_normalized_pixels",
            },
            "document": {
                "bounds": [0, 0, width, height], "method": "full_fallback",
                "quality": "fallback", "seam_y": None,
            },
            "passes": [{
                "id": "original", "kind": "original", "crop": [0, 0, width, height],
                "rotation_degrees": 0, "transform_to_image": [[1, 0, 0], [0, 1, 0], [0, 0, 1]],
                "elapsed_seconds": 0.1, "status": "ok",
            }],
            "lines": [],
            "runtime": {
                "model_sha256": {
                    "detector": tiles.sha256_file(detector),
                    "recognizer": tiles.sha256_file(recognizer),
                },
                "detector_profiles": {"primary": {
                    "limit_side_len": 736, "limit_type": "min", "global_min_side_len": 30,
                    "global_max_side_len": 2000, "global_use_preprocess_img": True,
                    "global_use_vertical_padding": True,
                }},
                "pass_detector_profiles": {"original": "primary"},
            },
        }
        return image_path, baseline, detector, recognizer

    def test_augments_copy_as_v3_and_maps_source_quads(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image_path, baseline, detector, recognizer = self._source_and_baseline(root, orientation=6)
            original = copy.deepcopy(baseline)
            engine = _SyntheticEngine()
            profile = baseline["runtime"]["detector_profiles"]["primary"]
            with (
                patch.object(region_pipeline, "create_cpu_engine", return_value=engine),
                patch.object(region_pipeline, "_engine_detector_config", return_value=profile),
                patch.object(region_pipeline, "cpu_provider_report", return_value={
                    "sessions": {"det": [tiles.CPU_PROVIDER], "rec": [tiles.CPU_PROVIDER]},
                    "available": ["ignored-provider-list"],
                }),
            ):
                # The lazy import in augment_source_tiles resolves this module
                # attribute after patching.
                augmented, plan = tiles.augment_source_tiles(
                    image_path, baseline, detector, recognizer, private_output_dir=root / "private",
                )
            self.assertEqual(baseline, original)
            self.assertEqual(augmented["schema_version"], tiles.SCHEMA_VERSION)
            self.assertEqual(len(augmented["passes"]) - len(original["passes"]), 2)
            self.assertEqual(len(augmented["lines"]), 2)
            first = augmented["lines"][0]
            self.assertEqual(first["quad"][0], [10.0, 20.0])
            self.assertEqual(augmented["image"]["width"], 1920)
            self.assertEqual(augmented["image"]["height"], 2560)
            second_pass = augmented["passes"][-1]
            self.assertEqual(second_pass["kind"], "source_tile")
            self.assertEqual(second_pass["rotation_degrees"], 0)
            self.assertEqual(second_pass["transform_to_image"][0][2], 0)
            self.assertEqual(second_pass["transform_to_image"][1][2], 1120)
            self.assertEqual(augmented["lines"][-1]["quad"][0], [10.0, 1140.0])
            self.assertTrue(plan["profile"]["passes"][0]["cpu_verified"])
            self.assertNotIn("available", plan["profile"]["passes"][0]["providers"])

    def test_cpu_mismatch_emits_error_pass_without_lines(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image_path, baseline, detector, recognizer = self._source_and_baseline(root)
            profile = baseline["runtime"]["detector_profiles"]["primary"]
            with (
                patch.object(region_pipeline, "create_cpu_engine", return_value=_SyntheticEngine()),
                patch.object(region_pipeline, "_engine_detector_config", return_value=profile),
                patch.object(region_pipeline, "cpu_provider_report", return_value={
                    "sessions": {"det": ["OtherProvider"], "rec": [tiles.CPU_PROVIDER]},
                    "available": [tiles.CPU_PROVIDER],
                }),
            ):
                augmented, plan = tiles.augment_source_tiles(
                    image_path, baseline, detector, recognizer, private_output_dir=root / "private",
                )
            self.assertEqual(augmented["lines"], [])
            self.assertTrue(all(p["status"] == "error" for p in augmented["passes"][1:]))
            self.assertTrue(all(p["line_count"] == 0 for p in plan["profile"]["passes"]))

    def test_already_resize_bounded_source_is_identity_noop(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image_path, baseline, detector, recognizer = self._source_and_baseline(root, (1280, 1600))
            with patch.object(region_pipeline, "create_cpu_engine", side_effect=AssertionError("engine must not load")):
                output, plan = tiles.augment_source_tiles(
                    image_path, baseline, detector, recognizer, private_output_dir=root / "private",
                )
            self.assertEqual(output, baseline)
            self.assertEqual(plan["status"], "no_op")
            self.assertEqual(output["schema_version"], "passport-ocr-observations/v2")

    def test_identity_guards_reject_wrong_hash_and_registered_frame(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image_path, baseline, detector, recognizer = self._source_and_baseline(root)
            baseline["image"]["sha256"] = "0" * 64
            with self.assertRaisesRegex(tiles.SourceTileError, "source_image_sha256_mismatch"):
                tiles.augment_source_tiles(
                    image_path, baseline, detector, recognizer, private_output_dir=root / "private-a",
                )
            _, baseline, detector, recognizer = self._source_and_baseline(root)
            baseline["runtime"]["registered_spread"] = {"coordinate_space_role": "derived_registered_spread"}
            with self.assertRaisesRegex(tiles.SourceTileError, "derived_registered_frame_not_supported"):
                tiles.augment_source_tiles(
                    image_path, baseline, detector, recognizer, private_output_dir=root / "private-b",
                )

    def test_model_hash_mismatch_is_rejected_before_engine_creation(self) -> None:
        for model_name, error_code in (
            ("detector", "detector_model_sha256_mismatch"),
            ("recognizer", "recognizer_model_sha256_mismatch"),
        ):
            with self.subTest(model=model_name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                image_path, baseline, detector, recognizer = self._source_and_baseline(root)
                model_path = detector if model_name == "detector" else recognizer
                model_path.write_bytes(b"different model")
                with patch.object(region_pipeline, "create_cpu_engine", side_effect=AssertionError("engine must not load")):
                    with self.assertRaisesRegex(tiles.SourceTileError, error_code):
                        tiles.augment_source_tiles(
                            image_path, baseline, detector, recognizer, private_output_dir=root / "private",
                        )


if __name__ == "__main__":
    unittest.main()
