"""Model-free tests for the private PaddleOCR-VL pilot runner."""

from __future__ import annotations

import json
import os
import signal
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from ocr.experiments import run_paddleocr_vl_trial as trial


def _block(text: str) -> dict[str, object]:
    return {"block_bbox": [0, 0, 100, 20], "block_label": "text", "block_content": text, "block_id": 0, "block_order": 0}


class PaddleOCRVLTrialTests(unittest.TestCase):
    def test_worker_argv_has_only_local_runtime_arguments_and_no_gold(self):
        argv = trial.worker_argv(
            Path("/env/bin/python"), Path("worker.py"), Path("/private/photo.jpg"),
            Path("/private/result.json"), Path("/private/cache-home"),
        )
        self.assertEqual(["--input", "--output", "--cache-root"], [arg for arg in argv if arg.startswith("--")])
        self.assertNotIn("--gold", argv)
        self.assertNotIn("ground-truth.json", " ".join(argv))

    def test_worker_environment_preserves_home_and_scopes_model_caches_without_gold(self):
        source = {"HOME": "/unchanged/user-home", "CODEX_HOME": "/unchanged/codex", "TMPDIR": "/unchanged/tmp", "GROUND_TRUTH": "/private/gold.json", "PATH": "/usr/bin"}
        env = trial.worker_environment(source, Path("/private/cache"))
        self.assertEqual(source["HOME"], env["HOME"])
        self.assertEqual(source["CODEX_HOME"], env["CODEX_HOME"])
        self.assertEqual(source["TMPDIR"], env["TMPDIR"])
        self.assertNotIn("GROUND_TRUTH", env)
        self.assertEqual("/private/cache/paddlex", env["PADDLE_PDX_CACHE_HOME"])
        self.assertEqual("/usr/bin", env["PATH"])

    def test_evaluator_validates_completed_outputs_before_opening_gold(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            inputs = [root / "one.jpg", root / "two.jpg"]
            for index, path in enumerate(inputs):
                path.write_bytes(f"image-{index}".encode())
            private = root / "private"
            private.mkdir()
            gold = root / "gold.json"
            gold.write_text("not json", encoding="utf-8")
            with self.assertRaisesRegex(trial.TrialError, "worker_json_missing_or_invalid"):
                trial.evaluate_outputs(
                    input_hashes={f"sample-{i:02d}": trial.sha256_file(path) for i, path in enumerate(inputs, 1)},
                    result_paths={f"sample-{i:02d}": private / f"missing-{i}.json" for i in (1, 2)},
                    metadata_paths={f"sample-{i:02d}": private / f"missing-meta-{i}.json" for i in (1, 2)},
                    gold_path=gold, private_root=private,
                )
            unknown = private / "unknown.json"
            unknown.write_text(json.dumps({"markdown": "SYNTHETIC_RAW"}), encoding="utf-8")
            with self.assertRaisesRegex(trial.TrialError, "worker_result_shape_unsupported"):
                trial.evaluate_outputs(
                    input_hashes={f"sample-{i:02d}": trial.sha256_file(path) for i, path in enumerate(inputs, 1)},
                    result_paths={"sample-01": unknown, "sample-02": unknown},
                    metadata_paths={}, gold_path=gold, private_root=private,
                )

    def test_evaluator_maps_unique_gold_and_scores_block_presence_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            inputs = [root / "one.jpg", root / "two.jpg"]
            for index, path in enumerate(inputs):
                path.write_bytes(f"image-{index}".encode())
            private = root / "private"
            private.mkdir()
            required = [field for field in trial.FIELDS if field != "patronymic"]
            gold_images = {}
            result_paths = {}
            metadata_paths = {}
            input_hashes = {}
            for index, path in enumerate(inputs, 1):
                sample_id = f"sample-{index:02d}"
                literal_values = {field: f"TOKEN_{field.upper()}" for field in required}
                gold_images[sample_id] = {
                    "source_image_id": sample_id,
                    "source_image_path": str(path),
                    "fields": {field: {"literal": value} for field, value in literal_values.items()},
                }
                result_path = private / f"{sample_id}.json"
                result_path.write_text(json.dumps({"parsing_res_list": [_block(" | ".join(literal_values.values()))]}), encoding="utf-8")
                result_paths[sample_id] = result_path
                metadata_path = private / f"{sample_id}-meta.json"
                metadata_path.write_text(json.dumps({
                    "worker_wall_seconds": 3.0, "model_init_seconds": 1.0, "predict_seconds": 2.0,
                    "rss_sample_peak_kib": 50, "rss_sample_count": 4,
                }), encoding="utf-8")
                metadata_paths[sample_id] = metadata_path
                input_hashes[sample_id] = trial.sha256_file(path)
            gold = root / "gold.json"
            gold.write_text(json.dumps({"images": gold_images}), encoding="utf-8")
            rows = trial.evaluate_outputs(
                input_hashes=input_hashes, result_paths=result_paths, metadata_paths=metadata_paths,
                gold_path=gold, private_root=private,
            )
            self.assertEqual(2, len(rows))
            self.assertEqual(len(required), rows[0]["ocr_required_field_text_presence"])
            self.assertEqual(len(required), rows[0]["ocr_required_fields_total"])
            self.assertEqual(3.0, rows[0]["worker_wall_seconds"])
            self.assertEqual({"ocr_required_field_text_presence", "ocr_required_fields_total", "ocr_optional_patronymic_text_presence", "field_results", "sample_id", "worker_wall_seconds", "model_init_seconds", "predict_seconds", "rss_sample_peak_kib", "rss_sample_count"}, set(rows[0]))
            self.assertTrue((private / "sample-01" / "posthoc-evaluation.json").is_file())

    def test_gold_source_mapping_rejects_duplicate_hash_matches(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image = root / "image.jpg"
            image.write_bytes(b"image")
            duplicate = root / "duplicate.jpg"
            duplicate.write_bytes(b"image")
            gold = root / "gold.json"
            gold.write_text(json.dumps({"images": {
                "first": {"source_image_path": str(image)},
                "duplicate": {"source_image_path": str(duplicate)},
            }}), encoding="utf-8")
            with self.assertRaisesRegex(trial.TrialError, "ground_truth_source_mapping_not_unique"):
                trial.map_gold_records(gold, {"sample-01": trial.sha256_file(image)})

    def test_token_presence_stays_within_single_block_and_preserves_date_tokens(self):
        self.assertTrue(trial.token_sequence_in_one_block("12.03.2000", ["Date: 12.03.2000;"]))
        self.assertFalse(trial.token_sequence_in_one_block("ALPHA BETA", ["ALPHA", "BETA"]))
        self.assertFalse(trial.token_sequence_in_one_block("12.03.2000", ["12.03.2001"]))

    def test_public_summary_allowlist_excludes_raw_text_paths_and_hashes(self):
        sample = {
            "sample_id": "sample-01",
            "field_results": [{"field_id": "surname", "gold_status": "provided", "ocr_token_presence": True}],
            "ocr_required_field_text_presence": 1, "ocr_required_fields_total": 10,
            "ocr_optional_patronymic_text_presence": None,
            "worker_wall_seconds": 5.0, "model_init_seconds": 1.0, "predict_seconds": 3.0,
            "rss_sample_peak_kib": 123, "rss_sample_count": 4,
            "private_raw": "SECRET_NAME", "private_path": "/Users/private/photo.jpg", "sha256": "a" * 64,
        }
        payload = trial.public_summary(samples=trial._public_samples([sample]), versions={"paddleocr": "3.7.0"}, total_seconds=5)
        encoded = json.dumps(payload)
        for forbidden in ("SECRET_NAME", "/Users/private", "a" * 64, "private_raw", "sha256"):
            self.assertNotIn(forbidden, encoded)
        self.assertTrue(payload["incumbent_reference"]["different_metric"])
        self.assertFalse(payload["evaluation_boundary"]["field_accuracy_claimed"])
        self.assertIn("5.000", trial.markdown_summary(payload))

    def test_private_files_have_restrictive_modes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "private"
            trial.write_private_json(root / "nested" / "data.json", {"private": True})
            self.assertEqual(0o700, root.stat().st_mode & 0o777)
            self.assertEqual(0o700, (root / "nested").stat().st_mode & 0o777)
            self.assertEqual(0o600, (root / "nested" / "data.json").stat().st_mode & 0o777)

    def test_deadline_limits_and_existing_unknown_venv_are_rejected(self):
        trial.validate_deadlines(1200, 900, 3600)
        with self.assertRaisesRegex(trial.TrialError, "total_timeout_out_of_bounds"):
            trial.validate_deadlines(1200, 900, 3601)
        with tempfile.TemporaryDirectory() as tmp:
            first = Path(tmp) / "one.jpg"
            second = Path(tmp) / "two.jpg"
            first.write_bytes(b"one")
            second.write_bytes(b"two")
            self.assertEqual([first.resolve(), second.resolve()], trial.validate_inputs([first, second]))
            with self.assertRaisesRegex(trial.TrialError, "exactly_two_inputs_required"):
                trial.validate_inputs([first])
            with self.assertRaisesRegex(trial.TrialError, "duplicate_input_path"):
                trial.validate_inputs([first, first])
            venv = Path(tmp) / ".venv-vl"
            venv.mkdir()
            sentinel = venv / "do-not-touch"
            sentinel.write_text("keep", encoding="utf-8")
            with self.assertRaisesRegex(trial.TrialError, "existing_venv_identity_unknown"):
                trial.ensure_venv(venv, Path(tmp), time.monotonic() + 10, 10)
            self.assertEqual("keep", sentinel.read_text(encoding="utf-8"))

    def test_bounded_subprocess_timeout_terminates_worker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            with self.assertRaisesRegex(trial.TrialError, "child_timeout"):
                trial.run_bounded(
                    [os.sys.executable, "-c", "import time; time.sleep(5)"],
                    cwd=root, env=os.environ.copy(), timeout=0.2,
                    stdout_path=root / "out.log", stderr_path=root / "err.log",
                )
            self.assertTrue((root / "err.log").exists())
            child = subprocess.Popen(
                [os.sys.executable, "-c", "import time; time.sleep(5)"],
                start_new_session=True,
            )
            trial._ACTIVE_CHILDREN.add(child)
            with self.assertRaisesRegex(trial.TrialError, "interrupted_by_sigterm"):
                trial._handle_termination(signal.SIGTERM, None)
            self.assertIsNotNone(child.poll())
            self.assertNotIn(child, trial._ACTIVE_CHILDREN)


if __name__ == "__main__":
    unittest.main()
