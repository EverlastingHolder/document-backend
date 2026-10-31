"""Synthetic tests for frame binding, rotation normalization and ROI crops."""

from __future__ import annotations

import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path

from PIL import Image

sys.path.insert(0, str(Path(__file__).resolve().parent))
from run_serial_roi_preview import (  # noqa: E402
    PreviewError,
    assess_field_split,
    centered_field_crop_bounds,
    crop_bounds,
    field_crop_plan,
    field_split_status_label,
    load_bound_frame,
    page_frame_request,
    validate_proposal,
)


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class SerialROIPreviewTests(unittest.TestCase):
    def _synthetic_job(self, root: Path, *, wrong_dimensions: bool = False) -> tuple[dict, Path]:
        dataset = root / "current-five"
        sample = dataset / "sample-01"
        (sample / "output/ocr").mkdir(parents=True)
        source = sample / "input.jpg"
        image = Image.new("RGB", (2, 3), (30, 80, 130))
        exif = Image.Exif()
        exif[274] = 6  # rotate 90 degrees clockwise for display
        image.save(source, format="JPEG", exif=exif)
        raw = source.read_bytes()
        obs_dimensions = [2, 3] if wrong_dimensions else [3, 2]
        observations = {
            "schema_version": "passport-ocr-observations/v1",
            "image": {"id": "synthetic-raster", "width": obs_dimensions[0],
                      "height": obs_dimensions[1], "sha256": sha(raw),
                      "coordinate_frame": "exif_normalized_pixels"},
        }
        (sample / "output/ocr/observations.json").write_text(json.dumps(observations))
        item = {
            "sample": "sample-01",
            "source_raster": ".local/ocr-research/datasets/current-five/sample-01/input.jpg",
            "source_kind": "input.jpg, EXIF-transposed in memory",
            "plan_dimensions": [3, 2],
            "plan_image_sha256": sha(raw),
            "coordinate_frame": "exif_normalized_pixels",
        }
        return item, dataset

    def test_exif_transpose_and_raw_sha_binding(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            item, dataset = self._synthetic_job(Path(temp))
            frame, observations, source, original = load_bound_frame(item, dataset)
            self.assertEqual(frame.size, (3, 2))
            self.assertEqual(observations["image"]["id"], "synthetic-raster")
            self.assertEqual(source.read_bytes(), original)

    def test_rejects_observation_dimensions_before_render(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            item, dataset = self._synthetic_job(Path(temp), wrong_dimensions=True)
            with self.assertRaisesRegex(PreviewError, "normalized raster does not match"):
                load_bound_frame(item, dataset)

    def test_rejects_wrong_manifest_sha(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            item, dataset = self._synthetic_job(Path(temp))
            item["plan_image_sha256"] = "0" * 64
            with self.assertRaisesRegex(PreviewError, "manifest SHA-256"):
                load_bound_frame(item, dataset)

    def test_registered_page_frames_require_bound_canonical_lineage(self) -> None:
        raster = {"id": "registered-raster", "width": 100, "height": 80, "sha256": "a" * 64}
        item = {"source_kind": "pipeline canonical-spread.png"}
        lineage = {
            "schema_version": "passport-registered-spread-lineage/v1",
            "canonical_image": {**raster},
            "page_mappings": {
                "lower": {"physical_page": "lower_page", "canonical_rect": [0, 40, 100, 80]},
                "upper": {"physical_page": "upper_page", "canonical_rect": [0, 0, 100, 40]},
            },
        }
        result = page_frame_request(item, raster, lineage)
        self.assertEqual(result["page_frame_kind"], "registered_pages")
        self.assertEqual([p["id"] for p in result["pages"]], ["lower_page", "upper_page"])

        lineage["canonical_image"]["sha256"] = "b" * 64
        fallback = page_frame_request(item, raster, lineage)
        self.assertEqual(fallback, {"page_frame_kind": "whole_document_fallback"})

    def test_source_frames_always_use_whole_document_fallback(self) -> None:
        raster = {"id": "source-raster", "width": 100, "height": 80, "sha256": "a" * 64}
        item = {"source_kind": "input.jpg, EXIF-transposed in memory"}
        lineage = {"canonical_image": {**raster}, "page_mappings": {"page": {"physical_page": "page", "canonical_rect": [0, 0, 100, 80]}}}
        self.assertEqual(page_frame_request(item, raster, lineage), {"page_frame_kind": "whole_document_fallback"})

    def test_centered_half_open_crop_contains_roi_and_clips_to_image(self) -> None:
        self.assertEqual(crop_bounds((10.2, 5.5, 20.1, 15.3), (40, 30)), (6, 2, 24, 19))
        self.assertEqual(crop_bounds((0, 0, 8, 6), (40, 30)), (0, 0, 11, 9))

    def test_field_crop_padding_stops_at_vertical_sibling_midpoint(self) -> None:
        proposal = {
            "series": {"bounds": [20, 20, 30, 40]},
            "number": {"bounds": [20, 42, 30, 65]},
        }
        split = (40 + 42) / 2
        series = centered_field_crop_bounds(tuple(proposal["series"]["bounds"]), (100, 100), "series", proposal)
        number = centered_field_crop_bounds(tuple(proposal["number"]["bounds"]), (100, 100), "number", proposal)
        self.assertLessEqual(series[3], split)
        self.assertGreaterEqual(number[1], split)
        self.assertLess(series[1], proposal["series"]["bounds"][1])
        self.assertGreater(number[3], proposal["number"]["bounds"][3])

    def test_field_crop_padding_stops_at_horizontal_sibling_midpoint(self) -> None:
        proposal = {
            "series": {"bounds": [10, 20, 25, 30]},
            "number": {"bounds": [28, 20, 50, 30]},
        }
        split = (25 + 28) / 2
        series = centered_field_crop_bounds(tuple(proposal["series"]["bounds"]), (100, 100), "series", proposal)
        number = centered_field_crop_bounds(tuple(proposal["number"]["bounds"]), (100, 100), "number", proposal)
        self.assertLessEqual(series[2], split)
        self.assertGreaterEqual(number[0], split)

    def test_rejects_non_finite_crop_padding(self) -> None:
        with self.assertRaises(PreviewError):
            crop_bounds((0, 0, 10, 10), (40, 30), float("nan"))

    def test_validates_proposal_frame_and_requires_review(self) -> None:
        raster = {"id": "synthetic-raster", "width": 100, "height": 80, "sha256": "a" * 64}
        roi = {"status": "proposed", "frame_id": raster["id"], "quad": [[10, 10], [20, 10], [20, 20], [10, 20]],
               "bounds": [10, 10, 20, 20], "manual_review_required": True}
        proposal = {"schema_version": "passport-serial-roi-preview/v1", "status": "proposed",
                    "raster": raster, "series": roi, "number": {"status": "unavailable"},
                    "shared_column": roi, "rotation_hypotheses": [0, 90, 180, 270]}
        validate_proposal(proposal, raster)
        proposal["series"] = {**roi, "frame_id": "other-frame"}
        with self.assertRaisesRegex(PreviewError, "ROI frame"):
            validate_proposal(proposal, raster)

    def test_rejects_identical_series_and_number_geometry(self) -> None:
        same = {"status": "proposed", "bounds": [10, 20, 20, 50],
                "reason": "sibling_numeric_geometry_only"}
        proposal = {"series": same, "number": {**same, "reason": None}}
        assessment = assess_field_split(same, proposal["number"])
        crops, omissions = field_crop_plan(proposal)
        self.assertEqual(assessment["status"], "invalid")
        self.assertEqual(set(crops), set())
        self.assertIn("identical", omissions["series"])
        self.assertIn("invalid", omissions["number"])

    def test_marks_heavily_overlapping_field_geometry_provisional(self) -> None:
        series = {"status": "proposed", "bounds": [0, 0, 10, 30]}
        number = {"status": "proposed", "bounds": [0, 12, 10, 42]}
        assessment = assess_field_split(series, number)
        self.assertEqual(assessment["status"], "provisional")
        crops, omissions = field_crop_plan({"series": series, "number": number})
        self.assertEqual(crops, {})
        self.assertEqual(set(omissions), {"series", "number"})

    def test_ambiguous_locator_label_preserves_invalid_field_split_status(self) -> None:
        roi = {"status": "ambiguous"}
        proposal = {"status": "ambiguous", "series": roi, "number": roi, "shared_column": roi}
        label = field_split_status_label(
            {"status": "invalid", "reason": "series_and_number_bounds_identical"}, proposal
        )
        self.assertIn("AMBIGUOUS + INVALID", label)
        self.assertIn("series_and_number_bounds_identical", label)

    def test_keeps_distinct_series_and_number_crops(self) -> None:
        proposal = {
            "series": {"status": "proposed", "bounds": [10, 20, 20, 35]},
            "number": {"status": "proposed", "bounds": [10, 36, 20, 60]},
        }
        crops, omissions = field_crop_plan(proposal)
        self.assertEqual(set(crops), {"series", "number"})
        self.assertEqual(omissions, {})


if __name__ == "__main__":
    unittest.main()
