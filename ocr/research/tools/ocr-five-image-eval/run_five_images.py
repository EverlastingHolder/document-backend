#!/usr/bin/env python3
"""Run current registered/template OCR and Go extraction privately on five fixed images.

This driver never reads reference data. Run it once for the frozen baseline and
again with a new label/output directory for a future paired regression.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys

_CODE_ROOT = Path(__file__).resolve().parents[4]
if str(_CODE_ROOT) not in sys.path:
    sys.path.insert(0, str(_CODE_ROOT))
from ocr.experiments.local_paths import (
    ARCHIVE_OUTPUT_ROOT,
    ARCHIVE_WORK_ROOT,
    PRIVATE_RESEARCH_ROOT,
    RAPIDOCR_MODEL_DIR,
    REPO_ROOT,
    VL_MODEL_DIR,
)
import time


WORKSPACE = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
MODEL_DIR = RAPIDOCR_MODEL_DIR
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
PYTHON = Path(os.environ.get("PASSPORT_OCR_PYTHON", sys.executable))
SOURCES = (
    ("sample-01", WORKSPACE / "work/ocr-passport-tests/inputs/image-01.jpg"),
    ("sample-02", WORKSPACE / "work/ocr-passport-tests/inputs/image-02.jpg"),
    ("sample-03", WORKSPACE / "work/ocr-new-examples/inputs/sample03.jpg"),
    ("sample-04", WORKSPACE / "work/ocr-new-examples/inputs/sample04.jpg"),
    ("sample-05", WORKSPACE / "work/ocr-new-examples/inputs/sample05.jpg"),
)
MODEL_FILES = (
    MODEL_DIR / "ch_PP-OCRv5_det_mobile.onnx",
    MODEL_DIR / "eslav_PP-OCRv5_rec_mobile.onnx",
)
PINNED_MODEL_HASHES = {
    "ch_PP-OCRv5_det_mobile.onnx": "4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae",
    "eslav_PP-OCRv5_rec_mobile.onnx": "08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab",
}


class DriverError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def private_dir(path: Path) -> None:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.chmod(0o700)
    if path.stat().st_mode & 0o077:
        raise DriverError("private_directory_permissions_invalid")


def write_private_json(path: Path, payload: object) -> None:
    private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def source_code_manifest() -> dict[str, str]:
    files = [Path(__file__).resolve()]
    files.extend(sorted((REPO / "ocr/experiments").glob("*.py")))
    files.extend(sorted((REPO / "cmd").rglob("*.go")))
    files.extend(sorted((REPO / "internal").rglob("*.go")))
    files.extend((REPO / "go.mod", REPO / "go.sum"))
    return {str(path): sha256(path) for path in files if path.is_file()}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", help="Run label, for example baseline or after")
    parser.add_argument("--output-dir", type=Path, help="New private directory; existing directories are refused")
    parser.add_argument("--per-image-timeout", type=int, default=150)
    parser.add_argument("--overall-timeout", type=int, default=900)
    parser.add_argument("--internal-worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--worker-sample", choices=[sample_id for sample_id, _ in SOURCES], help=argparse.SUPPRESS)
    parser.add_argument("--evaluator", type=Path, help=argparse.SUPPRESS)
    return parser.parse_args()


def process_group(command: list[str], *, cwd: Path, env: dict[str, str], timeout: float, log_path: Path) -> int:
    private_dir(log_path.parent)
    with log_path.open("ab") as log:
        os.fchmod(log.fileno(), 0o600)
        process = subprocess.Popen(
            command, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
            stdout=log, stderr=log, start_new_session=True,
        )
        try:
            return process.wait(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired as reap_error:
                raise DriverError("process_reap_timeout") from reap_error
            raise DriverError("process_timeout") from error


def worker(args: argparse.Namespace) -> int:
    os.umask(0o077)
    if Path(sys.executable).resolve() != PYTHON.resolve():
        raise DriverError("expected_pinned_python_interpreter")
    sample_id = args.worker_sample
    source = dict(SOURCES)[sample_id]
    sample_dir = args.output_dir.expanduser().absolute()
    private_dir(sample_dir)
    stage = "worker_import"
    started = time.monotonic()
    try:
        sys.path.insert(0, str(REPO))
        from ocr.experiments.registration_first_pipeline import _write_private_json
        from ocr.experiments.template_field_pipeline import build_template_passport

        stage = "ocr_inference"
        fresh = build_template_passport(
            source, sample_id, MODEL_FILES[0], MODEL_FILES[1],
            private_output_dir=sample_dir / "ocr-private",
        )
        observations = fresh["observations"]
        observations_path = sample_dir / "observations.json"
        _write_private_json(observations_path, observations)
        evidence_path = fresh.get("template_evidence_path")

        stage = "go_evaluation"
        extraction_path = sample_dir / "go-extraction.json"
        command = [
            str(args.evaluator.resolve()), "--input", str(observations_path), "--output", str(extraction_path),
            "--passes=all", "--normalize-names=true", "--mrz-policy=augment",
        ]
        if evidence_path:
            command += ["--template-evidence", str(evidence_path)]
        env = os.environ.copy()
        env.update({"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})
        try:
            result = subprocess.run(
                command, cwd=REPO, env=env, stdin=subprocess.DEVNULL,
                stdout=None, stderr=None, timeout=20,
            )
        except subprocess.TimeoutExpired as error:
            raise DriverError("go_evaluation_timeout") from error
        if result.returncode != 0:
            raise DriverError("go_evaluation_failed")
        extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
        if extraction.get("image_id") != observations.get("image", {}).get("id"):
            raise DriverError("go_extraction_image_id_mismatch")
        write_private_json(sample_dir / "worker-status.json", {
            "sample_id": sample_id, "status": "completed", "stage": "complete",
            "template_plan_status": fresh.get("template_plan", {}).get("status", "unknown"),
            "observation_sha256": sha256(observations_path),
            "extraction_sha256": sha256(extraction_path),
            "elapsed_seconds": round(time.monotonic() - started, 3),
        })
        return 0
    except DriverError as error:
        code = error.code
    except Exception:
        code = "unexpected_worker_error"
    write_private_json(sample_dir / "worker-status.json", {
        "sample_id": sample_id, "status": "failed", "stage": stage, "failure_code": code,
        "elapsed_seconds": round(time.monotonic() - started, 3),
    })
    return 2


def run(args: argparse.Namespace) -> None:
    os.umask(0o077)
    if args.internal_worker:
        if not args.worker_sample or not args.output_dir or not args.evaluator:
            raise DriverError("worker_arguments_invalid")
        raise SystemExit(worker(args))
    if not args.label or not args.output_dir:
        raise DriverError("label_and_output_directory_required")
    if Path(sys.executable).resolve() != PYTHON.resolve():
        raise DriverError("expected_pinned_python_interpreter")
    if args.per_image_timeout < 1 or args.per_image_timeout > 180:
        raise DriverError("per_image_timeout_must_be_1_to_180_seconds")
    if args.overall_timeout < 1 or args.overall_timeout > 900:
        raise DriverError("overall_timeout_must_be_1_to_900_seconds")
    output = args.output_dir.expanduser().absolute()
    if output.exists():
        raise DriverError("output_directory_already_exists")
    private_dir(output.parent)
    output.mkdir(mode=0o700)
    output.chmod(0o700)
    private_dir(output)
    evaluator = output / "ocr-eval"

    stage = "input_hashes"
    inputs_before: dict[str, str] = {}
    models_before: dict[str, str] = {}
    code_before: dict[str, str] = {}
    results: list[dict[str, object]] = []
    go_version = "unavailable"
    python_versions: dict[str, str] = {}
    started_all = time.monotonic()
    manifest_path = output / "manifest.json"
    try:
        for sample_id, path in SOURCES:
            if not path.is_file():
                raise DriverError("input_file_missing")
            inputs_before[sample_id] = sha256(path)
        models_before = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        if len(models_before) != len(MODEL_FILES) or any(
            PINNED_MODEL_HASHES[Path(path).name] != digest for path, digest in models_before.items()
        ):
            raise DriverError("pinned_model_hash_mismatch")
        code_before = source_code_manifest()

        stage = "toolchain_preflight"
        sys.path.insert(0, str(REPO))
        from ocr.experiments.run_algorithm_comparison import check_toolchain
        go_version, python_versions = check_toolchain(PYTHON.resolve(), GO, MODEL_DIR)
        env = os.environ.copy()
        env.update({"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})

        stage = "go_build"
        remaining = args.overall_timeout - (time.monotonic() - started_all)
        if remaining <= 0:
            raise DriverError("overall_timeout")
        try:
            build_rc = process_group(
                [str(GO), "build", "-o", str(evaluator), "./cmd/ocr-eval"],
                cwd=REPO, env=env, timeout=min(55, remaining), log_path=output / "go-build.log",
            )
        except DriverError as error:
            raise DriverError("go_build_timeout") from error
        if build_rc != 0:
            raise DriverError("go_build_failed")
        evaluator.chmod(0o700)

        for index, (sample_id, _source_path) in enumerate(SOURCES, start=1):
            stage = f"inference_{sample_id}"
            remaining = args.overall_timeout - (time.monotonic() - started_all)
            if remaining <= 0:
                raise DriverError("overall_timeout")
            sample_dir = output / sample_id
            private_dir(sample_dir)
            log_path = sample_dir / "private-runtime.log"
            print(f"{sample_id} {index}/5 started", flush=True)
            try:
                return_code = process_group(
                    [str(PYTHON), str(Path(__file__).resolve()), "--internal-worker",
                     "--worker-sample", sample_id, "--output-dir", str(sample_dir),
                     "--evaluator", str(evaluator)],
                    cwd=REPO, env=env, timeout=min(args.per_image_timeout, remaining), log_path=log_path,
                )
            except DriverError as error:
                if error.code == "process_timeout":
                    status = "per_image_timeout"
                else:
                    status = "worker_launch_failed"
                write_private_json(sample_dir / "worker-execution.json", {
                    "sample_id": sample_id, "status": "failed", "failure_code": status,
                })
                raise DriverError(f"{status}_{sample_id}") from error
            if return_code != 0:
                write_private_json(sample_dir / "worker-execution.json", {
                    "sample_id": sample_id, "status": "failed", "exit_code": return_code,
                })
                raise DriverError(f"worker_failed_{sample_id}")
            status_path = sample_dir / "worker-status.json"
            status = json.loads(status_path.read_text(encoding="utf-8"))
            if status.get("status") != "completed":
                raise DriverError(f"worker_failed_{sample_id}")
            results.append(status)
            print(f"{sample_id} completed", flush=True)

        stage = "post_run_integrity"
        inputs_after = {sample_id: sha256(path) for sample_id, path in SOURCES if path.is_file()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        code_after = source_code_manifest()
        if inputs_before != inputs_after:
            raise DriverError("input_changed_during_run")
        if models_before != models_after:
            raise DriverError("model_changed_during_run")
        if code_before != code_after:
            raise DriverError("source_code_changed_during_run")
        status = "completed"
        failure_code = None
    except DriverError as error:
        status = "failed"
        failure_code = error.code
        inputs_after = {sample_id: sha256(path) for sample_id, path in SOURCES if path.is_file()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        try:
            code_after = source_code_manifest()
        except Exception:
            code_after = {}
    except Exception:
        status = "failed"
        failure_code = "unexpected_internal_error"
        inputs_after = {sample_id: sha256(path) for sample_id, path in SOURCES if path.is_file()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        try:
            code_after = source_code_manifest()
        except Exception:
            code_after = {}

    manifest = {
        "schema_version": "passport-five-image-inference/v1",
        "label": args.label,
        "status": status,
        "failure_stage": None if status == "completed" else stage,
        "failure_code": failure_code,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "reference_data_read": False,
        "reference_data_used_for_runtime_or_planning": False,
        "go_options": ["--passes=all", "--normalize-names=true", "--mrz-policy=augment", "--template-evidence=optional"],
        "timeout_seconds": {"per_image": args.per_image_timeout, "overall": args.overall_timeout, "go_build": 55, "go_evaluation": 20},
        "toolchain": {"go": go_version, "python": str(PYTHON), "python_packages": python_versions},
        "inputs_sha256": {"before": inputs_before, "after": inputs_after},
        "models_sha256": {"before": models_before, "after": models_after},
        "source_code_sha256": {"before": code_before, "after": code_after},
        "integrity": {
            "inputs_unchanged": inputs_before == inputs_after,
            "models_unchanged": models_before == models_after,
            "source_code_unchanged": code_before == code_after,
        },
        "go_evaluator_sha256": sha256(evaluator) if evaluator.is_file() else None,
        "samples": results,
    }
    write_private_json(manifest_path, manifest)
    if status == "failed":
        raise DriverError(failure_code or "run_failed")
    print("all five images completed; private manifest written", flush=True)


def main() -> int:
    args = parse_args()
    try:
        run(args)
    except DriverError as error:
        print(f"failed status={error.code}", file=sys.stderr, flush=True)
        return 2
    except SystemExit as error:
        return int(error.code or 0)
    except Exception:
        # Never emit arbitrary exception details, which can contain raw PII or paths.
        print("failed status=unexpected_internal_error", file=sys.stderr, flush=True)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
