from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import numpy as np
from PIL import Image

from ocr.experiments import run_registration_probe as probe
from ocr.experiments.page_registration import RegistrationResult, _build_pages


def _fixture_registration(size: tuple[int, int] = (500, 600)) -> RegistrationResult:
    width, height = size
    quad = np.array([[20, 20], [width - 20, 20], [width - 20, height - 20], [20, height - 20]], dtype=np.float64)
    pages = _build_pages(quad, 0.52, size)
    assert pages is not None
    return RegistrationResult(
        "accepted", "provisional_quad_and_seam_supported", 1,
        source_quad=quad.tolist(), area_fraction=0.8, edge_support=(0.9, 0.9, 0.9, 0.9),
        candidate_score=0.9, seam_ratio=0.52, seam_support=0.8,
        pages=pages, input_size=size, selected_candidate_source="contour",
    )


class FakeEngine:
    def __init__(self, text: str = "private synthetic token") -> None:
        self.calls = 0
        self.text = text

    def __call__(self, image: np.ndarray) -> SimpleNamespace:
        self.calls += 1
        height, width = image.shape[:2]
        box = np.array([[10, 10], [min(width - 10, 300), 10], [min(width - 10, 300), 35], [10, 35]], dtype=np.float32)
        return SimpleNamespace(txts=[self.text], boxes=[box], scores=[0.91])


def _valid_baseline(sample_id: str, source_hash: str, models: dict[str, str]) -> dict[str, object]:
    quads = []
    for x, y in ((100, 100), (300, 100), (100, 300), (300, 300)):
        quads.append({
            "id": f"{sample_id}-line-{len(quads) + 1}",
            "pass_id": "original",
            "text_raw": "private baseline token",
            "score": 0.9,
            "quad": [[x, y], [x + 30, y], [x + 30, y + 10], [x, y + 10]],
        })
    return {
        "schema_version": probe.SCHEMA_V2,
        "image": {"id": sample_id, "sha256": source_hash, "coordinate_frame": "exif_normalized_pixels"},
        "runtime": {"model_sha256": models},
        "passes": [{"id": "original", "kind": "original"}, {"id": "mrz", "kind": "mrz"}],
        "lines": quads,
    }


