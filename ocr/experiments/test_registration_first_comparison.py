from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from ocr.experiments import run_registration_first_comparison as runner


class RegistrationFirstComparisonTests(unittest.TestCase):
    def _inputs(self, root: Path) -> tuple[Path, Path, Path]:
        image1 = root / "input-01.jpg"
        image2 = root / "input-02.jpg"
        image1.write_bytes(b"synthetic source one")
        image2.write_bytes(b"synthetic source two")
        gold = root / "ground-truth.json"
        gold.write_text(json.dumps({"images": {
            "image01": {"source_image_path": str(image1), "fields": {"given_name": {"literal": "private gold one"}}},
            "image02": {"source_image_path": str(image2), "fields": {"given_name": {"literal": "private gold two"}}},
        }}), encoding="utf-8")
        return image1, image2, gold

    def test_gold_is_read_posthoc_and_matched_by_source_path_sha(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image1, image2, gold = self._inputs(root)
            source1 = runner.incumbent.sha256_file(image1)
            source2 = runner.incumbent.sha256_file(image2)
            events = ["baseline-ocr-01", "registered-ocr-01", "baseline-go-01", "registered-go-01",
                      "baseline-ocr-02", "registered-ocr-02", "baseline-go-02", "registered-go-02"]
            completed = [
                {"sample_id": "sample-01", "arm": "baseline", "route": "accepted", "source_sha256": source1,
                 "ocr_seconds": 1.0, "go_seconds": 0.1, "extraction": {"private": "baseline output one"}},
                {"sample_id": "sample-01", "arm": "registration_first", "route": "accepted", "source_sha256": source1,
                 "ocr_seconds": 1.0, "go_seconds": 0.1, "extraction": {"private": "registered output one"}},
                {"sample_id": "sample-02", "arm": "baseline", "route": "fallback", "source_sha256": source2,
                 "ocr_seconds": 2.0, "go_seconds": 0.1, "extraction": {"private": "baseline output two"}},
                {"sample_id": "sample-02", "arm": "registration_first", "route": "fallback", "source_sha256": source2,
                 "ocr_seconds": 2.0, "go_seconds": 0.1, "extraction": {"private": "registered output two"}},
            ]
            with mock.patch.object(runner.incumbent, "score_extraction", side_effect=lambda _ext, fields: {"fields_used": fields}) as score:
                rows = runner._posthoc_score(gold, completed, event_log=events)
            self.assertEqual(events[-1], "gold_read")
            self.assertEqual(len(rows), 4)
            self.assertEqual(rows[0]["fields_used"]["given_name"]["literal"], "private gold one")
            self.assertEqual(rows[1]["fields_used"]["given_name"]["literal"], "private gold one")
            self.assertEqual(rows[2]["fields_used"]["given_name"]["literal"], "private gold two")
            self.assertEqual(rows[3]["fields_used"]["given_name"]["literal"], "private gold two")
            self.assertTrue(all(row["gold_mapping_verified"] for row in rows))
            self.assertEqual(score.call_count, 4)

    def test_duplicate_gold_source_hash_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            image1, image2, gold = self._inputs(root)
            payload = json.loads(gold.read_text(encoding="utf-8"))
            payload["images"]["duplicate-image01"] = dict(payload["images"]["image01"])
            gold.write_text(json.dumps(payload), encoding="utf-8")
            completed = []
            for sample_id, route, image in (("sample-01", "accepted", image1), ("sample-02", "fallback", image2)):
                for arm in ("baseline", "registration_first"):
                    completed.append({"sample_id": sample_id, "arm": arm, "route": route,
                                      "source_sha256": runner.incumbent.sha256_file(image), "extraction": {},
                                      "ocr_seconds": 0, "go_seconds": 0})
            with self.assertRaisesRegex(runner.RunnerError, "ground_truth_source_path_sha_missing_or_ambiguous"):
                runner._posthoc_score(gold, completed)

    def test_posthoc_scoring_requires_both_go_outputs_before_opening_gold(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            missing_gold = Path(temporary) / "does-not-exist.json"
            with self.assertRaisesRegex(runner.RunnerError, "posthoc_scoring_requires_four_completed_go_outputs"):
                runner._posthoc_score(missing_gold, [{"source_sha256": "a" * 64}])

    def test_aggregate_report_writer_keeps_payload_private(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "private" / "report.json"
            report = {"schema_version": "test", "results": [{"suggested_correct": 2}]}
            runner._private_json(output, report)
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            self.assertEqual(output.parent.stat().st_mode & 0o777, 0o700)
            content = output.read_text(encoding="utf-8")
            self.assertNotIn("private gold", content)
            self.assertNotIn("ocr output", content)


if __name__ == "__main__":
    unittest.main()
