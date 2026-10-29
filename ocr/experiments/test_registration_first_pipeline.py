from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import cv2
import numpy as np
from PIL import Image

from ocr.experiments import registration_first_pipeline as pipeline
from ocr.experiments.page_registration import RegistrationResult, _build_pages


def _accepted_registration(size: tuple[int, int] = (500, 600), source: str = "contour") -> RegistrationResult:
    width, height = size
    quad = np.array([
        [0.11 * width, 0.07 * height], [0.91 * width, 0.12 * height],
        [0.86 * width, 0.93 * height], [0.14 * width, 0.89 * height],
    ], dtype=np.float64)
    pages = _build_pages(quad, 0.52, size)
    assert pages is not None
    return RegistrationResult(
        "accepted", "provisional_quad_and_seam_supported", 1,
        orientation_status="orientation_unverified",
        source_quad=quad.tolist(), area_fraction=0.8,
        candidate_score=0.9, seam_ratio=0.52, seam_support=0.8,
        pages=pages, input_size=size, selected_candidate_source=source,
    )


class FakeEngine:
    def __init__(self, events: list[str] | None = None) -> None:
        self.calls = 0
        self.events = events if events is not None else []
        self.shapes: list[tuple[int, int, int]] = []

    def __call__(self, image: np.ndarray, **kwargs) -> SimpleNamespace:
        if kwargs != {"use_det": True, "use_cls": False, "use_rec": True}:
            raise AssertionError("whole-page OCR must explicitly restore detection")
        self.calls += 1
        self.events.append(f"ocr-{self.calls}")
        self.shapes.append(image.shape)
        height, width = image.shape[:2]
        box = np.array([
            [10, 10], [min(width - 5, 80), 10],
            [min(width - 5, 80), min(height - 5, 35)], [10, min(height - 5, 35)],
        ], dtype=np.float32)
        return SimpleNamespace(txts=[f"synthetic-{self.calls}"], boxes=[box], scores=[0.91])


