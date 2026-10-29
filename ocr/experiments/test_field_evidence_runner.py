from __future__ import annotations

import json
import hashlib
import tempfile
import unittest
from pathlib import Path
from subprocess import CompletedProcess
from unittest.mock import patch

from ocr.experiments import run_field_evidence_comparison as runner


def fixture_files(root: Path) -> tuple[Path, Path]:
    cache = root / "cache"
    cache.mkdir()
    run = {"model_sha256": {"eslav_PP-OCRv5_rec_mobile.onnx": "b" * 64}, "samples": []}
    gold_images = {}
    for sample_id in ("sample-01", "sample-02"):
        source_image = root / f"{sample_id}.jpg"
        source_image.write_bytes(sample_id.encode())
        image_sha = hashlib.sha256(source_image.read_bytes()).hexdigest()
        run["samples"].append({"sample_id": sample_id, "image_sha256": image_sha})
        gold_images[sample_id] = {"source_image_id": sample_id, "source_image_path": str(source_image), "fields": {"given_name": {"literal": "SYNTHETIC_TOKEN"}}}
        sample = cache / sample_id
        (sample / "baseline").mkdir(parents=True)
        obs = {
            "schema_version": "passport-ocr-observations/v1",
            "image": {"id": sample_id, "width": 1000, "height": 1000, "sha256": image_sha, "coordinate_frame": "exif_normalized_pixels"},
            "document": {"bounds": [0, 0, 1000, 1000], "seam_y": 500},
            "passes": [{"id": "original", "kind": "original", "rotation_degrees": 0, "status": "ok"}],
            "lines": [
                {"id": "surname", "pass_id": "original", "text_raw": "ALPHA", "score": 0.9, "quad": [[700, 600], [760, 600], [760, 610], [700, 610]]},
                {"id": "given", "pass_id": "original", "text_raw": "BETA", "score": 0.9, "quad": [[701, 620], [761, 620], [761, 630], [701, 630]]},
                {"id": "patronymic", "pass_id": "original", "text_raw": "GAMMA", "score": 0.9, "quad": [[699, 640], [759, 640], [759, 650], [699, 650]]},
                {"id": "dob", "pass_id": "original", "text_raw": "01.02.2000", "score": 0.9, "quad": [[700, 690], [760, 690], [760, 700], [700, 700]]},
            ],
        }
        candidates = [{"value": "OLD_VALUE", "raw": "OLD_VALUE", "raw_variants": ["OLD_VALUE"], "rules": ["baseline_rule"], "line_ids": ["given"], "pass_ids": ["original"], "context": "baseline"}]
        ext = {"schema_version": "passport-extraction/v1", "image_id": sample_id, "review_required": True, "fields": {
            "surname": {"candidates": [{"line_ids": ["surname"]}], "evidence": {"line_ids": ["surname"]}},
            "given_name": {"value": "OLD_VALUE", "state": "uncertain", "candidates": candidates, "rules": ["baseline_rule"], "issues": [], "evidence": {"line_ids": ["given"], "pass_ids": ["original"]}, "context": "baseline"},
            "patronymic": {"candidates": [{"line_ids": ["patronymic"]}], "evidence": {"line_ids": ["patronymic"]}},
        }}
        plan = {"hypotheses": [{
            "id": "given_name-roi-01", "field_id": "given_name", "role_quality": {"state": "supported_row"}, "proposal_method": "viz_layout_v1",
            "source_pass_ids": ["original"], "source_line_ids": ["given"], "crop_bounds": [690, 618, 820, 638],
            "adjacent_anchors": [{"role": "birth_date", "line_id": "dob", "pass_id": "original", "relation": "upper_row_boundary"}],
            "reader_plan": [{"reader": "rapidocr", "mode": "rec_only"}],
        }]}
        readers = [{"hypothesis_id": "given_name-roi-01", "field_id": "given_name", "reader": "rapidocr", "status": "ok", "candidate_count": 1,
                    "outputs": [{"text_raw": "SYNTHETIC_TOKEN", "raw_confidence": 0.8, "geometry_kind": "entire_crop", "box_source": [690, 618, 820, 638]}]}]
        for name, value in (("observations.json", obs), ("extraction.json", ext)):
            (sample / "baseline" / name).write_text(json.dumps(value), encoding="utf-8")
        (sample / "field-roi-plan.json").write_text(json.dumps(plan), encoding="utf-8")
        (sample / "reader-results.json").write_text(json.dumps(readers), encoding="utf-8")
    (cache / "run.json").write_text(json.dumps(run), encoding="utf-8")
    gold = root / "ground-truth.json"
    gold.write_text(json.dumps({"images": gold_images}), encoding="utf-8")
    return cache, gold


