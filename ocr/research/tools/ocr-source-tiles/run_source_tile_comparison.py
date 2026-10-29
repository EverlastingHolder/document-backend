#!/usr/bin/env python3
"""Private baseline-parity gate and opt-in source-tile comparison.

This runner never reads reference/gold data. It re-evaluates all frozen baseline
observations first and does not launch OCR tiles unless all five Go outputs are
canonical-JSON equal to the frozen extraction artifacts.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
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

_RESEARCH_SUPPORT = _CODE_ROOT / "ocr/research/tools/ocr-five-image-eval"
if str(_RESEARCH_SUPPORT) not in sys.path:
    sys.path.insert(0, str(_RESEARCH_SUPPORT))
import time
import traceback
from typing import Any


WORKSPACE = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
FIVE_IMAGE_WORK = WORKSPACE / "work/ocr-five-image-eval"
BASELINE = FIVE_IMAGE_WORK / "baseline-v2"
BASELINE_AUDIT = FIVE_IMAGE_WORK / "baseline-v2-integrity-audit.json"
BASELINE_AUDIT_SHA256 = "9b3b25aa085b18096294cd05c94f50cf58c5c984d6e69edfcff08afc1363a0a3"
DRIVER_PATH = REPO / "ocr/research/tools/ocr-five-image-eval/run_five_images.py"
PYTHON = Path(os.environ.get("PASSPORT_OCR_PYTHON", sys.executable))
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
MODEL_FILES = (
    RAPIDOCR_MODEL_DIR / "ch_PP-OCRv5_det_mobile.onnx",
    RAPIDOCR_MODEL_DIR / "eslav_PP-OCRv5_rec_mobile.onnx",
)
PINNED_MODEL_HASHES = {
    MODEL_FILES[0].name: "4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae",
    MODEL_FILES[1].name: "08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab",
}
TARGET_SAMPLES = ("sample-02", "sample-04", "sample-05")
ALL_SAMPLES = tuple(f"sample-{index:02d}" for index in range(1, 6))
REPO_PYTHON_FILES = (
    REPO / "ocr/experiments/source_tile_pipeline.py",
    REPO / "ocr/experiments/region_pipeline.py",
    REPO / "ocr/experiments/registration_first_pipeline.py",
    REPO / "ocr/experiments/mrz_pipeline.py",
    REPO / "ocr/experiments/requirements.txt",
    REPO / "ocr/experiments/local_paths.py",
)


class RunError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def normalized_model_hashes(value: Any) -> dict[str, str]:
    """Normalize historical absolute model paths while rejecting basename collisions."""
    if not isinstance(value, dict) or not value:
        raise RunError("baseline_model_hashes_invalid")
    normalized: dict[str, str] = {}
    for path, digest in value.items():
        if not isinstance(path, str) or not path or not isinstance(digest, str):
            raise RunError("baseline_model_hashes_invalid")
        basename = Path(path).name
        if not basename or basename in normalized:
            raise RunError("baseline_model_hash_path_collision")
        if len(digest) != 64 or any(character not in "0123456789abcdef" for character in digest):
            raise RunError("baseline_model_hashes_invalid")
        normalized[basename] = digest
    return normalized


def private_dir(path: Path) -> None:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path, 0o700)
    if path.stat().st_mode & 0o077:
        raise RunError("private_directory_permissions_invalid")


def write_private_json(path: Path, payload: dict[str, Any]) -> None:
    private_dir(path.parent)
    with path.open("x", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, separators=(",", ":"))
        stream.write("\n")


def load_five_image_driver():
    if str(FIVE_IMAGE_WORK) not in sys.path:
        sys.path.insert(0, str(FIVE_IMAGE_WORK))
    import run_five_images
    return run_five_images


def process_group(command: list[str], *, cwd: Path, env: dict[str, str], timeout: float, log_path: Path) -> int:
    """Use the baseline runner's bounded process-group termination behavior."""
    driver = load_five_image_driver()
    try:
        return driver.process_group(command, cwd=cwd, env=env, timeout=timeout, log_path=log_path)
    except driver.DriverError as error:
        if error.code == "process_timeout":
            raise RunError("child_process_timeout") from error
        raise RunError("child_process_reap_failed") from error


def run_capture(command: list[str], *, cwd: Path, env: dict[str, str], timeout: float) -> bytes:
    process = subprocess.Popen(
        command, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True,
    )
    try:
        stdout, _ = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as error:
        import signal
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
        process.wait(timeout=2)
        raise RunError("go_list_timeout") from error
    if process.returncode != 0:
        raise RunError("go_list_failed")
    return stdout


