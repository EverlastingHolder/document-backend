from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np
from PIL import Image

from ocr.experiments import run_field_roi_comparison as comparison
from ocr.experiments.test_field_roi_pipeline import fixture


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _write_private_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    path.write_text(json.dumps(payload), encoding="utf-8")
    path.chmod(0o600)


class FieldROIRunnerTests(unittest.TestCase):
    def _setup_case(self, root: Path):
        baseline = root / "baseline"
        models = root / "models"
        tessdata = root / "tessdata"
        work = root / "private-work"
        output = root / "public-output"
        for path in (baseline, models, tessdata):
            path.mkdir(parents=True)
        model_paths = {
            "det": models / comparison.roi.MODEL_FILENAMES["detector"],
            "rec": models / comparison.roi.MODEL_FILENAMES["recognizer"],
            "rus": tessdata / comparison.roi.TESSDATA_FILENAME,
        }
        for name, path in model_paths.items():
            path.write_bytes((f"model-{name}").encode())
        pinned_hashes = {name: _sha(path) for name, path in model_paths.items()}
        image_paths = []
        for sample_id in ("sample-01", "sample-02"):
            image_path = root / f"{sample_id}.jpg"
            image_path.write_bytes(f"synthetic-image-{sample_id}".encode())
            image_paths.append(image_path)
            observations, extraction = fixture()
            observations["image"]["id"] = sample_id
            observations["image"]["sha256"] = _sha(image_path)
            observations["runtime"] = {
                "model_sha256": {"detector": pinned_hashes["det"], "recognizer": pinned_hashes["rec"]},
                "detector_profiles": {"primary": {"limit_side_len": 736, "limit_type": "min"}},
            }
            extraction["image_id"] = sample_id
            _write_private_json(baseline / "native_config" / sample_id / "observations.json", observations)
            _write_private_json(baseline / "native_config" / sample_id / "extraction.json", extraction)
        tesseract = root / "tesseract"
        tesseract.write_text("stub", encoding="utf-8")
        gold = root / "gold.json"
        gold_images = {}
        for sample_id in ("sample-01", "sample-02"):
            gold_images[sample_id] = {
                "source_image_id": sample_id,
                "fields": {field_id: {"literal": f"EXPECTED_{field_id.upper()}"} for field_id in comparison.roi.FIELD_IDS},
            }
        gold.write_text(json.dumps({"images": gold_images}), encoding="utf-8")
        return baseline, models, tessdata, work, output, image_paths, tesseract, gold, pinned_hashes

    def test_run_comparison_maps_images_and_keeps_reader_text_private(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            baseline, models, tessdata, work, output, images, tesseract, gold, pins = self._setup_case(root)
            # Intentionally pass reversed images; map by SHA, then report stable anonymous sample IDs.
            events = []

            def fake_reader(hypothesis, image_path, **kwargs):
                events.append("reader")
                return {
                    "hypothesis_id": hypothesis["id"], "field_id": hypothesis["field_id"],
                    "readers": [
                        {"reader": "rapidocr", "status": "ok", "outputs": [{"text_raw": "SECRET_TOKEN_ALPHA", "raw_confidence": 0.8}]},
                        {"reader": "tesseract_rus", "status": "ok", "words": [{"text_raw": "SECRET_TOKEN_ALPHA", "raw_confidence": 92.0}]},
                    ],
                }

            original_read_text = Path.read_text

            def tracked_read_text(path, *args, **kwargs):
                if path == gold:
                    events.append("gold")
                return original_read_text(path, *args, **kwargs)

            with patch.object(comparison.roi, "DETECTOR_SHA256", pins["det"]), patch.object(comparison.roi, "RECOGNIZER_SHA256", pins["rec"]), patch.object(comparison.roi, "TESSDATA_SHA256", pins["rus"]), patch.object(Path, "read_text", tracked_read_text):
                summary = comparison.run_comparison(
                    list(reversed(images)), baseline_root=baseline, baseline_profile="native_config",
                    model_dir=models, tessdata_dir=tessdata, tesseract=tesseract,
                    work_root=work, output_dir=output, gold_path=gold, reader_runner=fake_reader, test_adapter=True,
                )
            self.assertEqual(["sample-01", "sample-02"], [row["sample_id"] for row in summary["samples"]])
            self.assertEqual(16, summary["reader_invocations"])
            self.assertEqual({field: 2 for field in comparison.roi.FIELD_IDS}, summary["candidate_coverage_by_field"])
            self.assertEqual("gold", events[-1])
            self.assertEqual(8, events.count("reader"))
            public = (output / "field-roi-comparison.json").read_text(encoding="utf-8")
            markdown = (output / "field-roi-comparison.md").read_text(encoding="utf-8")
            self.assertNotIn("SECRET_TOKEN_ALPHA", public)
            self.assertNotIn("SECRET_TOKEN_ALPHA", markdown)
            private = (work / "sample-01" / "reader-results.json").read_text(encoding="utf-8")
            self.assertIn("SECRET_TOKEN_ALPHA", private)
            self.assertEqual(0o600, (work / "sample-01" / "reader-results.json").stat().st_mode & 0o777)
            self.assertEqual(0o700, (work / "sample-01").stat().st_mode & 0o777)
            self.assertFalse(summary["models_voted_or_confidence_pooled"])
            self.assertEqual("test_adapter_unverified", summary["rapidocr_provider_verification"])
            self.assertEqual("test_adapter_unverified", summary["reader_execution_mode"])
            self.assertIn("Go given_name visual-Latin-to-Cyrillic proposal is not applied", summary["posthoc_normalization_note"])

    def test_explicit_unmarked_engine_is_rejected_before_default_reader_runner(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            baseline, models, tessdata, work, output, images, tesseract, _, pins = self._setup_case(root)
            calls = []
            with patch.object(comparison.roi, "DETECTOR_SHA256", pins["det"]), patch.object(comparison.roi, "RECOGNIZER_SHA256", pins["rec"]), patch.object(comparison.roi, "TESSDATA_SHA256", pins["rus"]):
                with self.assertRaises(comparison.ComparisonError) as error:
                    comparison.run_comparison(
                        images, baseline_root=baseline, baseline_profile="native_config", model_dir=models,
                        tessdata_dir=tessdata, tesseract=tesseract, work_root=work, output_dir=output,
                        engine=type("SpoofedEngine", (), {"_field_roi_cpu_provider_state": "verified_cpu_detector_and_recognizer"})(),
                    )
            self.assertEqual("explicit_rapidocr_engine_provider_guard_missing", error.exception.code)
            self.assertEqual([], calls)

    def test_baseline_mapping_and_profile_fail_fast_before_reader_calls(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            baseline, models, tessdata, work, output, images, tesseract, _, pins = self._setup_case(root)
            called = []
            with self.assertRaises(comparison.ComparisonError) as error:
                comparison.run_comparison(
                    images, baseline_root=baseline, baseline_profile="larger_primary_detector_min_side_1280",
                    model_dir=models, tessdata_dir=tessdata, tesseract=tesseract,
                    work_root=work, output_dir=output, reader_runner=lambda *args, **kwargs: called.append(True),
                )
            self.assertEqual("only_native_736_baseline_is_supported", error.exception.code)
            self.assertEqual([], called)
            # Exact local weight pins are checked before any reader work.
            (models / comparison.roi.MODEL_FILENAMES["detector"]).write_bytes(b"wrong-weight")
            with patch.object(comparison.roi, "DETECTOR_SHA256", pins["det"]):
                with self.assertRaises(comparison.ComparisonError) as mismatch:
                    comparison._validate_pins(models, tessdata)
            self.assertEqual("local_model_pin_mismatch", mismatch.exception.code)

    def test_saved_1280_profile_is_rejected_even_when_named_native(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            baseline, _, _, _, _, images, _, _, pins = self._setup_case(root)
            path = baseline / "native_config" / "sample-01" / "observations.json"
            observation = json.loads(path.read_text(encoding="utf-8"))
            observation["runtime"]["detector_profiles"]["primary"]["limit_side_len"] = 1280
            _write_private_json(path, observation)
            with patch.object(comparison.roi, "DETECTOR_SHA256", pins["det"]), patch.object(comparison.roi, "RECOGNIZER_SHA256", pins["rec"]):
                with self.assertRaises(comparison.ComparisonError) as error:
                    comparison._map_inputs_to_baseline(images, baseline, "native_config")
            self.assertEqual("baseline_not_native_736_min", error.exception.code)

    def test_scoring_uses_literal_nfc_and_whole_token_sequence_only(self) -> None:
        self.assertEqual(
            {"candidate_group_count": 1, "exact_normalized_whole_candidate_group_match": False, "contiguous_whole_token_sequence_in_allowed_group": False},
            comparison.score_reader_diagnostics({"outputs": [{"text_raw": "EXPECTEDNAMESUFFIX"}]}, "EXPECTEDNAME", "given_name"),
        )
        self.assertTrue(comparison.score_reader_diagnostics({"outputs": [{"text_raw": "prefix EXPECTEDNAME suffix"}]}, "EXPECTEDNAME", "given_name")["contiguous_whole_token_sequence_in_allowed_group"])
        self.assertTrue(comparison.score_reader_diagnostics({"outputs": [{"text_raw": " ÉXAMPLE  ONE "}]}, "éxample one", "given_name")["exact_normalized_whole_candidate_group_match"])

    def test_multiline_place_only_matches_inside_one_connected_paragraph_block(self) -> None:
        def word(text, *, block, line, left, top):
            return {
                "text_raw": text, "page_num": 1, "block_num": block, "par_num": 1,
                "line_num": line, "word_num": 1, "box_source": [left, top, left + 100, top + 20],
            }

        wrapped = {"reader": "tesseract_rus", "words": [word("ALPHA", block=1, line=1, left=100, top=100), word("BETA", block=1, line=2, left=102, top=125)]}
        wrapped_score = comparison.score_reader_diagnostics(wrapped, "ALPHA BETA", "birth_place")
        self.assertEqual(1, wrapped_score["candidate_group_count"])
        self.assertTrue(wrapped_score["contiguous_whole_token_sequence_in_allowed_group"])

        separated_blocks = {"reader": "tesseract_rus", "words": [word("ALPHA", block=1, line=1, left=100, top=100), word("BETA", block=2, line=1, left=102, top=125)]}
        block_score = comparison.score_reader_diagnostics(separated_blocks, "ALPHA BETA", "birth_place")
        self.assertEqual(2, block_score["candidate_group_count"])
        self.assertFalse(block_score["contiguous_whole_token_sequence_in_allowed_group"])

        columns = {"reader": "tesseract_rus", "words": [word("ALPHA", block=1, line=1, left=100, top=100), word("BETA", block=1, line=2, left=700, top=125)]}
        column_score = comparison.score_reader_diagnostics(columns, "ALPHA BETA", "issuer")
        self.assertEqual(2, column_score["candidate_group_count"])
        self.assertFalse(column_score["contiguous_whole_token_sequence_in_allowed_group"])

        names = {"reader": "tesseract_rus", "words": [word("ALPHA", block=1, line=1, left=100, top=100), word("BETA", block=1, line=2, left=102, top=125)]}
        name_score = comparison.score_reader_diagnostics(names, "ALPHA BETA", "given_name")
        self.assertEqual(2, name_score["candidate_group_count"])
        self.assertFalse(name_score["contiguous_whole_token_sequence_in_allowed_group"])

        missing_word_number = {"reader": "tesseract_rus", "words": [
            {**word("ALPHA", block=1, line=1, left=100, top=100), "word_num": 1},
            {**word("BETA", block=1, line=1, left=400, top=100), "word_num": 3},
        ]}
        gap_score = comparison.score_reader_diagnostics(missing_word_number, "ALPHA BETA", "given_name")
        self.assertEqual(2, gap_score["candidate_group_count"])
        self.assertFalse(gap_score["contiguous_whole_token_sequence_in_allowed_group"])

        same_tsv_line_far_gap = {"reader": "tesseract_rus", "words": [
            word("ALPHA", block=1, line=1, left=100, top=100),
            {**word("BETA", block=1, line=1, left=700, top=100), "word_num": 2},
        ]}
        far_gap_groups = comparison.build_candidate_groups(same_tsv_line_far_gap, "birth_place")
        self.assertEqual(2, len(far_gap_groups))
        self.assertFalse(comparison.score_reader_diagnostics(same_tsv_line_far_gap, "ALPHA BETA", "birth_place")["contiguous_whole_token_sequence_in_allowed_group"])

    def test_rapid_block_join_requires_aligned_adjacent_rows_and_names_stay_single_line(self) -> None:
        aligned = {"reader": "rapidocr", "outputs": [
            {"text_raw": "ALPHA", "box_source": [100, 100, 200, 120], "geometry_kind": "detector_word_or_line_box"},
            {"text_raw": "BETA", "box_source": [102, 125, 202, 145], "geometry_kind": "detector_word_or_line_box"},
        ]}
        self.assertTrue(comparison.score_reader_diagnostics(aligned, "ALPHA BETA", "birth_place")["contiguous_whole_token_sequence_in_allowed_group"])
        self.assertFalse(comparison.score_reader_diagnostics(aligned, "ALPHA BETA", "given_name")["contiguous_whole_token_sequence_in_allowed_group"])
        split = {"reader": "rapidocr", "outputs": [
            {"text_raw": "ALPHA", "box_source": [100, 100, 200, 120], "geometry_kind": "detector_word_or_line_box"},
            {"text_raw": "BETA", "box_source": [700, 125, 800, 145], "geometry_kind": "detector_word_or_line_box"},
        ]}
        self.assertFalse(comparison.score_reader_diagnostics(split, "ALPHA BETA", "issuer")["contiguous_whole_token_sequence_in_allowed_group"])

        two_columns_same_baseline = {"reader": "rapidocr", "outputs": [
            {"text_raw": "ALPHA", "box_source": [100, 100, 200, 120], "geometry_kind": "detector_word_or_line_box"},
            {"text_raw": "BETA", "box_source": [700, 100, 800, 120], "geometry_kind": "detector_word_or_line_box"},
        ]}
        column_rows = comparison.build_candidate_groups(two_columns_same_baseline, "birth_place")
        self.assertEqual(2, len(column_rows))
        self.assertFalse(comparison.score_reader_diagnostics(two_columns_same_baseline, "ALPHA BETA", "birth_place")["contiguous_whole_token_sequence_in_allowed_group"])

    def test_rapid_factory_is_given_only_pinned_paths_and_leaves_lazy_provider_check_pending(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            model_dir = Path(temp)
            detector = model_dir / comparison.roi.MODEL_FILENAMES["detector"]
            recognizer = model_dir / comparison.roi.MODEL_FILENAMES["recognizer"]
            detector.write_bytes(b"detector")
            recognizer.write_bytes(b"recognizer")
            hashes = {detector.name: "d" * 64, recognizer.name: "r" * 64}
            fake_engine = type("FakeEngine", (), {
                "cfg": type("Config", (), {"Det": {"limit_side_len": 736, "limit_type": "min"}, "Global": {}})(),
            })()
            with patch.object(comparison.roi, "DETECTOR_SHA256", hashes[detector.name]), patch.object(comparison.roi, "RECOGNIZER_SHA256", hashes[recognizer.name]), patch.object(
                comparison.roi, "sha256_file", side_effect=lambda path: hashes[Path(path).name]
            ), patch("ocr.experiments.region_pipeline.create_cpu_engine", return_value=fake_engine) as factory:
                created = comparison.roi.create_pinned_cpu_engine(model_dir)
            self.assertIs(created, fake_engine)
            factory.assert_called_once_with(detector, recognizer)
            self.assertEqual("pending_lazy_first_call", fake_engine._field_roi_cpu_provider_state)

    def test_rapid_factory_rejects_already_created_non_cpu_sessions_without_inference(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            model_dir = Path(temp)
            detector = model_dir / comparison.roi.MODEL_FILENAMES["detector"]
            recognizer = model_dir / comparison.roi.MODEL_FILENAMES["recognizer"]
            detector.write_bytes(b"detector")
            recognizer.write_bytes(b"recognizer")
            hashes = {detector.name: "d" * 64, recognizer.name: "r" * 64}
            engine = type("CachedEngine", (), {"cfg": type("Config", (), {"Det": {"limit_side_len": 736, "limit_type": "min"}, "Global": {}})()})()
            with patch.object(comparison.roi, "DETECTOR_SHA256", hashes[detector.name]), patch.object(comparison.roi, "RECOGNIZER_SHA256", hashes[recognizer.name]), patch.object(
                comparison.roi, "sha256_file", side_effect=lambda path: hashes[Path(path).name]
            ), patch("ocr.experiments.region_pipeline.create_cpu_engine", return_value=engine), patch(
                "ocr.experiments.region_pipeline.cpu_provider_report", return_value={"sessions": {"det": ["CUDAExecutionProvider"], "rec": ["CPUExecutionProvider"]}, "available": ["CUDAExecutionProvider"]}
            ):
                with self.assertRaises(comparison.roi.FieldROIError) as error:
                    comparison.roi.create_pinned_cpu_engine(model_dir)
            self.assertEqual("rapidocr_cpu_provider_validation_failed", error.exception.code)
            self.assertEqual("failed", engine._field_roi_cpu_provider_state)

    def test_lazy_rec_only_verifies_used_recognizer_and_later_detector_block_checks_both(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            image_path = Path(temp) / "input.png"
            Image.fromarray(np.zeros((80, 100, 3), dtype=np.uint8)).save(image_path)
            cfg = type("Config", (), {"Det": {"limit_side_len": 736, "limit_type": "min"}, "Global": {}})()

            class LazyEngine:
                def __init__(self):
                    self.cfg = cfg
                    self._field_roi_cpu_provider_state = "pending_lazy_first_call"
                    self.text_det = None
                    self.text_rec = None

            engine = LazyEngine()
            crop = [10, 10, 90, 40]
            hypothesis = {"id": "gender-roi", "field_id": "gender", "crop_bounds": crop, "reader_plan": [{"reader": "rapidocr", "mode": "rec_only"}, {"reader": "tesseract_rus", "mode": "psm7"}]}

            def fake_rapid(current_engine, _crop, *, mode):
                self.assertEqual("rec_only", mode)
                current_engine.text_rec = type("Stage", (), {"session": type("Wrapper", (), {"session": type("Session", (), {"get_providers": lambda self: ["CPUExecutionProvider"]})()})()})()
                return {"status": "ok", "outputs": []}

            with patch.object(comparison.roi, "rapidocr_reader", side_effect=fake_rapid), patch(
                "ocr.experiments.field_roi_pipeline.run_tesseract_reader", return_value={"status": "empty", "words": []}
            ), patch("ocr.experiments.region_pipeline.cpu_provider_report", side_effect=lambda _engine: {"sessions": {"det": None, "rec": ["CPUExecutionProvider"]}, "available": ["GPUExecutionProvider"]}):
                result = comparison.roi.run_hypothesis_readers(hypothesis, image_path, engine=engine, tesseract="unused", tessdata_dir=temp)
            self.assertEqual("verified_cpu_rec_only_detector_not_used", engine._field_roi_cpu_provider_state)
            self.assertEqual(2, len(result["readers"]))

            engine.text_det = type("Stage", (), {"session": type("Wrapper", (), {"session": type("Session", (), {"get_providers": lambda self: ["CPUExecutionProvider"]})()})()})()
            block_hypothesis = {"id": "place-roi", "field_id": "birth_place", "crop_bounds": crop, "reader_plan": [{"reader": "rapidocr", "mode": "det_rec"}, {"reader": "tesseract_rus", "mode": "psm6"}]}
            with patch.object(comparison.roi, "rapidocr_reader", side_effect=lambda *_args, **_kwargs: {"status": "ok", "outputs": []}), patch(
                "ocr.experiments.run_resolution_comparison.estimate_detector_pixels", return_value=100
            ), patch("ocr.experiments.region_pipeline.cpu_provider_report", side_effect=lambda _engine: {"sessions": {"det": ["CPUExecutionProvider"], "rec": ["CPUExecutionProvider"]}, "available": []}), patch(
                "ocr.experiments.region_pipeline._assert_cpu_only", return_value={"sessions": {"det": ["CPUExecutionProvider"], "rec": ["CPUExecutionProvider"]}}
            ) as both_guard, patch("ocr.experiments.field_roi_pipeline.run_tesseract_reader", return_value={"status": "empty", "words": []}):
                comparison.roi.run_hypothesis_readers(block_hypothesis, image_path, engine=engine, tesseract="unused", tessdata_dir=temp)
            both_guard.assert_called_once_with(engine)
            self.assertEqual("verified_cpu_detector_and_recognizer", engine._field_roi_cpu_provider_state)

    def test_cpu_provider_mismatch_aborts_before_tesseract_or_next_reader(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            image_path = Path(temp) / "input.png"
            Image.fromarray(np.zeros((80, 100, 3), dtype=np.uint8)).save(image_path)
            cfg = type("Config", (), {"Det": {"limit_side_len": 736, "limit_type": "min"}, "Global": {}})()
            engine = type("LazyEngine", (), {"cfg": cfg, "_field_roi_cpu_provider_state": "pending_lazy_first_call"})()
            hypothesis = {"id": "place-roi", "field_id": "birth_place", "crop_bounds": [10, 10, 90, 40], "reader_plan": [{"reader": "rapidocr", "mode": "det_rec"}, {"reader": "tesseract_rus", "mode": "psm6"}]}
            call_order = []
            with patch.object(comparison.roi, "rapidocr_reader", side_effect=lambda *_args, **_kwargs: (call_order.append("rapid") or {"status": "ok", "outputs": []})), patch(
                "ocr.experiments.run_resolution_comparison.estimate_detector_pixels", return_value=100
            ), patch("ocr.experiments.region_pipeline.cpu_provider_report", return_value={"sessions": {"det": ["CUDAExecutionProvider"], "rec": ["CPUExecutionProvider"]}, "available": ["CUDAExecutionProvider"]}), patch(
                "ocr.experiments.region_pipeline._assert_cpu_only", side_effect=RuntimeError("provider detail")
            ), patch("ocr.experiments.field_roi_pipeline.run_tesseract_reader", side_effect=lambda *_args, **_kwargs: call_order.append("tesseract")):
                with self.assertRaises(comparison.roi.FieldROIError) as error:
                    comparison.roi.run_hypothesis_readers(hypothesis, image_path, engine=engine, tesseract="unused", tessdata_dir=temp)
            self.assertEqual("rapidocr_cpu_provider_validation_failed", error.exception.code)
            self.assertEqual(["rapid"], call_order)
            self.assertEqual("failed", engine._field_roi_cpu_provider_state)


if __name__ == "__main__":
    unittest.main()
