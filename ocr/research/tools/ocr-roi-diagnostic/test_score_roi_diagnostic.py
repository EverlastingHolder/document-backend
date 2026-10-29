from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import score_roi_diagnostic as scorer


def synthetic_inputs():
    tasks = {}
    references = {}
    baseline = {}
    for index, sample in enumerate(scorer.SAMPLES):
        refs = {}
        base_fields = {}
        for field in scorer.FIELDS:
            expected = "М" if field == "gender" else "СЕВЕРНЫЙ ГОРОД"
            refs[field] = {"literal": expected, "normalized": expected.casefold()}
            base_fields[field] = {"value": expected}
        references[sample] = {"fields": refs}
        baseline[scorer.SAMPLE_TO_BASELINE[sample]] = {"extraction": {"fields": base_fields}}
    for index, key in enumerate(sorted(scorer.EXPECTED_TASKS)):
        sample, field = key
        expected = "М" if field == "gender" else "СЕВЕРНЫЙ ГОРОД"
        if index % 3 == 0:
            rapid, rapid_status = expected, "ok"
        elif index % 3 == 1:
            rapid, rapid_status = "ДРУГОЕ ЗНАЧЕНИЕ", "ok"
        else:
            rapid, rapid_status = None, "error"
        tess, tess_status = expected, "ok"
        tasks[key] = {
            "sample_id": sample,
            "field_id": field,
            "reader_statuses": {"rapidocr": rapid_status, "tesseract": tess_status},
            "rapidocr_natural_full_field_text_raw": rapid,
            "tesseract": {"natural_full_field_text_raw": tess},
        }
    return tasks, references, baseline


class RoiScorerTests(unittest.TestCase):
    def test_runtime_readiness_failure_precedes_reference_open(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            gold_old = root / "old-reference.json"
            gold_new = root / "new-reference.json"
            with patch.object(scorer, "_json", side_effect=AssertionError("reference was opened")) as read_json:
                with self.assertRaises(scorer.ScoreError) as raised:
                    scorer.run(
                        root / "missing-results.json", root / "missing-crops.json",
                        root / "baseline.json", root / "audit.json", gold_old, gold_new,
                        root / "output",
                    )
            self.assertEqual(str(raised.exception), "runtime_artifact_missing")
            read_json.assert_not_called()
            self.assertFalse(gold_old.exists())
            self.assertFalse(gold_new.exists())

    def test_synthetic_fourteen_task_scores_and_consensus_are_separate(self):
        tasks, references, baseline = synthetic_inputs()
        report = scorer.build_report(tasks, references, baseline)
        self.assertEqual(len(report["samples"]), 14)
        for reader in ("rapidocr", "tesseract", "baseline_go"):
            for field in scorer.FIELDS:
                counts = report["totals_by_reader_and_field"][f"{reader}:{field}"]
                expected_n = sum(field_id == field for _, field_id in scorer.EXPECTED_TASKS)
                self.assertEqual(sum(counts.values()), expected_n)
        self.assertEqual(sum(sum(counts.values()) for counts in report["totals_by_reader_and_field"].values()), 42)
        self.assertEqual(report["reader_pair_totals_by_field"]["gender"]["exact_agreement"], 2)
        self.assertEqual(report["reader_pair_totals_by_field"]["gender"]["consensus_correct"], 2)
        self.assertNotIn("СЕВЕРНЫЙ ГОРОД", str(report))

    def test_gender_map_is_closed_and_names_do_not_use_gender_map(self):
        self.assertEqual(scorer.canonical("МУЖ.", "gender"), "m")
        self.assertEqual(scorer.canonical("Ж.", "gender"), "f")
        self.assertEqual(scorer.canonical("МУЖ.", "given_name"), "муж.")
        self.assertEqual(scorer.canonical("ДРУГОЕ", "gender"), "другое")
        self.assertEqual(scorer._expected({"literal": "МУЖ.", "normalized": "m"}, "gender"), "m")
        self.assertEqual(scorer._expected({"literal": "МУЖ.", "normalized": "М"}, "given_name"), "МУЖ.")

    def test_production_visual_map_is_exact_and_uppercase_only(self):
        source = "ABCEHKMOPTX Y"
        mapped = "".join(scorer.VISUAL_LATIN_TO_CYR.get(character, character) for character in source)
        self.assertEqual(scorer.production_visual_canonical(source, "given_name"), scorer.canonical(mapped, "given_name"))
        self.assertEqual(scorer.production_visual_canonical("aDz", "given_name"), "adz")
        self.assertEqual(scorer.production_visual_canonical("MУЖ.", "gender"), "m")

    def test_whole_field_comparison_preserves_word_order(self):
        forward = scorer.canonical("город\nновый мир", "birth_place")
        reverse = scorer.canonical("мир новый\nгород", "birth_place")
        self.assertEqual(forward, "город новый мир")
        self.assertNotEqual(forward, reverse)
        self.assertNotEqual(scorer.canonical("новый мир", "birth_place"), scorer.canonical("город новый мир", "birth_place"))

    def test_historical_baseline_guard_allows_later_go_source_change(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            old_go = root / "old-go-source.go"
            old_go.write_text("historical source", encoding="utf-8")
            old_hash = "0" * 64
            current_go = root / "current-go-source.go"
            current_go.write_text("later v3 source", encoding="utf-8")
            extraction_paths = {}
            for sample in scorer.SAMPLE_TO_BASELINE.values():
                path = root / sample / "go-extraction.json"
                path.parent.mkdir()
                path.write_text('{"fields":{"issuer":{"value":"synthetic"}}}', encoding="utf-8")
                extraction_paths[sample] = {"go-extraction.json": path}
            frozen_manifest = {
                "schema_version": "passport-five-image-inference/v1",
                "source_code_sha256": {"before": {str(old_go): old_hash}, "after": {str(old_go): old_hash}},
            }
            # The historical guard has already validated its pinned audit and
            # artifacts; its frozen manifest hash is intentionally not compared
            # with the later current Go source bytes.
            with patch.object(scorer, "verify_historical_baseline", return_value=(frozen_manifest, extraction_paths)):
                rows = scorer._baseline_rows(scorer.BASELINE_MANIFEST, scorer.BASELINE_AUDIT)
            self.assertNotEqual(scorer.sha256(old_go), scorer.sha256(current_go))
            self.assertEqual(set(rows), set(scorer.SAMPLE_TO_BASELINE.values()))
            self.assertEqual(rows["sample-01"]["extraction"]["fields"]["issuer"]["value"], "synthetic")


if __name__ == "__main__":
    unittest.main()