def go_source_closure(env: dict[str, str]) -> dict[str, str]:
    raw = run_capture([str(GO), "list", "-deps", "-json", "./cmd/ocr-eval"], cwd=REPO, env=env, timeout=30)
    decoder = json.JSONDecoder()
    text = raw.decode("utf-8")
    offset = 0
    files: set[Path] = {REPO / "go.mod", REPO / "go.sum"}
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset >= len(text):
            break
        package, offset = decoder.raw_decode(text, offset)
        if package.get("Standard") or not package.get("Dir"):
            continue
        directory = Path(package["Dir"])
        for key in ("GoFiles", "CgoFiles", "EmbedFiles"):
            files.update(directory / name for name in package.get(key, []))
    return {str(path): sha256(path) for path in sorted(files) if path.is_file()}


def python_source_manifest(driver_path: Path) -> dict[str, str]:
    files = set(REPO_PYTHON_FILES)
    files.add(driver_path.resolve())
    files.add(Path(__file__).resolve())
    return {str(path): sha256(path) for path in sorted(files) if path.is_file()}


def python_runtime_metadata() -> dict[str, str]:
    result = {}
    for package in ("rapidocr", "onnxruntime", "numpy", "opencv-python", "Pillow"):
        try:
            result[package] = importlib.metadata.version(package)
        except importlib.metadata.PackageNotFoundError:
            result[package] = "unavailable"
    return result


def baseline_preflight(driver) -> tuple[dict[str, Path], dict[str, str], dict[str, str], dict[str, Any]]:
    manifest_path = BASELINE / "manifest.json"
    if not manifest_path.is_file():
        raise RunError("frozen_baseline_manifest_missing")
    if not BASELINE_AUDIT.is_file() or sha256(BASELINE_AUDIT) != BASELINE_AUDIT_SHA256:
        raise RunError("frozen_baseline_audit_hash_mismatch")
    audit = json.loads(BASELINE_AUDIT.read_text(encoding="utf-8"))
    frozen = json.loads(manifest_path.read_text(encoding="utf-8"))
    if frozen.get("schema_version") != "passport-five-image-inference/v1":
        raise RunError("frozen_baseline_manifest_invalid")
    if (
        audit.get("schema_version") != "passport-five-image-integrity-audit/v1"
        or audit.get("original_manifest_sha256") != sha256(manifest_path)
        or audit.get("original_status") != "failed"
        or audit.get("original_failure_code") != "source_code_changed_during_run"
        or audit.get("accepted_measurement_status") != "dependency_audited"
    ):
        raise RunError("frozen_baseline_audit_case_invalid")
    excluded_api_files = {
        str(REPO / "internal/api/response.go"),
        str(REPO / "internal/api/response_test.go"),
    }
    changed = audit.get("changed_files", [])
    if {item.get("path") for item in changed if isinstance(item, dict)} != excluded_api_files:
        raise RunError("frozen_baseline_audit_changed_files_invalid")
    if audit.get("go_dependency_closure", {}).get("result") != [
        "passport-backend/cmd/ocr-eval",
        "passport-backend/internal/mrz",
        "passport-backend/internal/passport",
    ]:
        raise RunError("frozen_baseline_audit_go_closure_invalid")
    expected_inputs = {sample: digest for sample, digest in audit.get("inputs_sha256", {}).items()}
    if expected_inputs != frozen.get("inputs_sha256", {}).get("before", {}):
        raise RunError("frozen_baseline_audit_inputs_mismatch")
    expected_models = normalized_model_hashes(frozen.get("models_sha256", {}).get("before", {}))
    audited_models = normalized_model_hashes(audit.get("models_sha256", {}))
    if expected_models != audited_models or expected_models != PINNED_MODEL_HASHES:
        raise RunError("frozen_baseline_audit_models_mismatch")
    evaluator = BASELINE / "ocr-eval"
    if not evaluator.is_file() or sha256(evaluator) != audit.get("go_evaluator_sha256"):
        raise RunError("frozen_baseline_evaluator_hash_mismatch")
    expected_artifacts = {
        (sample, filename): digest
        for sample in ALL_SAMPLES
        for filename, digest in (
            ("observations.json", next((item.get("observation_sha256") for item in frozen.get("samples", []) if item.get("sample_id") == sample), None)),
            ("go-extraction.json", next((item.get("extraction_sha256") for item in frozen.get("samples", []) if item.get("sample_id") == sample), None)),
        )
    }
    audited_artifacts = {
        (item.get("sample_id"), Path(item.get("artifact", "")).name): item.get("sha256")
        for item in audit.get("artifacts", []) if isinstance(item, dict)
    }
    if len(audited_artifacts) != len(audit.get("artifacts", [])) or audited_artifacts != expected_artifacts:
        raise RunError("frozen_baseline_audit_artifact_set_invalid")
    frozen_inputs = frozen.get("inputs_sha256", {}).get("before", {})
    frozen_models = frozen.get("models_sha256", {}).get("before", {})
    sources = dict(driver.SOURCES)
    if set(sources) != set(ALL_SAMPLES):
        raise RunError("source_sample_set_mismatch")
    input_hashes = {}
    for sample in ALL_SAMPLES:
        source = sources[sample]
        if not source.is_file() or sample not in frozen_inputs:
            raise RunError("frozen_source_missing")
        digest = sha256(source)
        if digest != frozen_inputs[sample]:
            raise RunError("frozen_source_hash_mismatch")
        input_hashes[sample] = digest
        sample_manifest = next((item for item in frozen.get("samples", []) if item.get("sample_id") == sample), None)
        base_dir = BASELINE / sample
        if not sample_manifest or sample_manifest.get("status") != "completed":
            raise RunError("frozen_sample_artifact_status_invalid")
        for filename, key in (("observations.json", "observation_sha256"), ("go-extraction.json", "extraction_sha256")):
            artifact = base_dir / filename
            if not artifact.is_file() or sha256(artifact) != sample_manifest.get(key):
                raise RunError("frozen_sample_artifact_hash_mismatch")
    model_hashes = {}
    for model in MODEL_FILES:
        expected = PINNED_MODEL_HASHES[model.name]
        if not model.is_file() or sha256(model) != expected or frozen_models.get(str(model)) != expected:
            raise RunError("pinned_model_hash_mismatch")
        model_hashes[str(model)] = expected
    if frozen.get("reference_data_read") is not False or frozen.get("reference_data_used_for_runtime_or_planning") is not False:
        raise RunError("frozen_baseline_gold_boundary_invalid")
    return sources, input_hashes, model_hashes, audit