class RegistrationProbeTests(unittest.TestCase):
    def test_rejected_registration_never_calls_engine(self) -> None:
        engine = FakeEngine()
        registration = RegistrationResult("rejected", "contour_not_found", 0)
        public, private = probe.probe_image(
            sample_id="sample-01", image_rgb=np.zeros((100, 100, 3), dtype=np.uint8), image_sha256="a" * 64,
            baseline={"text_quads": []}, engine=engine, det_sha256="d" * 64, rec_sha256="r" * 64,
            registration=registration,
        )
        self.assertEqual(engine.calls, 0)
        self.assertEqual(public["ocr_status"], "not_run")
        self.assertEqual(private["pages"], [])

    def test_accepted_page_registration_calls_each_page_once_and_maps_original_quads(self) -> None:
        engine = FakeEngine()
        registration = _fixture_registration()
        rgb = np.full((600, 500, 3), 240, dtype=np.uint8)
        public, private = probe.probe_image(
            sample_id="sample-01", image_rgb=rgb, image_sha256="a" * 64, baseline={"text_quads": []},
            engine=engine, det_sha256="d" * 64, rec_sha256="r" * 64,
            provider_validator=lambda _: {"sessions": {"det": ["CPUExecutionProvider"], "rec": ["CPUExecutionProvider"]}},
            registration=registration,
        )
        self.assertEqual(engine.calls, 2)
        self.assertEqual(public["page_count"], 2)
        self.assertEqual(public["line_count"], 2)
        for page in private["pages"]:
            self.assertEqual(len(page["lines"]), 1)
            self.assertIn("quad_page", page["lines"][0])
            self.assertIn("quad_original", page["lines"][0])
            self.assertTrue(np.isfinite(np.asarray(page["lines"][0]["quad_original"])).all())

    def test_baseline_rejects_wrong_source_hash_and_model_pins(self) -> None:
        models = {"detector": "d" * 64, "recognizer": "r" * 64}
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "observations.json"
            path.write_text(json.dumps(_valid_baseline("sample-01", "a" * 64, models)), encoding="utf-8")
            with mock.patch.dict(probe.base.MODEL_HASHES, {
                "det": ("det.onnx", models["detector"]), "rec": ("rec.onnx", models["recognizer"]),
            }):
                with self.assertRaisesRegex(probe.ProbeError, "baseline_observations_sha_id_or_model_pin_mismatch"):
                    probe._validate_baseline(path, "sample-01", "b" * 64)
                bad = _valid_baseline("sample-01", "a" * 64, {"detector": "x" * 64, "recognizer": models["recognizer"]})
                path.write_text(json.dumps(bad), encoding="utf-8")
                with self.assertRaisesRegex(probe.ProbeError, "baseline_observations_sha_id_or_model_pin_mismatch"):
                    probe._validate_baseline(path, "sample-01", "a" * 64)

    def test_model_directory_requires_exact_local_pins(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            det = root / "det.onnx"
            rec = root / "rec.onnx"
            det.write_bytes(b"synthetic detector")
            rec.write_bytes(b"synthetic recognizer")
            det_hash = hashlib.sha256(det.read_bytes()).hexdigest()
            rec_hash = hashlib.sha256(rec.read_bytes()).hexdigest()
            with mock.patch.dict(probe.base.MODEL_HASHES, {
                "det": ("det.onnx", det_hash), "rec": ("rec.onnx", rec_hash),
            }):
                self.assertEqual(probe._validate_models(root), {"detector": det, "recognizer": rec})
                with mock.patch.dict(probe.base.MODEL_HASHES, {"rec": ("rec.onnx", "0" * 64)}):
                    with self.assertRaisesRegex(probe.ProbeError, "local_model_pin_mismatch_rec"):
                        probe._validate_models(root)

    def test_rapidocr_resolution_configuration_is_read_from_local_defaults(self) -> None:
        config = probe._rapidocr_resolution_config()
        self.assertEqual(config["global_min_side_len"], 30)
        self.assertEqual(config["global_max_side_len"], 2000)
        self.assertTrue(config["global_preprocess_enabled"])
        self.assertEqual(config["detector_limit_side_len"], 736)
        self.assertEqual(config["detector_limit_type"], "min")

    def test_public_report_omits_raw_text_while_private_report_retains_it(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            model_dir = root / "models"
            baseline_root = root / "baseline"
            work_root = root / "private-work"
            output_dir = root / "public-output"
            for path in (model_dir, baseline_root):
                path.mkdir(mode=0o700)
            det = model_dir / "det.onnx"
            rec = model_dir / "rec.onnx"
            det.write_bytes(b"synthetic detector")
            rec.write_bytes(b"synthetic recognizer")
            det_hash = hashlib.sha256(det.read_bytes()).hexdigest()
            rec_hash = hashlib.sha256(rec.read_bytes()).hexdigest()
            expected_models = {"detector": det_hash, "recognizer": rec_hash}
            images = []
            with mock.patch.dict(probe.base.MODEL_HASHES, {
                "det": ("det.onnx", det_hash), "rec": ("rec.onnx", rec_hash),
            }), mock.patch.object(probe, "register_spread", return_value=_fixture_registration()) as registration_mock:
                for index in (1, 2):
                    image_path = root / f"image-{index:02d}.png"
                    color = (220, 80, 50) if index == 1 else (50, 80, 220)
                    Image.fromarray(np.full((600, 500, 3), color, dtype=np.uint8)).save(image_path)
                    images.append(image_path)
                    sample_id = f"sample-{index:02d}"
                    sample_dir = baseline_root / sample_id
                    sample_dir.mkdir(mode=0o700)
                    baseline = _valid_baseline(sample_id, probe._sha256(image_path), expected_models)
                    (sample_dir / "observations.json").write_text(json.dumps(baseline), encoding="utf-8")

                engine = FakeEngine("xPRIVATEy")
                report = probe.run_probe(
                    images=images, baseline_work_root=baseline_root, model_dir=model_dir,
                    work_root=work_root, output_dir=output_dir,
                    engine_factory=lambda _det, _rec: engine,
                    provider_validator=lambda _: {"sessions": {"det": ["CPUExecutionProvider"], "rec": ["CPUExecutionProvider"]}},
                )
            self.assertEqual(registration_mock.call_count, 2)
            self.assertEqual(engine.calls, 4)
            public_json = (output_dir / "registration-probe.json").read_text(encoding="utf-8")
            public_md = (output_dir / "registration-probe.md").read_text(encoding="utf-8")
            self.assertNotIn("xPRIVATEy", public_json + public_md)
            self.assertNotIn("source_paths", public_json)
            private_json = (work_root / "registration-probe-private.json").read_text(encoding="utf-8")
            self.assertIn("xPRIVATEy", private_json)
            self.assertEqual(report["results"][0]["orientation_status"], "orientation_unverified")
            self.assertEqual(report["rapidocr_configured_resolution"]["detector_limit_side_len"], 736)
            self.assertEqual(report["rapidocr_configured_resolution"]["detector_limit_type"], "min")
            self.assertEqual((work_root.stat().st_mode & 0o777), 0o700)
            self.assertEqual(((work_root / "sample-01" / "registration-private.json").stat().st_mode & 0o777), 0o600)
            self.assertEqual(((output_dir / "registration-probe.json").stat().st_mode & 0o777), 0o600)
            self.assertEqual(((work_root / "sample-01" / "registration-overlay.jpg").stat().st_mode & 0o777), 0o600)
            page_one = np.asarray(Image.open(work_root / "sample-01" / "image_upper-page.png"))
            page_two = np.asarray(Image.open(work_root / "sample-02" / "image_upper-page.png"))
            self.assertGreater(float(page_one[:, :, 0].mean()), float(page_two[:, :, 0].mean()))
            self.assertLess(float(page_one[:, :, 2].mean()), float(page_two[:, :, 2].mean()))

    def test_geometry_only_probe_needs_no_baseline_or_models(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            images = []
            for index in (1, 2):
                image_path = root / f"image-{index:02d}.png"
                Image.fromarray(np.full((600, 500, 3), 150 + index, dtype=np.uint8)).save(image_path)
                images.append(image_path)
            work_root = root / "private-work"
            output_dir = root / "report"
            with mock.patch.object(probe, "register_spread", return_value=_fixture_registration()):
                report = probe.run_probe(
                    images=images, baseline_work_root=None, model_dir=None,
                    work_root=work_root, output_dir=output_dir, geometry_only=True,
                )
            self.assertEqual(report["status"], "PASS")
            public_json = (output_dir / "registration-probe.json").read_text(encoding="utf-8")
            private_json = (work_root / "registration-probe-private.json").read_text(encoding="utf-8")
            self.assertNotIn("rapidocr_configured_resolution", public_json)
            self.assertNotIn("model_sha256", private_json)
            self.assertNotIn("source_paths", public_json)
            self.assertIn('"selected_candidate_source": "contour"', private_json)
            self.assertTrue((work_root / "sample-01" / "image_upper-page.png").is_file())


if __name__ == "__main__":
    unittest.main()
