"""Small synthetic guards for timeout cleanup and the reference-read boundary."""
from __future__ import annotations

import importlib.util
import copy
import json
from pathlib import Path
import signal
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent


def load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError("test module could not be loaded")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RUNNER = load_module("auto_vl_runner_test", ROOT / "run_comparison.py")
SCORER = load_module("auto_vl_scorer_test", ROOT / "score_comparison.py")


class FakeTimedOutProcess:
    pid = 12345

    def __init__(self) -> None:
        self.timeouts: list[float] = []

    def wait(self, timeout: float) -> None:
        self.timeouts.append(timeout)
        if len(self.timeouts) == 1:
            raise subprocess.TimeoutExpired("fake", timeout)


class EvaluationGuardTests(unittest.TestCase):
    def test_stop_group_sends_kill_after_term_grace_and_bounded_reap(self) -> None:
        process = FakeTimedOutProcess()
        signals: list[tuple[int, int]] = []
        with patch.object(RUNNER.os, "killpg", side_effect=lambda pid, sig: signals.append((pid, sig))):
            RUNNER._stop_group(process)
        self.assertEqual(signals, [(process.pid, signal.SIGTERM), (process.pid, signal.SIGKILL)])
        self.assertEqual(process.timeouts, [RUNNER.TERMINATE_GRACE, RUNNER.KILL_GRACE])

    def test_incomplete_run_is_rejected_before_either_reference_is_opened(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path = Path(tmp) / "manifest.json"
            manifest_path.write_text(json.dumps({"schema_version": SCORER.SCHEMA, "status": "failed"}), encoding="utf-8")
            original_load_json = SCORER.load_json
            opened: list[Path] = []

            def guarded_load(path: Path, code: str):
                resolved = Path(path).resolve()
                if resolved in {SCORER.OLD_GOLD.resolve(), SCORER.NEW_GOLD.resolve()}:
                    opened.append(resolved)
                    raise AssertionError("reference read attempted for incomplete run")
                return original_load_json(path, code)

            with patch.object(SCORER, "load_json", side_effect=guarded_load):
                with self.assertRaises(SCORER.ScoreError):
                    SCORER.score(manifest_path, Path(tmp) / "report", SCORER.COMPARISON_RUN_DIR / "manifest.json",
                                 SCORER.COMPARISON_AUDIT, SCORER.COMPARISON_AUDIT_SHA256)
            self.assertEqual(opened, [])

    def test_comparison_arguments_are_mandatory_before_reference_reads(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path = Path(tmp) / "manifest.json"
            manifest_path.write_text("{}", encoding="utf-8")
            opened: list[Path] = []
            original_load_json = SCORER.load_json

            def guarded_load(path: Path, code: str):
                resolved = Path(path).resolve()
                if resolved in {SCORER.OLD_GOLD.resolve(), SCORER.NEW_GOLD.resolve()}:
                    opened.append(resolved)
                    raise AssertionError("reference read attempted without mandatory pair arguments")
                return original_load_json(path, code)

            invalid_arguments = (
                (None, None, None),
                (SCORER.COMPARISON_RUN_DIR / "manifest.json", None, None),
                (None, SCORER.COMPARISON_AUDIT, SCORER.COMPARISON_AUDIT_SHA256),
                (SCORER.COMPARISON_RUN_DIR / "manifest.json", SCORER.COMPARISON_AUDIT, None),
            )
            with patch.object(SCORER, "load_json", side_effect=guarded_load):
                for comparison_manifest, comparison_audit, comparison_sha in invalid_arguments:
                    with self.assertRaises(SCORER.ScoreError):
                        SCORER.score(manifest_path, Path(tmp) / "report", comparison_manifest,
                                     comparison_audit, comparison_sha)
            self.assertEqual(opened, [])

    def test_baseline_parity_only_ignores_field_evidence_set_order(self) -> None:
        frozen = {
            "fields": {
                "patronymic": {
                    "evidence": {"line_ids": ["line-c", "line-a", "line-b"], "pass_ids": ["pass-2", "pass-1"]},
                    "candidates": [
                        {"value": "x", "raw_variants": ["x", "X"], "rules": ["r1", "r2"]},
                        {"value": "y", "raw_variants": ["y"], "rules": ["r3"]},
                    ],
                },
            },
        }
        reordered_evidence = copy.deepcopy(frozen)
        reordered_evidence["fields"]["patronymic"]["evidence"]["line_ids"] = ["line-a", "line-b", "line-c"]
        reordered_evidence["fields"]["patronymic"]["evidence"]["pass_ids"] = ["pass-1", "pass-2"]
        expected = RUNNER.baseline_equivalence_json(frozen)
        self.assertEqual(expected, RUNNER.baseline_equivalence_json(reordered_evidence))
        self.assertEqual(expected, SCORER.baseline_equivalence_json(reordered_evidence))

        changed_candidate_order = copy.deepcopy(reordered_evidence)
        changed_candidate_order["fields"]["patronymic"]["candidates"].reverse()
        self.assertNotEqual(expected, RUNNER.baseline_equivalence_json(changed_candidate_order))
        changed_raw_variants = copy.deepcopy(reordered_evidence)
        changed_raw_variants["fields"]["patronymic"]["candidates"][0]["raw_variants"].reverse()
        self.assertNotEqual(expected, RUNNER.baseline_equivalence_json(changed_raw_variants))
        changed_rules = copy.deepcopy(reordered_evidence)
        changed_rules["fields"]["patronymic"]["candidates"][0]["rules"].reverse()
        self.assertNotEqual(expected, RUNNER.baseline_equivalence_json(changed_rules))

    def test_posthoc_scorer_is_excluded_but_exact_runtime_pins_are_required(self) -> None:
        current = SCORER.python_source_manifest()
        expected = {
            str((SCORER.REPO / "ocr/research/tools/ocr-auto-vl-eval/run_comparison.py").resolve()),
            str(SCORER.HISTORICAL_SCORER.resolve()),
            str((SCORER.REPO / "ocr/experiments/auto_vl_pipeline.py").resolve()),
            str((SCORER.REPO / "ocr/experiments/local_paths.py").resolve()),
        }
        self.assertEqual(set(current), expected)
        self.assertNotIn(str((ROOT / "score_comparison.py").resolve()), current)
        SCORER.validate_python_source_pins(current, dict(current), dict(current))

        modified = dict(current)
        key = next(iter(modified))
        modified[key] = "0" * 64
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_python_source_pins(current, current, modified)

        missing = dict(current)
        missing.pop(key)
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_python_source_pins(missing, current, current)

        extra = dict(current)
        extra[str(ROOT / "unexpected.py")] = "0" * 64
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_python_source_pins(extra, extra, extra)

    def test_historical_go_scope_accepts_only_the_audited_planner_change(self) -> None:
        planner = str((SCORER.REPO / SCORER.APPROVED_GO_SOURCE_CHANGE).resolve())
        baseline = {
            str(SCORER.REPO / "cmd/ocr-eval/main.go"): "a" * 64,
            planner: "b" * 64,
        }
        current = dict(baseline)
        current[planner] = "c" * 64
        SCORER.validate_historical_go_scope(baseline, current)

        unrelated = dict(current)
        unrelated[str(SCORER.REPO / "cmd/ocr-eval/main.go")] = "d" * 64
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_historical_go_scope(baseline, unrelated)

        missing = dict(current)
        missing.pop(planner)
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_historical_go_scope(baseline, missing)

        extra = dict(current)
        extra[str(SCORER.REPO / "internal/passport/unreviewed.go")] = "e" * 64
        with self.assertRaises(SCORER.ScoreError):
            SCORER.validate_historical_go_scope(baseline, extra)

    def test_historical_audit_sha_guard_fails_before_reference_reads(self) -> None:
        opened: list[Path] = []
        original_load_json = SCORER.load_json

        def guarded_load(path: Path, code: str):
            resolved = Path(path).resolve()
            if resolved in {SCORER.OLD_GOLD.resolve(), SCORER.NEW_GOLD.resolve()}:
                opened.append(resolved)
                raise AssertionError("reference read attempted before historical guard")
            return original_load_json(path, code)

        with patch.object(SCORER, "load_json", side_effect=guarded_load):
            with self.assertRaises(SCORER.ScoreError):
                SCORER.validate_comparison_run(
                    SCORER.COMPARISON_RUN_DIR / "manifest.json", SCORER.COMPARISON_AUDIT,
                    "0" * 64, {"go_source_sha256": {"after": {}}},
                )
        self.assertEqual(opened, [])


if __name__ == "__main__":
    unittest.main()
