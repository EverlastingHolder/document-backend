from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from ocr.experiments import run_mrz_comparison as runner
from ocr.experiments import run_algorithm_comparison as base


class PublicMRZSummaryTests(unittest.TestCase):
    def test_timing_boundary_says_ocr_was_shared(self) -> None:
        self.assertIn("same v2 observations, including the MRZ OCR passes", runner.TIMING_BOUNDARY)
        self.assertIn("not a VIZ-only versus VIZ+MRZ end-to-end speed comparison", runner.TIMING_BOUNDARY)
        self.assertIn("extraction seconds cover Go post-processing only", runner.TIMING_BOUNDARY)

    def test_only_counts_and_codes_are_returned(self) -> None:
        extraction = {
            "mrz": {
                "status": "valid",
                "issues": ["raw-secret-issue-is-not-a-code"],
                "candidates": [{
                    "raw_lines": ["SECRET-RAW-LINE-1", "SECRET-RAW-LINE-2"],
                    "result": {
                        "status": "valid",
                        "raw_lines": ["SECRET-PARSER-LINE"],
                        "fields": {"surname": "SECRET-NAME"},
                        "checks": [{"name": "birth_date", "status": "valid", "expected": "7", "observed": "7"}],
                        "issues": ["line_length_invalid"],
                        "name_issues": ["punctuation_not_recoverable"],
                    },
                }],
            }
        }
        summary = runner.public_mrz_summary(extraction)
        encoded = json.dumps(summary)
        for secret in ("SECRET-RAW", "SECRET-NAME", "expected", "observed"):
            self.assertNotIn(secret, encoded)
        self.assertEqual(summary["status_counts"]["valid"], 1)
        self.assertEqual(summary["checks"]["birth_date"]["valid"], 1)
        self.assertEqual(summary["issues"], ["line_length_invalid"])
        self.assertEqual(summary["name_issues"], ["punctuation_not_recoverable"])

    def test_off_has_no_diagnostics(self) -> None:
        summary = runner.public_mrz_summary({"fields": {}})
        self.assertFalse(summary["enabled"])
        self.assertEqual(sum(summary["status_counts"].values()), 0)


class ReusableObservationTests(unittest.TestCase):
    def _write(self, root: Path, version: str) -> tuple[Path, Path]:
        image = root / "neutral.jpg"
        image.write_bytes(b"neutral-test-image-bytes")
        sample = root / "work" / "sample-01"
        sample.mkdir(parents=True)
        model_hashes = {
            "detector": base.MODEL_HASHES["det"][1],
            "recognizer": base.MODEL_HASHES["rec"][1],
        }
        data = {
            "schema_version": version,
            "image": {"id": "sample-01", "sha256": base.sha256_file(image), "coordinate_frame": "exif_normalized_pixels"},
            "runtime": {"model_sha256": model_hashes},
            "passes": [{"id": "mrz_rapid", "kind": "mrz", "status": "ok"}],
        }
        (sample / "observations.json").write_text(json.dumps(data), encoding="utf-8")
        return image, root / "work"

    def test_requires_matching_v2_with_mrz_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image, work = self._write(root, "passport-ocr-observations/v2")
            loaded = runner.load_v2_observations(work, [("sample-01", image, "private-key")])
            self.assertEqual(loaded["sample-01"], work / "sample-01" / "observations.json")

    def test_rejects_v1_cache(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image, work = self._write(root, "passport-ocr-observations/v1")
            with self.assertRaises(runner.MRZComparisonError):
                runner.load_v2_observations(work, [("sample-01", image, "private-key")])


if __name__ == "__main__":
    unittest.main()
