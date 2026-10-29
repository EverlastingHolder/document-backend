"""Pure runner checks that do not load images, OCR models, or private observations."""

from __future__ import annotations

import subprocess
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from ocr.experiments import run_algorithm_comparison as runner


class AlgorithmRunnerTests(unittest.TestCase):
    def test_optional_patronymic_label_checks_value_presence_before_state(self) -> None:
        self.assertEqual(
            runner.patronymic_status({
                "expected_present": True,
                "value_present": False,
                "state": "uncertain",
                "recognized_correct": False,
                "suggested_correct": False,
            }),
            "missing",
        )
        self.assertEqual(
            runner.patronymic_status({
                "expected_present": True,
                "value_present": True,
                "state": "uncertain",
                "recognized_correct": False,
                "suggested_correct": False,
            }),
            "uncertain-wrong",
        )

    def test_gender_canonicalization_matches_known_printed_forms_only(self) -> None:
        self.assertEqual(runner.canonical("M", "gender"), runner.canonical("МУЖ.", "gender"))
        self.assertEqual(runner.canonical("F", "gender"), runner.canonical("ЖЕН.", "gender"))
        self.assertNotEqual(runner.canonical("M", "gender"), runner.canonical("ЖЕН.", "gender"))
        self.assertNotEqual(runner.canonical("F", "gender"), runner.canonical("МУЖ.", "gender"))
        self.assertNotEqual(runner.canonical("M?", "gender"), runner.canonical("M", "gender"))
        self.assertNotEqual(runner.canonical("МУЖ/ЖЕН", "gender"), runner.canonical("M", "gender"))

    def test_scoring_uses_selected_gender_value_not_candidate_list(self) -> None:
        gold_fields = {name: {"literal": "expected"} for name in runner.FIELDS}
        gold_fields["gender"] = {"literal": "МУЖ."}
        extraction_fields = {
            name: {"state": "uncertain", "value": "other"}
            for name in runner.FIELDS
        }
        extraction_fields["gender"] = {
            "state": "uncertain",
            "value": "F",
            "candidates": [{"value": "M"}],
        }

        result = runner.score_extraction({"fields": extraction_fields}, gold_fields)

        self.assertFalse(result["fields"]["gender"]["suggested_correct"])
        self.assertEqual(result["fields"]["gender"]["state"], "uncertain")

    def test_evaluator_runs_directly_with_go_bool_flag_syntax(self) -> None:
        evaluator = Path("/tmp/ocr-eval")
        observations = Path("/tmp/sample/observations.json")
        extraction = Path("/tmp/sample/A_original_raw.json")

        for normalize_names, bool_text in ((False, "false"), (True, "true")):
            with self.subTest(normalize_names=normalize_names):
                command = runner.evaluator_command(
                    evaluator, observations, extraction, "original", normalize_names
                )
                completed = SimpleNamespace(returncode=0, stdout="ok", stderr="")
                with patch.object(runner.subprocess, "run", return_value=completed) as run:
                    result = runner.run_executable(
                        Path(command[0]), command[1:], env={}, timeout=1, phase="go_extract"
                    )

                self.assertEqual(result, "ok")
                run.assert_called_once()
                argv = run.call_args.args[0]
                self.assertEqual(argv, [
                    "/tmp/ocr-eval", "--input", "/tmp/sample/observations.json",
                    "--output", "/tmp/sample/A_original_raw.json", "--passes=original",
                    f"--normalize-names={bool_text}",
                ])
                self.assertNotIn("go", argv)


if __name__ == "__main__":
    unittest.main()