def run_go_extraction(
    evaluator: Path, observation_path: Path, output_path: Path, *, template_path: Path | None,
    sample_dir: Path, timeout: float, env: dict[str, str],
) -> None:
    command = [
        str(evaluator), "--input", str(observation_path), "--output", str(output_path),
        "--passes=all", "--normalize-names=true", "--mrz-policy=augment",
    ]
    if template_path is not None:
        command.extend(["--template-evidence", str(template_path)])
    rc = process_group(command, cwd=REPO, env=env, timeout=timeout, log_path=sample_dir / "go-private.log")
    if rc != 0 or not output_path.is_file():
        raise RunError("go_extraction_failed")


def baseline_parity_gate(evaluator: Path, output: Path, *, env: dict[str, str], timeout: float) -> None:
    for sample in ALL_SAMPLES:
        sample_dir = output / "baseline-parity" / sample
        private_dir(sample_dir)
        source_dir = BASELINE / sample
        observations = source_dir / "observations.json"
        extraction = source_dir / "go-extraction.json"
        template = source_dir / "ocr-private/template-evidence.json"
        actual = sample_dir / "go-extraction.json"
        run_go_extraction(
            evaluator, observations, actual, template_path=template if template.is_file() else None,
            sample_dir=sample_dir, timeout=min(30, timeout), env=env,
        )
        expected_obj = json.loads(extraction.read_text(encoding="utf-8"))
        actual_obj = json.loads(actual.read_text(encoding="utf-8"))
        if canonical_json(actual_obj) != canonical_json(expected_obj):
            raise RunError("baseline_go_json_parity_failed")


