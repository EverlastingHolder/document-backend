"""Fast synthetic tests for geometry, transforms and the private observation API."""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import cv2
import numpy as np
from PIL import Image

import region_pipeline as pipeline


class RegionPipelineTests(unittest.TestCase):
    @staticmethod
    def _session(providers: list[str] | None) -> object:
        if providers is None:
            return SimpleNamespace()
        return SimpleNamespace(get_providers=lambda: providers)

    @classmethod
    def _provider_engine(cls, det: list[str] | None, rec: list[str] | None) -> SimpleNamespace:
        return SimpleNamespace(
            text_det=SimpleNamespace(session=SimpleNamespace(session=cls._session(det))),
            text_rec=SimpleNamespace(session=SimpleNamespace(session=cls._session(rec))),
        )

    @staticmethod
    def _distributed_lines(x_shift: int = 0, y_shift: int = 0) -> list[dict[str, object]]:
        lines = []
        for row, center_y in enumerate((200, 500, 800)):
            for col, center_x in enumerate((200, 580, 1000)):
                x = center_x - 20 + x_shift
                y = center_y - 6 + y_shift
                lines.append({
                    "quad": [[x, y], [x + 40, y], [x + 40, y + 12], [x, y + 12]],
                    "text_raw": f"synthetic-{row}-{col}",
                    "score": 0.9,
                })
        return lines

    def test_identity_and_crop_translation_map_all_quad_points(self) -> None:
        quad = [[0, 0], [10, 0], [10, 5], [0, 5]]
        self.assertEqual(pipeline.map_quad_to_source(quad, (0, 0, 20, 20)), [
            [0.0, 0.0], [10.0, 0.0], [10.0, 5.0], [0.0, 5.0]
        ])
        self.assertEqual(pipeline.map_quad_to_source(quad, (23, 41, 100, 90)), [
            [23.0, 41.0], [33.0, 41.0], [33.0, 46.0], [23.0, 46.0]
        ])

    def test_cv_rotation_pixel_center_mappings_include_minus_one(self) -> None:
        # A 7x5 crop rotated by OpenCV: coordinates refer to integer pixel centers.
        crop = (10, 20, 17, 25)
        rotated_cw_point = [4, 0]  # source crop pixel (0, 0)
        rotated_ccw_point = [0, 6]  # source crop pixel (0, 0)
        self.assertEqual(pipeline.map_quad_to_source([rotated_cw_point] * 4, crop, 90)[0], [10.0, 20.0])
        self.assertEqual(pipeline.map_quad_to_source([rotated_ccw_point] * 4, crop, 270)[0], [10.0, 20.0])

    def test_generic_outer_document_and_seam_split(self) -> None:
        image = np.full((800, 600, 3), 245, dtype=np.uint8)
        cv2.rectangle(image, (95, 75), (505, 725), (60, 60, 60), 4)
        # Interior page content and a strong structural seam, with no passport-specific coordinates.
        cv2.line(image, (100, 400), (500, 400), (70, 70, 70), 8)
        cv2.rectangle(image, (140, 130), (280, 260), (110, 110, 110), 2)
        bounds, score = pipeline.detect_document_bounds(image)
        self.assertIsNotNone(bounds)
        self.assertGreaterEqual(score, pipeline._MIN_CANDIDATE_SCORE)
        assert bounds is not None
        self.assertLess(bounds[0], 100)
        self.assertGreater(bounds[2], 500)
        seam_y = pipeline.detect_horizontal_seam(image, bounds)
        self.assertIsNotNone(seam_y)
        self.assertLess(abs(int(seam_y) - 400), 20)
        crops = pipeline.page_crops(bounds, seam_y, 600, 800)
        self.assertEqual(len(crops), 2)
        self.assertGreater(crops[0][3] - crops[1][1], 0)
        for x0, y0, x1, y1 in crops:
            self.assertGreaterEqual(x0, 0)
            self.assertGreaterEqual(y0, 0)
            self.assertLessEqual(x1, 600)
            self.assertLessEqual(y1, 800)

    def test_small_stamp_like_rectangle_is_not_document_candidate(self) -> None:
        image = np.full((800, 600, 3), 245, dtype=np.uint8)
        cv2.rectangle(image, (250, 300), (350, 400), (35, 35, 35), 5)
        bounds, _ = pipeline.detect_document_bounds(image)
        self.assertIsNone(bounds)

    def test_ocr_bbox_envelope_is_generic_translated_geometry(self) -> None:
        lines = self._distributed_lines()
        bounds = pipeline.detect_ocr_bbox_envelope(lines, 1200, 1000)
        shifted = pipeline.detect_ocr_bbox_envelope(self._distributed_lines(40, 30), 1200, 1000)
        self.assertIsNotNone(bounds)
        self.assertIsNotNone(shifted)
        assert bounds is not None and shifted is not None
        self.assertEqual(shifted, (bounds[0] + 40, bounds[1] + 30, bounds[2] + 40, bounds[3] + 30))

    def test_ocr_bbox_envelope_rejects_compact_face_or_stamp_cluster(self) -> None:
        lines = []
        for index in range(8):
            x = 430 + (index % 2) * 12
            y = 350 + (index // 2) * 10
            lines.append({"quad": [[x, y], [x + 8, y], [x + 8, y + 4], [x, y + 4]]})
        self.assertIsNone(pipeline.detect_ocr_bbox_envelope(lines, 1200, 1000))

    def test_margin_skip_requires_exact_vertical_right_margin_token(self) -> None:
        bounds = (100, 100, 1100, 900)
        vertical_quad = [[950, 300], [970, 300], [970, 500], [950, 500]]

        def line(text: str, quad: list[list[int]]) -> dict[str, object]:
            return {"text_raw": text, "quad": quad}

        self.assertTrue(pipeline._has_vertical_right_margin_serial_candidate(
            [line("1234 567-890", vertical_quad)], bounds
        ))
        self.assertFalse(pipeline._has_vertical_right_margin_serial_candidate(
            [line("12345678901", vertical_quad)], bounds
        ))
        self.assertFalse(pipeline._has_vertical_right_margin_serial_candidate(
            [line("123456", [[950, 400], [1050, 400], [1050, 420], [950, 420]])], bounds
        ))
        self.assertFalse(pipeline._has_vertical_right_margin_serial_candidate(
            [line("1234567890", [[200, 780], [900, 780], [900, 820], [200, 820]])], bounds
        ))

        # A tall, right-edge, near-vertical line can still be an MRZ-like
        # bottom row; the explicit bottom-wide exclusion must take precedence.
        bottom_wide_quad = [[80, 2300], [83, 2310], [288, 2874], [285, 2864]]
        self.assertFalse(pipeline._has_vertical_right_margin_serial_candidate(
            [line("1234567890", bottom_wide_quad)], (0, 0, 300, 3000)
        ))

    def test_fake_engine_runs_margin_passes_only_without_valid_serial(self) -> None:
        class FakeEngine:
            def __init__(self, text: str, quad: list[list[int]]) -> None:
                provider = RegionPipelineTests._provider_engine(
                    ["CPUExecutionProvider"], ["CPUExecutionProvider"]
                )
                self.text_det = provider.text_det
                self.text_rec = provider.text_rec
                self.text = text
                self.quad = quad
                self.calls = 0

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                self.calls += 1
                if self.calls != 1:
                    return SimpleNamespace(txts=[], boxes=[], scores=[])
                return SimpleNamespace(txts=[self.text], boxes=[self.quad], scores=[0.9])

        fixtures = (
            ("1234567890<<<<<<<<<<<<", [[200, 780], [900, 780], [900, 820], [200, 820]], 5),
            ("1234567890", [[950, 300], [970, 300], [970, 500], [950, 500]], 3),
        )
        image = np.full((1000, 1200, 3), 255, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "synthetic-margin.png"
            Image.fromarray(image).save(path)
            for text, quad, expected_pass_count in fixtures:
                engine = FakeEngine(text, quad)
                with (
                    patch.object(pipeline, "detect_document_bounds", return_value=((100, 100, 1100, 900), 0.9)),
                    patch.object(pipeline, "detect_horizontal_seam", return_value=500),
                ):
                    result = pipeline.build_observations(
                        path, "synthetic-margin", "missing-det", "missing-rec", engine=engine
                    )
                kinds = [entry["kind"] for entry in result["passes"]]
                self.assertEqual(len(kinds), expected_pass_count)
                self.assertEqual(engine.calls, expected_pass_count)
                self.assertLessEqual(len(kinds), 5)
                if expected_pass_count == 5:
                    self.assertEqual(kinds[-2:], ["right_margin_cw", "right_margin_ccw"])
                else:
                    self.assertNotIn("right_margin_cw", kinds)
                    self.assertNotIn("right_margin_ccw", kinds)

    def test_ocr_envelope_fallback_retains_original_lines_and_marks_method(self) -> None:
        class DistributedFakeEngine:
            def __init__(self) -> None:
                provider = RegionPipelineTests._provider_engine(
                    ["CPUExecutionProvider"], ["CPUExecutionProvider"]
                )
                self.text_det = provider.text_det
                self.text_rec = provider.text_rec
                self.calls = 0

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                self.calls += 1
                lines = RegionPipelineTests._distributed_lines() if self.calls == 1 else []
                return SimpleNamespace(
                    txts=[line["text_raw"] for line in lines],
                    boxes=[line["quad"] for line in lines],
                    scores=[line["score"] for line in lines],
                )

        image = np.full((1000, 1200, 3), 255, dtype=np.uint8)
        engine = DistributedFakeEngine()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plain-background.png"
            Image.fromarray(image).save(path)
            result = pipeline.build_observations(
                path, "spread-fixture", "missing-det", "missing-rec", engine=engine
            )

        self.assertEqual(result["document"]["method"], "ocr_bbox_envelope")
        self.assertEqual(result["document"]["quality"], "candidate")
        self.assertEqual(result["passes"][0]["kind"], "original")
        self.assertEqual(len([line for line in result["lines"] if line["pass_id"] == "original"]), 9)
        self.assertLessEqual(len(result["passes"]), 5)
        schema = json.loads(Path(__file__).parents[1].joinpath("schema/observations-v1.schema.json").read_text())
        self.assertIn("ocr_bbox_envelope", schema["properties"]["document"]["properties"]["method"]["enum"])

    def test_insufficient_original_lines_keep_full_fallback(self) -> None:
        class SparseFakeEngine:
            def __init__(self) -> None:
                provider = RegionPipelineTests._provider_engine(
                    ["CPUExecutionProvider"], ["CPUExecutionProvider"]
                )
                self.text_det = provider.text_det
                self.text_rec = provider.text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                lines = RegionPipelineTests._distributed_lines()[:7]
                return SimpleNamespace(
                    txts=[line["text_raw"] for line in lines],
                    boxes=[line["quad"] for line in lines],
                    scores=[line["score"] for line in lines],
                )

        image = np.full((1000, 1200, 3), 255, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "sparse-background.png"
            Image.fromarray(image).save(path)
            result = pipeline.build_observations(
                path, "sparse-fixture", "missing-det", "missing-rec", engine=SparseFakeEngine()
            )
        self.assertEqual(result["document"]["method"], "full_fallback")
        self.assertEqual(result["document"]["quality"], "fallback")
        self.assertEqual([entry["kind"] for entry in result["passes"]], ["original"])
        self.assertEqual(len(result["lines"]), 7)

    def test_padding_clamps_to_image_without_invalid_extent(self) -> None:
        self.assertEqual(
            pipeline._pad_and_clamp((-2, 4, 30, 18), 25, 20, fraction=0.2, minimum=8),
            (0, 0, 25, 20),
        )

    def test_engine_creation_is_cached_and_cpu_only_flags_are_explicit(self) -> None:
        pipeline._create_cpu_engine_cached.cache_clear()
        with patch.object(pipeline, "RapidOCR", return_value=object()) as constructor:
            first = pipeline.create_cpu_engine("det.onnx", "rec.onnx")
            second = pipeline.create_cpu_engine("det.onnx", "rec.onnx")
        self.assertIs(first, second)
        constructor.assert_called_once()
        params = constructor.call_args.kwargs["params"]
        self.assertEqual(params["Det.engine_type"], pipeline.EngineType.ONNXRUNTIME)
        self.assertEqual(params["Rec.engine_type"], pipeline.EngineType.ONNXRUNTIME)
        self.assertFalse(params["Global.use_cls"])
        for accelerator in ("cuda", "dml", "cann", "coreml"):
            self.assertFalse(params[f"EngineConfig.onnxruntime.use_{accelerator}"])
        self.assertNotIn("Det.limit_side_len", params)

    def test_detector_limit_override_is_validated_forwarded_and_cached_separately(self) -> None:
        pipeline._create_cpu_engine_cached.cache_clear()
        with patch.object(pipeline, "RapidOCR", side_effect=[object(), object()]) as constructor:
            default = pipeline.create_cpu_engine("det.onnx", "rec.onnx")
            larger = pipeline.create_cpu_engine("det.onnx", "rec.onnx", det_limit_side_len=1280)
            larger_again = pipeline.create_cpu_engine("det.onnx", "rec.onnx", det_limit_side_len=1280)
        self.assertIsNot(default, larger)
        self.assertIs(larger, larger_again)
        self.assertEqual(constructor.call_count, 2)
        self.assertNotIn("Det.limit_side_len", constructor.call_args_list[0].kwargs["params"])
        params = constructor.call_args_list[1].kwargs["params"]
        self.assertEqual(params["Det.limit_side_len"], 1280)
        self.assertEqual(params["Det.model_path"], str(Path("det.onnx").resolve()))
        self.assertFalse(params["EngineConfig.onnxruntime.use_cuda"])

    def test_bad_detector_limit_override_fails_before_engine_creation(self) -> None:
        for value in (True, 255, 1921, 1280.0, "1280"):
            with self.subTest(value=value), patch.object(pipeline, "RapidOCR") as constructor:
                with self.assertRaises(ValueError):
                    pipeline.create_cpu_engine("det.onnx", "rec.onnx", det_limit_side_len=value)  # type: ignore[arg-type]
                constructor.assert_not_called()

    def test_detector_config_report_keeps_unknown_fake_engine_settings_unknown(self) -> None:
        self.assertEqual(
            pipeline._engine_detector_config(object()),
            {
                "limit_side_len": None,
                "limit_type": None,
                "global_min_side_len": None,
                "global_max_side_len": None,
                "global_use_preprocess_img": None,
                "global_use_vertical_padding": None,
                "global_min_height": None,
                "global_width_height_ratio": None,
            },
        )

    def test_explicit_override_rejects_reused_engine_with_unknown_or_different_config(self) -> None:
        image = np.full((32, 32, 3), 255, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config-mismatch.png"
            Image.fromarray(image).save(path)
            mismatched_engines = (
                SimpleNamespace(),
                SimpleNamespace(cfg=SimpleNamespace(Det=SimpleNamespace(limit_side_len=1280, limit_type="max"))),
            )
            for engine in mismatched_engines:
                with self.subTest(engine=engine), self.assertRaisesRegex(ValueError, "provided_engine_detector_config_mismatch"):
                    pipeline.build_observations(
                        path, "config-mismatch", "missing-det", "missing-rec",
                        engine=engine, det_limit_side_len=1280,
                    )

    def test_auxiliary_engine_is_used_only_for_margin_and_mrz_passes(self) -> None:
        class FakeEngine:
            def __init__(self, limit: int) -> None:
                provider = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"])
                self.text_det = provider.text_det
                self.text_rec = provider.text_rec
                self.cfg = SimpleNamespace(
                    Det=SimpleNamespace(limit_side_len=limit, limit_type="min"),
                    Global=SimpleNamespace(
                        min_side_len=30, max_side_len=2000, use_preprocess_img=True,
                        use_vertical_padding=True, min_height=30, width_height_ratio=8,
                    ),
                )
                self.calls = 0

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                self.calls += 1
                lines = RegionPipelineTests._distributed_lines() if self.calls == 1 else []
                return SimpleNamespace(
                    txts=[line["text_raw"] for line in lines],
                    boxes=[line["quad"] for line in lines],
                    scores=[line["score"] for line in lines],
                )

        primary, auxiliary = FakeEngine(1280), FakeEngine(736)
        image = np.full((1000, 1200, 3), 255, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "aux-routing.png"
            Image.fromarray(image).save(path)
            with (
                patch.object(pipeline, "detect_document_bounds", return_value=((100, 100, 1100, 900), 0.9)),
                patch.object(pipeline, "detect_horizontal_seam", return_value=500),
                patch.object(pipeline, "find_tesseract", return_value=None),
            ):
                result = pipeline.build_observations(
                    path, "aux-routing", "missing-det", "missing-rec", engine=primary,
                    auxiliary_engine=auxiliary, enhanced=True, include_mrz=True, det_limit_side_len=1280,
                )

        self.assertEqual(primary.calls, 3)  # original and the two page crops
        self.assertEqual(auxiliary.calls, 3)  # two margins and RapidOCR MRZ
        self.assertEqual(result["runtime"]["detector_profiles"]["primary"]["limit_side_len"], 1280)
        self.assertEqual(result["runtime"]["detector_profiles"]["auxiliary"]["limit_side_len"], 736)
        self.assertEqual(result["runtime"]["pass_detector_profiles"], {
            "original": "primary", "upper_page": "primary", "lower_page": "primary",
            "right_margin_cw": "auxiliary", "right_margin_ccw": "auxiliary", "mrz_rapid": "auxiliary",
        })
        self.assertEqual(result["runtime"]["auxiliary_providers"]["sessions"]["det"], ["CPUExecutionProvider"])

    def test_build_observations_keeps_source_coordinates_and_pass_ids(self) -> None:
        class FakeEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                h, w = image.shape[:2]
                x = min(5, max(0, w - 2))
                y = min(5, max(0, h - 2))
                return SimpleNamespace(
                    txts=["synthetic"],
                    boxes=[[[x, y], [x + 1, y], [x + 1, y + 1], [x, y + 1]]],
                    scores=[0.9],
                )

        image = np.full((800, 600, 3), 245, dtype=np.uint8)
        cv2.rectangle(image, (95, 75), (505, 725), (60, 60, 60), 4)
        cv2.line(image, (100, 400), (500, 400), (70, 70, 70), 8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "synthetic.png"
            Image.fromarray(image).save(path)
            result = pipeline.build_observations(path, "fixture", "missing-det", "missing-rec", engine=FakeEngine())

        self.assertEqual(result["schema_version"], "passport-ocr-observations/v1")
        self.assertEqual(result["image"]["coordinate_frame"], "exif_normalized_pixels")
        self.assertEqual(result["passes"][0]["kind"], "original")
        self.assertEqual(result["runtime"]["providers"]["sessions"]["det"], ["CPUExecutionProvider"])
        self.assertEqual(result["runtime"]["providers"]["sessions"]["rec"], ["CPUExecutionProvider"])
        ids = [entry["id"] for entry in result["passes"]]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertLessEqual(len(result["passes"]), 5)
        self.assertTrue(all(len(line["quad"]) == 4 for line in result["lines"]))
        for line in result["lines"]:
            self.assertIn(line["pass_id"], ids)
            for x, y in line["quad"]:
                self.assertGreaterEqual(x, 0)
                self.assertGreaterEqual(y, 0)
                self.assertLessEqual(x, 600)
                self.assertLessEqual(y, 800)

    def test_default_build_contract_remains_v1_and_mrz_is_opt_in(self) -> None:
        class FakeEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                return SimpleNamespace(txts=[], boxes=[], scores=[])

        image = np.full((60, 80, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "v1-default.png"
            Image.fromarray(image).save(path)
            result = pipeline.build_observations(
                path, "fixture", "missing-det", "missing-rec", engine=FakeEngine(), enhanced=False
            )
        self.assertEqual(result["schema_version"], "passport-ocr-observations/v1")
        self.assertEqual([entry["kind"] for entry in result["passes"]], ["original"])
        self.assertNotIn("candidates", result)

    def test_include_mrz_adds_bounded_v2_passes_without_candidate_root(self) -> None:
        class FakeEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __init__(self) -> None:
                self.calls = 0

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                self.calls += 1
                if self.calls == 1:
                    return SimpleNamespace(
                        txts=["PNRUS<<<<<<<<<<<<<<<<<<<<", "1234567890<<<<<<<<<<<<<<"],
                        boxes=[
                            [[100, 800], [900, 800], [900, 818], [100, 818]],
                            [[100, 832], [900, 832], [900, 850], [100, 850]],
                        ],
                        scores=[0.8, 0.8],
                    )
                return SimpleNamespace(txts=["raw-fragment"], boxes=[[[3, 4], [13, 4], [13, 10], [3, 10]]], scores=[0.7])

        engine = FakeEngine()
        image = np.full((1000, 1200, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "v2-mrz.png"
            Image.fromarray(image).save(path)
            with patch.object(pipeline, "find_tesseract", return_value=None):
                result = pipeline.build_observations(
                    path, "fixture", "missing-det", "missing-rec", engine=engine,
                    enhanced=False, include_mrz=True,
                )

        self.assertEqual(result["schema_version"], "passport-ocr-observations/v2")
        self.assertNotIn("candidates", result)
        mrz_passes = [entry for entry in result["passes"] if entry["kind"] == "mrz"]
        self.assertEqual([entry["id"] for entry in mrz_passes], ["mrz_rapid", "mrz_tesseract"])
        self.assertEqual(mrz_passes[0]["reader"], "rapidocr")
        self.assertEqual(mrz_passes[1]["reader"], "tesseract-eng")
        self.assertEqual(mrz_passes[1]["issue_code"], "tesseract_unavailable")
        schema = json.loads(Path(__file__).parents[1].joinpath("schema/observations-v2.schema.json").read_text())
        pass_properties = schema["properties"]["passes"]["items"]["properties"]
        self.assertIn("mrz", pass_properties["kind"]["enum"])
        self.assertEqual(pass_properties["reader"]["enum"], ["rapidocr", "tesseract-eng"])
        self.assertEqual(pass_properties["issue_code"]["type"], "string")
        mrz_line = next(line for line in result["lines"] if line["pass_id"] == "mrz_rapid")
        self.assertEqual(mrz_line["text_raw"], "raw-fragment")
        crop_x0, crop_y0, _, _ = mrz_passes[0]["crop"]
        self.assertEqual(mrz_line["quad"][0], [float(crop_x0 + 3), float(crop_y0 + 4)])
        self.assertEqual(engine.calls, 2)

    def test_include_mrz_keeps_pass_budget_at_seven_with_seam_and_margins(self) -> None:
        class EmptyFakeEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                return SimpleNamespace(txts=[], boxes=[], scores=[])

        image = np.full((400, 300, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "pass-budget.png"
            Image.fromarray(image).save(path)
            with (
                patch.object(pipeline, "find_tesseract", return_value=None),
                patch.object(pipeline, "detect_document_bounds", return_value=((10, 10, 290, 390), 0.9)),
                patch.object(pipeline, "detect_horizontal_seam", return_value=200),
            ):
                result = pipeline.build_observations(
                    path, "fixture", "missing-det", "missing-rec", engine=EmptyFakeEngine(),
                    enhanced=True, include_mrz=True,
                )
        self.assertEqual(len(result["passes"]), 7)
        self.assertLessEqual(len(result["passes"]), 7)

    def test_mrz_reader_failure_is_recorded_without_failing_observations(self) -> None:
        class FailingMrzEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __init__(self) -> None:
                self.calls = 0

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                self.calls += 1
                if self.calls == 2:
                    raise RuntimeError("private OCR failure detail")
                return SimpleNamespace(txts=[], boxes=[], scores=[])

        image = np.full((200, 300, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "mrz-error.png"
            Image.fromarray(image).save(path)
            with patch.object(pipeline, "find_tesseract", return_value=None):
                result = pipeline.build_observations(
                    path, "fixture", "missing-det", "missing-rec", engine=FailingMrzEngine(),
                    enhanced=False, include_mrz=True,
                )
        passes = {item["id"]: item for item in result["passes"]}
        self.assertEqual(result["schema_version"], "passport-ocr-observations/v2")
        self.assertEqual(passes["mrz_rapid"]["status"], "error")
        self.assertEqual(passes["mrz_rapid"]["issue_code"], "rapidocr_error")
        self.assertEqual(passes["mrz_tesseract"]["issue_code"], "tesseract_unavailable")
        self.assertNotIn("private OCR failure detail", json.dumps(result))

    def test_tesseract_crop_rows_map_from_crop_to_original_coordinates(self) -> None:
        class EmptyFakeEngine:
            text_det = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_det
            text_rec = RegionPipelineTests._provider_engine(["CPUExecutionProvider"], ["CPUExecutionProvider"]).text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                return SimpleNamespace(txts=[], boxes=[], scores=[])

        image = np.full((100, 200, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tess-transform.png"
            Image.fromarray(image).save(path)
            with (
                patch.object(pipeline, "find_tesseract", return_value="/fake/tesseract"),
                patch.object(pipeline, "run_tesseract_crop", return_value=(
                    [{"text_raw": "PNRUS<", "score": 0.87,
                      "quad": [[12, 20], [56, 20], [56, 36], [12, 36]]}], None, 0.01
                )),
            ):
                result = pipeline.build_observations(
                    path, "fixture", "missing-det", "missing-rec", engine=EmptyFakeEngine(),
                    enhanced=False, include_mrz=True,
                )
        line = next(item for item in result["lines"] if item["pass_id"] == "mrz_tesseract")
        crop_x0, crop_y0, _, _ = next(item for item in result["passes"] if item["id"] == "mrz_tesseract")["crop"]
        self.assertEqual(line["text_raw"], "PNRUS<")
        self.assertEqual(line["quad"][0], [float(crop_x0 + 12), float(crop_y0 + 20)])

    def test_non_cpu_provider_is_rejected_after_lazy_session_exists(self) -> None:
        engine = self._provider_engine(["CUDAExecutionProvider", "CPUExecutionProvider"], ["CPUExecutionProvider"])
        self.assertRaises(pipeline.CPUProviderValidationError, pipeline._assert_cpu_only, engine)

    def test_missing_actual_provider_is_rejected(self) -> None:
        engine = self._provider_engine(["CPUExecutionProvider"], None)
        self.assertRaises(pipeline.CPUProviderValidationError, pipeline._assert_cpu_only, engine)

    def test_nonfinite_ocr_values_are_filtered_and_observations_are_strict_json(self) -> None:
        class NonfiniteFakeEngine:
            # This is only a synthetic callable; no RapidOCR model is loaded.
            def __init__(self) -> None:
                provider_engine = RegionPipelineTests._provider_engine(
                    ["CPUExecutionProvider"], ["CPUExecutionProvider"]
                )
                self.text_det = provider_engine.text_det
                self.text_rec = provider_engine.text_rec

            def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
                valid = [[1, 1], [8, 1], [8, 4], [1, 4]]
                invalid_quad = [[1, 1], [float("nan"), 1], [8, 4], [1, 4]]
                return SimpleNamespace(
                    txts=["valid", "nan-score", "inf-score", "nan-coordinate"],
                    boxes=[valid, valid, valid, invalid_quad],
                    scores=[0.9, float("nan"), float("inf"), 0.5],
                )

        image = np.full((30, 40, 3), 245, dtype=np.uint8)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "finite-only.png"
            Image.fromarray(image).save(path)
            result = pipeline.build_observations(
                path, "finite-fixture", "missing-det", "missing-rec", engine=NonfiniteFakeEngine(), enhanced=False
            )

        self.assertEqual([line["text_raw"] for line in result["lines"]], ["valid"])
        self.assertEqual(result["lines"][0]["score"], 0.9)
        json.dumps(result, allow_nan=False)


if __name__ == "__main__":
    unittest.main()
