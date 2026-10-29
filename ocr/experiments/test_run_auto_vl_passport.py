"""Pure orchestration checks; these tests never invoke OCR, Paddle, or Go."""
from __future__ import annotations

import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import signal
import tempfile
import unittest
from unittest.mock import patch

import run_auto_vl_passport as runner


def write_json(path: Path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(json.dumps(value), encoding="utf-8")
    path.chmod(0o600)


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def executable(path: Path):
    path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    path.chmod(0o700)


class FakeProcessRunner:
    def __init__(self, failure_stage=None):
        self.failure_stage = failure_stage
        self.calls = []
        self.model_sha = None

    def __call__(self, argv, *, cwd, env, stdout_path, stderr_path, timeout, stage):
        self.calls.append((stage, argv, timeout))
        if self.failure_stage == stage:
            raise runner.PipelineFailure(f"{stage}_failed")
        if stage == "ocr":
            stage_dir = Path(argv[argv.index("--stage-dir") + 1])
            original = Path(argv[argv.index("--image") + 1])
            canonical = stage_dir / "canonical-spread.png"
            canonical.write_bytes(b"registered-raster")
            canonical.chmod(0o600)
            raster_sha = sha(canonical)
            observations = {
                "image": {"id": "image:registered-spread", "sha256": raster_sha,
                          "width": 10, "height": 20, "coordinate_frame": "exif_normalized_pixels"},
                "lines": [],
            }
            obs_path = stage_dir / "observations.json"
            write_json(obs_path, observations)
            write_json(stage_dir / "stage-result.json", {
                "status": "completed", "registration_status": "accepted",
                "observations_sha256": sha(obs_path), "raster_path": str(canonical),
                "raster_sha256": raster_sha,
                "raster": {"id": "image:registered-spread", "sha256": raster_sha,
                           "width": 10, "height": 20, "coordinate_frame": "exif_normalized_pixels",
                           "role": "registered_canonical_raster"},
                "template_evidence_path": None, "template_evidence_sha256": None,
            })
            write_json(stage_dir / "stage-status.json", {"status": "completed"})
            self.assert_not_original = original != canonical
        elif stage == "go_plan":
            output = Path(argv[argv.index("--output") + 1])
            plan = Path(argv[argv.index("--vl-plan-output") + 1])
            write_json(output, {"baseline": "private"})
            write_json(plan, {"plan": "go-generated"})
        elif stage == "vl":
            stage_dir = Path(argv[argv.index("--stage-dir") + 1])
            self.vl_image = Path(argv[argv.index("--image") + 1])
            self.vl_plan = Path(argv[argv.index("--plan-file") + 1])
            evidence = stage_dir / "evidence.json"
            write_json(evidence, {"private_text": "OCR_SECRET"})
            self.model_sha = runner._model_tree_snapshot(self.test_model_dir)["tree_sha256"]
            write_json(stage_dir / "stage-result.json", {
                "status": "completed", "evidence_sha256": sha(evidence),
                "model_tree_sha256": self.model_sha, "region_statuses": ["ok"],
            })
            write_json(stage_dir / "stage-status.json", {"status": "completed"})
        elif stage == "go_final":
            self.final_argv = argv
            output = Path(argv[argv.index("--output") + 1])
            write_json(output, {"fields": {"given_name": {"value": "DO_NOT_PRINT"}}})
        return 0.01


class RunAutoVLPassportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.image = self.root / "input.jpg"
        self.image.write_bytes(b"source-image-bytes")
        self.evaluator = self.root / "ocr-eval"
        executable(self.evaluator)
        self.det = self.root / "det.onnx"
        self.rec = self.root / "rec.onnx"
        self.det.write_bytes(b"detector")
        self.rec.write_bytes(b"recognizer")
        self.python_ocr = self.root / "python-ocr"
        self.python_vl = self.root / "python-vl"
        self.python_ocr_target = self.root / "python-ocr-target"
        self.python_vl_target = self.root / "python-vl-target"
        executable(self.python_ocr_target)
        executable(self.python_vl_target)
        self.python_ocr.symlink_to(self.python_ocr_target.name)
        self.python_vl.symlink_to(self.python_vl_target.name)
        self.model = self.root / "vl-model"
        self.model.mkdir()
        (self.model / "inference.yml").write_text("Global:\n  model_name: PaddleOCR-VL-1.6-0.9B\n")
        (self.model / "config.json").write_text("{}\n")
        (self.model / "weights.pdparams").write_bytes(b"fake-weights")
        self.model.chmod(0o700)
        self.output = self.root / "private-run"
        self.args = runner._parser().parse_args([
            "--image", str(self.image), "--image-id", "image-01",
            "--output-dir", str(self.output), "--evaluator", str(self.evaluator),
            "--det-model", str(self.det), "--rec-model", str(self.rec),
            "--vl-model-dir", str(self.model), "--python-ocr", str(self.python_ocr),
            "--python-vl", str(self.python_vl), "--vl-policy", "primary",
        ])

    def tearDown(self):
        self.temp.cleanup()

    def fake_runner(self, failure_stage=None):
        fake = FakeProcessRunner(failure_stage)
        fake.test_model_dir = self.model
        return fake

    def test_multistage_order_uses_registered_raster_and_keeps_policy_private(self):
        fake = self.fake_runner()
        result = runner.run_pipeline(self.args, process_runner=fake)
        self.assertEqual([stage for stage, _, _ in fake.calls], ["ocr", "go_plan", "vl", "go_final"])
        self.assertEqual(fake.calls[0][1][0], str(self.python_ocr.absolute()))
        self.assertEqual(fake.calls[2][1][0], str(self.python_vl.absolute()))
        vl_argv = fake.calls[2][1]
        self.assertNotEqual(Path(vl_argv[vl_argv.index("--image") + 1]), self.image)
        self.assertEqual(Path(vl_argv[vl_argv.index("--image") + 1]), fake.vl_image)
        self.assertEqual(fake.vl_image.name, "canonical-spread.png")
        self.assertEqual(fake.vl_plan, (self.output / "vl-plan.json").resolve())
        for stage in ("go_plan", "go_final"):
            argv = next(argv for name, argv, _ in fake.calls if name == stage)
            self.assertIn("--vl-policy=primary", argv)
            self.assertIn("--passes=all", argv)
            self.assertIn("--normalize-names=true", argv)
            self.assertIn("--mrz-policy=augment", argv)
        final = fake.final_argv
        self.assertEqual(final[final.index("--vl-model-sha256") + 1], fake.model_sha)
        self.assertEqual(result["status"], "completed")
        self.assertEqual(json.loads((self.output / "run-manifest.json").read_text())["policy"], "primary")

    def test_public_status_does_not_print_extracted_or_ocr_text(self):
        fake = self.fake_runner()
        output = io.StringIO()
        with patch.object(runner, "_run_process", fake), contextlib.redirect_stdout(output):
            code = runner.main([
                "--image", str(self.image), "--image-id", "image-01",
                "--output-dir", str(self.output), "--evaluator", str(self.evaluator),
                "--det-model", str(self.det), "--rec-model", str(self.rec),
                "--vl-model-dir", str(self.model), "--python-ocr", str(self.python_ocr),
                "--python-vl", str(self.python_vl), "--vl-policy", "primary",
            ])
        self.assertEqual(code, 0)
        self.assertIn("review_required=true", output.getvalue())
        self.assertNotIn("DO_NOT_PRINT", output.getvalue())
        self.assertNotIn("OCR_SECRET", output.getvalue())

    def test_ocr_failure_stops_before_go_plan_and_vl(self):
        fake = self.fake_runner("ocr")
        with self.assertRaisesRegex(runner.PipelineFailure, "ocr_failed"):
            runner.run_pipeline(self.args, process_runner=fake)
        self.assertEqual([stage for stage, _, _ in fake.calls], ["ocr"])
        manifest = json.loads((self.output / "run-manifest.json").read_text())
        self.assertEqual(manifest["status"], "failed")
        self.assertEqual(manifest["error_code"], "ocr_failed")

    def test_go_plan_failure_stops_before_vl_model_worker(self):
        fake = self.fake_runner("go_plan")
        with self.assertRaisesRegex(runner.PipelineFailure, "go_plan_failed"):
            runner.run_pipeline(self.args, process_runner=fake)
        self.assertEqual([stage for stage, _, _ in fake.calls], ["ocr", "go_plan"])
        self.assertFalse((self.output / "vl").exists())

    def test_kill_process_group_kills_surviving_grandchild_after_parent_exits(self):
        class FakeProcess:
            pid = 84521

            def __init__(self):
                self.exited = False
                self.wait_calls = []

            def poll(self):
                return 0 if self.exited else None

            def wait(self, timeout):
                self.wait_calls.append(timeout)
                self.exited = True
                return 0

        process = FakeProcess()
        signals = []
        group_child_alive = True

        def fake_killpg(pid, sig):
            nonlocal group_child_alive
            signals.append((pid, sig))
            if sig == signal.SIGKILL:
                group_child_alive = False

        with patch.object(runner.os, "killpg", side_effect=fake_killpg):
            runner._kill_process_group(process)
        self.assertEqual([sig for _, sig in signals], [signal.SIGTERM, signal.SIGKILL])
        self.assertEqual(len(process.wait_calls), 2)
        self.assertFalse(group_child_alive)


if __name__ == "__main__":
    unittest.main()