def worker(args: argparse.Namespace) -> int:
    os.umask(0o077)
    if Path(sys.executable).resolve() != PYTHON.resolve():
        raise RunError("expected_pinned_python_interpreter")
    if args.worker_sample not in TARGET_SAMPLES or not args.output_dir or not args.evaluator:
        raise RunError("worker_arguments_invalid")
    driver = load_five_image_driver()
    source_map = dict(driver.SOURCES)
    sample = args.worker_sample
    sample_dir = args.output_dir.expanduser().absolute()
    private_dir(sample_dir)
    baseline_observations_path = BASELINE / sample / "observations.json"
    baseline_observations = json.loads(baseline_observations_path.read_text(encoding="utf-8"))
    if baseline_observations.get("schema_version") != "passport-ocr-observations/v2":
        raise RunError("tile_target_not_source_fallback_v2")

    sys.path.insert(0, str(REPO))
    from ocr.experiments.source_tile_pipeline import augment_source_tiles
    augmented, tile_plan = augment_source_tiles(
        source_map[sample], baseline_observations, MODEL_FILES[0], MODEL_FILES[1],
        private_output_dir=sample_dir,
    )
    observations_path = sample_dir / "observations.json"
    extraction_path = sample_dir / "go-extraction.json"
    write_private_json(observations_path, augmented)
    run_go_extraction(
        args.evaluator, observations_path, extraction_path, template_path=None,
        sample_dir=sample_dir, timeout=args.per_image_timeout,
        env=driver_env(),
    )
    extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
    if extraction.get("image_id") != augmented.get("image", {}).get("id"):
        raise RunError("go_extraction_image_mismatch")
    write_private_json(sample_dir / "worker-status.json", {
        "sample_id": sample,
        "status": "completed",
        "stage": "source_tiles_and_go_extraction",
        "observations_schema": augmented.get("schema_version"),
        "tile_count": len(tile_plan.get("profile", {}).get("tiles", [])),
        "tile_pass_statuses": [p.get("status") for p in tile_plan.get("profile", {}).get("passes", [])],
        "observation_sha256": sha256(observations_path),
        "extraction_sha256": sha256(extraction_path),
    })
    return 0