class RegistrationFirstPipelineTests(unittest.TestCase):
    def _source(self, root: Path) -> tuple[Path, np.ndarray]:
        image = np.zeros((600, 500, 3), dtype=np.uint8)
        image[:, :, 0] = 37
        image[:, :, 1] = 91
        path = root / "source.png"
        Image.fromarray(image).save(path)
        return path, image

    def test_accepted_contour_registers_before_engine_and_emits_canonical_geometry(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image_path, source = self._source(root)
            private = root / "private"
            events: list[str] = []
            engine = FakeEngine(events)
            registration = _accepted_registration()

            def register(image: np.ndarray, *, text_quads: tuple[()]) -> RegistrationResult:
                events.append("register")
                np.testing.assert_array_equal(image, source)
                self.assertEqual(text_quads, ())
                return registration

            def factory(_det: str | Path, _rec: str | Path) -> FakeEngine:
                events.append("factory")
                return engine

            result = pipeline.build_registered_observations(
                image_path, "sample", "det", "rec", private_output_dir=private,
                engine_factory=factory, registration_fn=register,
                provider_validator=lambda _: {"sessions": {"det": ["CPUExecutionProvider"], "rec": ["CPUExecutionProvider"]}},
            )

            self.assertEqual(events, ["register", "factory", "ocr-1", "ocr-2"])
            self.assertEqual(result["status"], "accepted")
            self.assertEqual(engine.calls, 2)
            observations = result["observations"]
            self.assertEqual(observations["image"]["coordinate_frame"], "exif_normalized_pixels")
            self.assertNotEqual(observations["image"]["sha256"], pipeline._sha256_bytes(image_path.read_bytes()))
            self.assertEqual(observations["runtime"]["registered_spread"]["coordinate_space_role"], "derived_registered_spread")
            self.assertEqual(observations["document"]["seam_y"], float(observations["passes"][1]["crop"][1]))
            self.assertEqual([item["kind"] for item in observations["passes"]], ["upper_page", "lower_page"])
            self.assertEqual(len(observations["lines"]), 2)
            for line in observations["lines"]:
                pass_record = next(item for item in observations["passes"] if item["id"] == line["pass_id"])
                _, y0, x1, y1 = pass_record["crop"]
                quad = np.asarray(line["quad"], dtype=np.float64)
                self.assertGreaterEqual(float(quad[:, 0].min()), 0)
                self.assertLessEqual(float(quad[:, 0].max()), x1)
                self.assertGreaterEqual(float(quad[:, 1].min()), y0)
                self.assertLessEqual(float(quad[:, 1].max()), y1)
                if line["pass_id"] == "upper_page":
                    self.assertLessEqual(float(quad[:, 1].max()), observations["document"]["seam_y"])
                else:
                    self.assertGreaterEqual(float(quad[:, 1].min()), observations["document"]["seam_y"])

            lineage = json.loads(Path(result["lineage_path"]).read_text(encoding="utf-8"))
            self.assertEqual(lineage["source_image"]["sha256"], pipeline._sha256_bytes(image_path.read_bytes()))
            self.assertEqual(lineage["canonical_image"]["sha256"], observations["image"]["sha256"])
            for name in ("image_upper", "image_lower"):
                item = lineage["page_mappings"][name]
                canonical_to_source = np.asarray(item["canonical_to_source"], dtype=np.float64)
                page_to_source = np.asarray(item["registered_page_to_source"], dtype=np.float64)
                canonical_to_page = np.asarray(item["canonical_to_registered_page"], dtype=np.float64)
                np.testing.assert_allclose(canonical_to_source, page_to_source @ canonical_to_page, atol=1e-10)
                line = next(record for record in observations["lines"] if record["pass_id"] == pipeline._PASS_KIND[name])
                canonical_quad = np.asarray(line["quad"], dtype=np.float64)
                source_quad = cv2.perspectiveTransform(
                    canonical_quad.astype(np.float32).reshape(1, 4, 2), canonical_to_source.astype(np.float32)
                )[0]
                canonical_roundtrip = cv2.perspectiveTransform(
                    source_quad.reshape(1, 4, 2), np.linalg.inv(canonical_to_source).astype(np.float32)
                )[0]
                np.testing.assert_allclose(canonical_roundtrip, canonical_quad, atol=1e-3)
                registered_source_quad = np.asarray(item["source_quad"], dtype=np.float64)
                self.assertGreaterEqual(float(source_quad[:, 0].min()), float(registered_source_quad[:, 0].min()) - 1.0)
                self.assertLessEqual(float(source_quad[:, 0].max()), float(registered_source_quad[:, 0].max()) + 1.0)
                self.assertGreaterEqual(float(source_quad[:, 1].min()), float(registered_source_quad[:, 1].min()) - 1.0)
                self.assertLessEqual(float(source_quad[:, 1].max()), float(registered_source_quad[:, 1].max()) + 1.0)
            self.assertTrue(all((Path(result[key]).stat().st_mode & 0o777) == 0o600 for key in ("canonical_image_path", "lineage_path", "observations_path")))
            self.assertEqual(private.stat().st_mode & 0o777, 0o700)
            np.testing.assert_array_equal(np.asarray(Image.open(image_path)), source)

    def test_rejected_and_ambiguous_registration_fall_back_without_warping(self) -> None:
        for status in ("rejected", "ambiguous"):
            with self.subTest(status=status), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                image_path, _ = self._source(root)
                registration = RegistrationResult(status, "test_geometry_status", 0)
                events: list[str] = []
                engine = FakeEngine(events)

                def register(_image: np.ndarray, *, text_quads: tuple[()]) -> RegistrationResult:
                    events.append("register")
                    return registration

                def factory(_det: str | Path, _rec: str | Path) -> FakeEngine:
                    events.append("factory")
                    return engine

                with mock.patch.object(pipeline, "warp_registered_page") as warp, mock.patch(
                    "ocr.experiments.region_pipeline.build_observations",
                    return_value={"schema_version": "passport-ocr-observations/v2", "lines": []},
                ) as fallback:
                    result = pipeline.build_registered_observations(
                        image_path, "sample", "det", "rec", private_output_dir=root / "private",
                        engine_factory=factory, registration_fn=register,
                    )
                self.assertEqual(result["status"], "fallback")
                self.assertIn(f"registration_{status}", result["reason"])
                self.assertEqual(events, ["register", "factory"])
                self.assertEqual(engine.calls, 0)
                warp.assert_not_called()
                fallback.assert_called_once()
                self.assertTrue(fallback.call_args.kwargs["include_mrz"])

    def test_missing_or_noncontour_source_provenance_fails_closed(self) -> None:
        for source in (None, "line_intersections"):
            with self.subTest(source=source), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                image_path, _ = self._source(root)
                registration = _accepted_registration(source=source)  # type: ignore[arg-type]
                engine = FakeEngine()
                with mock.patch.object(pipeline, "warp_registered_page") as warp, mock.patch(
                    "ocr.experiments.region_pipeline.build_observations",
                    return_value={"schema_version": "passport-ocr-observations/v2", "lines": []},
                ):
                    result = pipeline.build_registered_observations(
                        image_path, "sample", "det", "rec", private_output_dir=root / "private",
                        engine=engine, registration_fn=lambda _image, **_kwargs: registration,
                    )
                self.assertEqual(result["status"], "fallback")
                self.assertEqual(result["reason"], "registered_source_unsupported_or_unverified")
                self.assertEqual(engine.calls, 0)
                warp.assert_not_called()


if __name__ == "__main__":
    unittest.main()