class FieldEvidenceRunnerTests(unittest.TestCase):
    def test_go_runs_and_baseline_parity_precede_gold_with_sanitized_public_report(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cache, gold = fixture_files(root)
            events = []
            def fake_go(command, **kwargs):
                events.append("go")
                output = Path(command[command.index("--output") + 1])
                sample_id = Path(command[command.index("--input") + 1]).parent.parent.name
                baseline = json.loads((cache / sample_id / "baseline/extraction.json").read_text(encoding="utf-8"))
                result = json.loads(json.dumps(baseline))
                if "--field-evidence" in command:
                    field = result["fields"]["given_name"]
                    field["candidates"].append({"value": "SYNTHETIC_TOKEN", "raw": "SYNTHETIC_TOKEN", "raw_variants": ["SYNTHETIC_TOKEN"], "rules": ["field_roi"], "line_ids": [], "pass_ids": [], "context": "field_roi"})
                    field["value"] = None
                    field["state"] = "uncertain"
                    field["issues"] = ["conflicting_field_candidates"]
                output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                output.write_text(json.dumps(result), encoding="utf-8")
                output.chmod(0o600)
                return CompletedProcess(command, 0, "completed", "")

            original_read = runner._read_json

            def tracked_read(path, code):
                if path == gold:
                    events.append("gold")
                return original_read(path, code)

            output = root / "public"
            work = root / "private"
            with patch.object(runner, "_read_json", tracked_read):
                summary = runner.run_comparison(cache_root=cache, work_root=work, output_dir=output, gold_path=gold, go_runner=fake_go)
            self.assertEqual("gold", events[-1])
            self.assertEqual(4, events.count("go"))
            self.assertTrue(all(event == "go" for event in events[:-1]))
            self.assertTrue(summary["gold_loaded_after_adapter_and_go_runs"])
            report = (output / "field-evidence-comparison.json").read_text(encoding="utf-8")
            markdown = (output / "field-evidence-comparison.md").read_text(encoding="utf-8")
            self.assertNotIn("SYNTHETIC_TOKEN", report)
            self.assertNotIn("SYNTHETIC_TOKEN", markdown)
            self.assertEqual(0o600, (work / "sample-01/field-evidence.json").stat().st_mode & 0o777)
            self.assertEqual(0o600, (output / "field-evidence-comparison.json").stat().st_mode & 0o777)

    def test_manifest_image_hash_mismatch_fails_before_adapter_or_go(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cache, gold = fixture_files(root)
            path = cache / "sample-01/baseline/observations.json"
            obs = json.loads(path.read_text(encoding="utf-8"))
            obs["image"]["sha256"] = "f" * 64
            path.write_text(json.dumps(obs), encoding="utf-8")
            calls = []
            with self.assertRaises(runner.ComparisonError) as error:
                runner.run_comparison(cache_root=cache, work_root=root / "private", output_dir=root / "public", gold_path=gold, go_runner=lambda *args, **kwargs: calls.append(True))
            self.assertEqual("cached_observation_manifest_sha_mismatch", error.exception.code)
            self.assertEqual([], calls)

    def test_duplicate_gold_source_hash_fails_closed_after_go_runs(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cache, gold = fixture_files(root)
            gold_data = json.loads(gold.read_text(encoding="utf-8"))
            original = gold_data["images"]["sample-01"]
            gold_data["images"]["duplicate-source"] = dict(original)
            gold.write_text(json.dumps(gold_data), encoding="utf-8")
            calls = []

            def fake_go(command, **kwargs):
                calls.append(True)
                sample_id = Path(command[command.index("--input") + 1]).parent.parent.name
                output = Path(command[command.index("--output") + 1])
                output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                output.write_text((cache / sample_id / "baseline/extraction.json").read_text(encoding="utf-8"), encoding="utf-8")
                output.chmod(0o600)
                return CompletedProcess(command, 0, "completed", "")

            with self.assertRaises(runner.ComparisonError) as error:
                runner.run_comparison(cache_root=cache, work_root=root / "private", output_dir=root / "public", gold_path=gold, go_runner=fake_go)
            self.assertEqual("ground_truth_source_sha_ambiguous", error.exception.code)
            self.assertEqual(4, len(calls))


if __name__ == "__main__":
    unittest.main()