def driver_env() -> dict[str, str]:
    env = os.environ.copy()
    env.update({"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "PYTHONUNBUFFERED": "1"})
    return env


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", help="Private run label")
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--per-image-timeout", type=int, default=180)
    parser.add_argument("--overall-timeout", type=int, default=900)
    parser.add_argument("--preflight-only", action="store_true", help="Build Go evaluator and verify all five frozen Go outputs; never run OCR")
    parser.add_argument("--preflight-output-dir", type=Path, help="Reuse a successful --preflight-only result without rebuilding or rerunning parity")
    parser.add_argument("--internal-worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--worker-sample", choices=TARGET_SAMPLES, help=argparse.SUPPRESS)
    parser.add_argument("--worker-output", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--evaluator", type=Path, help=argparse.SUPPRESS)
    return parser.parse_args()


def run(args: argparse.Namespace) -> None:
    os.umask(0o077)
    if args.internal_worker:
        if args.worker_output is None:
            raise RunError("worker_arguments_invalid")
        args.output_dir = args.worker_output
        args.evaluator = args.evaluator.resolve()
        raise SystemExit(worker(args))
    if not args.label or not args.output_dir:
        raise RunError("label_and_output_directory_required")
    if args.preflight_only and args.preflight_output_dir:
        raise RunError("preflight_mode_arguments_conflict")
    if Path(sys.executable).resolve() != PYTHON.resolve():
        raise RunError("expected_pinned_python_interpreter")
    if not 1 <= args.per_image_timeout <= 180 or not 1 <= args.overall_timeout <= 900:
        raise RunError("timeout_out_of_bounds")
    output = args.output_dir.expanduser().absolute()
    if output.exists():
        raise RunError("output_directory_already_exists")
    private_dir(output.parent)
    output.mkdir(mode=0o700)
    output.chmod(0o700)

    driver = load_five_image_driver()
    env = driver_env()
    stage = "preflight"
    started = time.monotonic()
    baseline_manifest = json.loads((BASELINE / "manifest.json").read_text(encoding="utf-8"))
    baseline_audit_sha = sha256(BASELINE_AUDIT) if BASELINE_AUDIT.is_file() else None
    baseline_manifest_sha = sha256(BASELINE / "manifest.json") if (BASELINE / "manifest.json").is_file() else None
    input_hashes_before: dict[str, str] = {}
    models_before: dict[str, str] = {}
    go_code_before: dict[str, str] = {}
    py_code_before: dict[str, str] = {}
    evaluator = output / "ocr-eval"
    sample_statuses = []
    status = "failed"
    failure_code = None
    baseline_parity_passed = False
    try:
        if not BASELINE.is_dir() or not GO.is_file():
            raise RunError("baseline_or_toolchain_missing")
        sources, input_hashes_before, models_before, audit = baseline_preflight(driver)
        baseline_audit_sha = sha256(BASELINE_AUDIT)
        baseline_manifest_sha = sha256(BASELINE / "manifest.json")
        go_code_before = go_source_closure(env)
        py_code_before = python_source_manifest(DRIVER_PATH)

        if args.preflight_output_dir:
            stage = "preflight_artifact_validation"
            preflight_dir = args.preflight_output_dir.expanduser().absolute()
            preflight_manifest_path = preflight_dir / "manifest.json"
            if not preflight_manifest_path.is_file():
                raise RunError("preflight_manifest_missing")
            preflight = json.loads(preflight_manifest_path.read_text(encoding="utf-8"))
            evaluator = preflight_dir / "ocr-eval"
            if (
                preflight.get("status") != "preflight_completed"
                or preflight.get("baseline_audit_sha256") != baseline_audit_sha
                or preflight.get("baseline_manifest_sha256") != baseline_manifest_sha
                or preflight.get("input_sha256", {}).get("before") != input_hashes_before
                or preflight.get("model_sha256", {}).get("before") != models_before
                or preflight.get("go_source_sha256", {}).get("before") != go_code_before
                or preflight.get("go_source_sha256", {}).get("after") != go_code_before
                or preflight.get("python_source_sha256", {}).get("before") != py_code_before
                or preflight.get("python_source_sha256", {}).get("after") != py_code_before
                or not evaluator.is_file()
                or sha256(evaluator) != preflight.get("go_evaluator_sha256")
                or preflight.get("baseline_parity_gate") != "passed"
            ):
                raise RunError("preflight_artifact_invalid")
            for sample in ALL_SAMPLES:
                parity_path = preflight_dir / "baseline-parity" / sample / "go-extraction.json"
                expected_hash = preflight.get("baseline_parity_extraction_sha256", {}).get(sample)
                if not parity_path.is_file() or sha256(parity_path) != expected_hash:
                    raise RunError("preflight_parity_artifact_hash_mismatch")
            baseline_parity_passed = True
            baseline_parity_output = preflight_dir / "baseline-parity"
        else:
            stage = "go_build"
            rc = process_group(
                [str(GO), "build", "-o", str(evaluator), "./cmd/ocr-eval"],
                cwd=REPO, env=env, timeout=min(60, args.overall_timeout), log_path=output / "go-build.log",
            )
            if rc != 0 or not evaluator.is_file():
                raise RunError("go_build_failed")
            evaluator.chmod(0o700)

            stage = "baseline_parity_gate"
            baseline_parity_gate(evaluator, output, env=env, timeout=args.per_image_timeout)
            baseline_parity_passed = True
            baseline_parity_output = output / "baseline-parity"

        if args.preflight_only:
            stage = "preflight_post_run_integrity"
        else:
            for sample in ("sample-01", "sample-03"):
                sample_dir = output / sample
                private_dir(sample_dir)
                shutil.copyfile(BASELINE / sample / "observations.json", sample_dir / "observations.json")
                shutil.copyfile(baseline_parity_output / sample / "go-extraction.json", sample_dir / "go-extraction.json")
                os.chmod(sample_dir / "observations.json", 0o600)
                os.chmod(sample_dir / "go-extraction.json", 0o600)
                sample_statuses.append({
                    "sample_id": sample,
                    "status": "completed",
                    "route": "registered_template_baseline_parity_reuse",
                    "observations_sha256": sha256(sample_dir / "observations.json"),
                    "extraction_sha256": sha256(sample_dir / "go-extraction.json"),
                })

            for index, sample in enumerate(TARGET_SAMPLES, start=1):
                stage = f"tile_worker_{sample}"
                remaining = args.overall_timeout - (time.monotonic() - started)
                if remaining <= 0:
                    raise RunError("overall_timeout")
                sample_dir = output / sample
                private_dir(sample_dir)
                print(f"{sample} {index}/{len(TARGET_SAMPLES)} started", flush=True)
                rc = process_group(
                    [str(PYTHON), str(Path(__file__).resolve()), "--internal-worker",
                     "--worker-sample", sample, "--worker-output", str(sample_dir),
                     "--evaluator", str(evaluator)],
                    cwd=REPO, env=env, timeout=min(args.per_image_timeout, remaining),
                    log_path=sample_dir / "private-runtime.log",
                )
                if rc != 0:
                    raise RunError(f"tile_worker_failed_{sample}")
                worker_status = json.loads((sample_dir / "worker-status.json").read_text(encoding="utf-8"))
                if worker_status.get("status") != "completed":
                    raise RunError(f"tile_worker_failed_{sample}")
                sample_statuses.append(worker_status)
                print(f"{sample} completed", flush=True)

        stage = "post_run_integrity"
        inputs_after = {sample: sha256(path) for sample, path in sources.items()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES}
        go_code_after = go_source_closure(env)
        py_code_after = python_source_manifest(DRIVER_PATH)
        if input_hashes_before != inputs_after:
            raise RunError("source_input_changed")
        if models_before != models_after:
            raise RunError("pinned_model_changed")
        if go_code_before != go_code_after or py_code_before != py_code_after:
            raise RunError("participating_source_changed")
        status = "preflight_completed" if args.preflight_only else "completed"
    except RunError as error:
        failure_code = error.code
        inputs_after = {sample: sha256(path) for sample, path in dict(driver.SOURCES).items() if path.is_file()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        go_code_after = go_source_closure(env) if go_code_before else {}
        py_code_after = python_source_manifest(DRIVER_PATH) if py_code_before else {}
    except Exception:
        with (output / "private-traceback.txt").open("w", encoding="utf-8") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(traceback.format_exc())
        failure_code = "unexpected_internal_error"
        inputs_after = {sample: sha256(path) for sample, path in dict(driver.SOURCES).items() if path.is_file()}
        models_after = {str(path): sha256(path) for path in MODEL_FILES if path.is_file()}
        go_code_after = go_source_closure(env) if go_code_before else {}
        py_code_after = python_source_manifest(DRIVER_PATH) if py_code_before else {}

    manifest = {
        "schema_version": "passport-source-tile-experiment/v1",
        "label": args.label,
        "status": status,
        "failure_stage": None if status == "completed" else stage,
        "failure_code": failure_code,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "baseline_manifest_status": baseline_manifest.get("status"),
        "baseline_failure_stage": baseline_manifest.get("failure_stage"),
        "baseline_failure_code": baseline_manifest.get("failure_code"),
        "baseline_audit_sha256": baseline_audit_sha,
        "baseline_manifest_sha256": baseline_manifest_sha,
        "baseline_parity_gate": "passed" if baseline_parity_passed else "not_passed",
        "baseline_parity_extraction_sha256": {
            sample: sha256(baseline_parity_output / sample / "go-extraction.json")
            for sample in ALL_SAMPLES
        } if baseline_parity_passed else {},
        "reference_data_read": False,
        "reference_data_used_for_runtime_or_planning": False,
        "targets": list(TARGET_SAMPLES),
        "artifact_layout": {
            "baseline_parity_extractions": "baseline-parity/sample-*/go-extraction.json",
            "sample_observations": "sample-*/observations.json",
            "sample_extractions": "sample-*/go-extraction.json",
            "source_tile_plans": "sample-02|sample-04|sample-05/source-tile-plan.json",
            "sample_worker_status": "sample-*/worker-status.json",
            "private_logs": "go-build.log and sample-*/private-runtime.log",
            "preflight_only": args.preflight_only,
        },
        "input_sha256": {"before": input_hashes_before, "after": inputs_after},
        "model_sha256": {"before": models_before, "after": models_after},
        "go_source_sha256": {"before": go_code_before, "after": go_code_after},
        "python_source_sha256": {"before": py_code_before, "after": py_code_after},
        "python_runtime": python_runtime_metadata(),
        "go_evaluator_sha256": sha256(evaluator) if evaluator.is_file() else None,
        "samples": sample_statuses,
        "timeouts_seconds": {"per_image": args.per_image_timeout, "overall": args.overall_timeout},
    }
    write_private_json(output / "manifest.json", manifest)
    if status not in {"completed", "preflight_completed"}:
        raise RunError(failure_code or "source_tile_run_failed")
    if args.preflight_only:
        print("baseline parity passed")


def main() -> int:
    try:
        run(parse_args())
        return 0
    except RunError as error:
        print(f"source-tile comparison failed: {error.code}", file=sys.stderr)
        return 2
    except Exception:
        args = parse_args()
        target = args.worker_output if args.internal_worker else args.output_dir
        if target is not None and target.is_dir():
            with (target / "private-traceback.txt").open("w", encoding="utf-8") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(traceback.format_exc())
        print("source-tile comparison failed: unexpected_internal_error", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
