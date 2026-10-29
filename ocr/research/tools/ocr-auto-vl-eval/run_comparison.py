#!/usr/bin/env python3
"""Run a private five-image automatic VL field-crop comparison.

Ground truth is never read here. The runner first re-evaluates all frozen
observations with current Go and requires exact JSON parity with baseline-v2;
only then does it launch the bounded, sequential PaddleOCR-VL readers.
"""
from __future__ import annotations

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
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
import traceback
from typing import Any


TASK = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
WORK = TASK / "work/ocr-auto-vl-eval"
BASELINE = TASK / "work/ocr-five-image-eval/baseline-v2"
BASELINE_MANIFEST = BASELINE / "manifest.json"
BASELINE_AUDIT = TASK / "work/ocr-five-image-eval/baseline-v2-integrity-audit.json"
HISTORICAL_SCORER = REPO / "ocr/research/tools/ocr-source-tiles/score_source_tile_comparison.py"
PYTHON_VL = Path(os.environ.get("PASSPORT_OCR_VL_PYTHON", sys.executable))
MODEL_DIR = VL_MODEL_DIR
SOURCES = {
    "sample-01": TASK / "work/ocr-passport-tests/inputs/image-01.jpg",
    "sample-02": TASK / "work/ocr-passport-tests/inputs/image-02.jpg",
    "sample-03": TASK / "work/ocr-new-examples/inputs/sample03.jpg",
    "sample-04": TASK / "work/ocr-new-examples/inputs/sample04.jpg",
    "sample-05": TASK / "work/ocr-new-examples/inputs/sample05.jpg",
}
FIELDS = ("surname", "given_name", "gender", "birth_date", "birth_place", "series", "number", "issue_date", "issuer", "department_code")
OPTIONAL_FIELDS = ("patronymic",)
SCHEMA = "passport-auto-vl-five-image-run/v1"
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
PER_IMAGE_TIMEOUT = 360
TOTAL_TIMEOUT = 1800
GO_TIMEOUT = 20
TERMINATE_GRACE = 2
KILL_GRACE = 2


class RunError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def baseline_equivalence_json(value: Any) -> str:
    """Ignore ordering only in field evidence line_ids and pass_ids sets."""
    normalized = copy.deepcopy(value)
    fields = normalized.get("fields") if isinstance(normalized, dict) else None
    if isinstance(fields, dict):
        for result in fields.values():
            evidence = result.get("evidence") if isinstance(result, dict) else None
            if not isinstance(evidence, dict):
                continue
            for key in ("line_ids", "pass_ids"):
                items = evidence.get(key)
                if isinstance(items, list) and all(isinstance(item, str) for item in items):
                    evidence[key] = sorted(items)
    return canonical_json(normalized)


def load_json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise RunError(code) from error


def private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)
    if path.stat().st_mode & 0o077:
        raise RunError("private_directory_permissions_invalid")


def write_json(path: Path, value: Any, *, exclusive: bool = True) -> None:
    private_dir(path.parent)
    flags = os.O_WRONLY | os.O_CREAT | (os.O_EXCL if exclusive else os.O_TRUNC)
    descriptor = os.open(path, flags, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(value, stream, ensure_ascii=False, sort_keys=True, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())


def load_historical_scorer():
    spec = importlib.util.spec_from_file_location("source_tile_score_for_auto_vl", HISTORICAL_SCORER)
    if spec is None or spec.loader is None:
        raise RunError("historical_scorer_unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def verify_historical_baseline() -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
    try:
        module = load_historical_scorer()
        return module.verify_historical_baseline()
    except Exception as error:
        raise RunError("historical_baseline_integrity_invalid") from error


def go_source_manifest(env: dict[str, str]) -> dict[str, str]:
    try:
        result = subprocess.run(
            [str(GO), "list", "-deps", "-json", "./cmd/ocr-eval"], cwd=REPO,
            env=env, text=True, capture_output=True, timeout=30, check=True,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise RunError("go_dependency_closure_failed") from error
    decoder = json.JSONDecoder()
    offset = 0
    packages: set[str] = set()
    files = {REPO / "go.mod", REPO / "go.sum"}
    while offset < len(result.stdout):
        while offset < len(result.stdout) and result.stdout[offset].isspace():
            offset += 1
        if offset >= len(result.stdout):
            break
        package, offset = decoder.raw_decode(result.stdout, offset)
        if package.get("Standard") or not package.get("Dir"):
            continue
        packages.add(package.get("ImportPath"))
        directory = Path(package["Dir"])
        for key in ("GoFiles", "CgoFiles", "EmbedFiles"):
            files.update(directory / name for name in package.get(key, []))
    expected = {"passport-backend/cmd/ocr-eval", "passport-backend/internal/mrz", "passport-backend/internal/passport"}
    if packages != expected:
        raise RunError("go_dependency_closure_unexpected")
    return {str(path): sha256(path) for path in sorted(files) if path.is_file()}


def python_source_manifest() -> dict[str, str]:
    paths = {
        Path(__file__).resolve(),
        HISTORICAL_SCORER,
        REPO / "ocr/experiments/auto_vl_pipeline.py",
        REPO / "ocr/experiments/local_paths.py",
    }
    if any(not path.is_file() for path in paths):
        raise RunError("python_runtime_source_missing")
    return {str(path): sha256(path) for path in sorted(paths)}


def private_tree_manifest(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for current, directories, filenames in os.walk(root, followlinks=False):
        current_path = Path(current)
        directories.sort()
        filenames.sort()
        if any((current_path / name).is_symlink() for name in directories):
            raise RunError("reader_artifact_symlink_rejected")
        for name in filenames:
            path = current_path / name
            if path.is_symlink() or not path.is_file():
                raise RunError("reader_artifact_nonregular_file_rejected")
            result[path.relative_to(root.parent).as_posix()] = sha256(path)
    return result


def environment() -> dict[str, str]:
    env = os.environ.copy()
    env.update({
        "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
        "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
        "PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK": "True",
    })
    return env


def _stop_group(process: subprocess.Popen[Any]) -> None:
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=TERMINATE_GRACE)
    except subprocess.TimeoutExpired:
        pass
    # The direct child may have exited on SIGTERM while Paddle descendants in
    # the same process group ignored it. Always signal the group before return.
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=KILL_GRACE)
    except subprocess.TimeoutExpired as error:
        raise RunError("child_process_reap_timeout") from error


def run_logged(command: list[str], *, cwd: Path, env: dict[str, str], timeout: float, log_path: Path) -> None:
    private_dir(log_path.parent)
    with log_path.open("ab") as log:
        os.fchmod(log.fileno(), 0o600)
        process = subprocess.Popen(
            command, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
            stdout=log, stderr=log, start_new_session=True,
        )
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            _stop_group(process)
            raise RunError("child_process_timeout") from error
    if code != 0:
        raise RunError("child_process_failed")


def _go_command(evaluator: Path, input_path: Path, output_path: Path, *, template_path: Path | None = None,
                plan_path: Path | None = None, evidence_path: Path | None = None,
                model_sha: str | None = None, policy: str = "union") -> list[str]:
    command = [
        str(evaluator), "--input", str(input_path), "--output", str(output_path),
        "--passes=all", "--normalize-names=true", "--mrz-policy=augment",
    ]
    if template_path is not None:
        command += ["--template-evidence", str(template_path)]
    if plan_path is not None:
        command += ["--vl-plan-output", str(plan_path)]
    if evidence_path is not None:
        if not model_sha:
            raise RunError("vl_model_identity_missing")
        command += ["--vl-evidence", str(evidence_path), "--vl-model-sha256", model_sha, "--vl-policy", policy]
    return command


def _canonical_image_path(sample_id: str, plan: dict[str, Any]) -> Path:
    image = plan.get("image")
    if not isinstance(image, dict) or not isinstance(image.get("sha256"), str):
        raise RunError("vl_plan_image_invalid")
    source = SOURCES[sample_id]
    if sha256(source) == image["sha256"]:
        return source
    canonical = BASELINE / sample_id / "ocr-private/canonical-spread.png"
    if canonical.is_file() and sha256(canonical) == image["sha256"]:
        return canonical
    raise RunError("vl_plan_image_binding_invalid")


def worker(args: argparse.Namespace) -> int:
    os.umask(0o077)
    sample_dir = args.output_dir.expanduser().absolute()
    private_dir(sample_dir)
    started = time.monotonic()
    stage = "worker_import"
    safe_failure_type: type[BaseException] | None = None
    try:
        if Path(sys.executable).resolve() != PYTHON_VL.resolve():
            raise RunError("pinned_vl_python_interpreter_mismatch")
        sys.path.insert(0, str(REPO))
        from ocr.experiments.auto_vl_pipeline import AutoVLFailure, model_tree_snapshot, read_auto_vl_fields
        safe_failure_type = AutoVLFailure

        if args.identity_only:
            stage = "model_identity"
            identity = model_tree_snapshot(args.model_dir.expanduser().resolve())
            write_json(sample_dir / "worker-status.json", {
                "sample_id": "preflight", "status": "completed", "stage": "model_identity",
                "model_tree_sha256": identity["tree_sha256"], "file_count": identity["file_count"],
            })
            return 0

        stage = "vl_reader"
        plan = load_json(args.plan.expanduser().resolve(), "vl_plan_invalid")
        evidence = read_auto_vl_fields(
            args.image.expanduser().resolve(), plan,
            private_output_dir=sample_dir / "reader-private", model_dir=args.model_dir.expanduser().resolve(),
        )
        evidence_path = sample_dir / "reader-private/evidence.json"
        if not evidence_path.is_file():
            raise RunError("vl_evidence_missing")
        saved_evidence = load_json(evidence_path, "vl_evidence_invalid")
        if canonical_json(saved_evidence) != canonical_json(evidence):
            raise RunError("vl_evidence_readback_mismatch")
        status = {
            "sample_id": args.worker_sample, "status": "completed", "stage": "complete",
            "evidence_sha256": sha256(evidence_path), "model_tree_sha256": evidence["reader"]["model_tree_sha256"],
            "planned_region_count": len(plan["regions"]),
            "reader_region_count": len(evidence["regions"]),
            "elapsed_seconds": round(time.monotonic() - started, 3),
        }
        write_json(sample_dir / "worker-status.json", status)
        return 0
    except RunError as error:
        failure = error.code
    except Exception as error:
        failure = error.code if safe_failure_type is not None and isinstance(error, safe_failure_type) else "unexpected_worker_error"
        write_json(sample_dir / "private-traceback.json", {
            "stage": stage, "exception_type": type(error).__name__, "traceback": traceback.format_exc(),
        })
    write_json(sample_dir / "worker-status.json", {
        "sample_id": args.worker_sample, "status": "failed", "stage": stage,
        "failure_code": failure, "elapsed_seconds": round(time.monotonic() - started, 3),
    })
    return 2


def _source_rows(historical_manifest: dict[str, Any]) -> dict[str, dict[str, Any]]:
    rows = historical_manifest.get("samples")
    if not isinstance(rows, list) or len(rows) != 5:
        raise RunError("historical_sample_set_invalid")
    mapping = {row.get("sample_id"): row for row in rows if isinstance(row, dict)}
    if set(mapping) != set(SOURCES):
        raise RunError("historical_sample_set_invalid")
    return mapping


def _create_empty_manifest(output: Path, label: str) -> Path:
    path = output / "manifest.json"
    write_json(path, {
        "schema_version": SCHEMA, "label": label, "status": "running",
        "failure_stage": None, "failure_code": None,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "reference_data_read": False, "reference_data_used_for_runtime_or_planning": False,
        "samples": [],
    })
    return path


def _final_manifest(path: Path, manifest: dict[str, Any]) -> None:
    temporary = path.with_suffix(".tmp")
    write_json(temporary, manifest)
    temporary.replace(path)
    path.chmod(0o600)


def run(args: argparse.Namespace) -> None:
    os.umask(0o077)
    if args.internal_worker:
        if args.identity_only:
            if not args.output_dir:
                raise RunError("worker_arguments_invalid")
        elif not all((args.worker_sample, args.output_dir, args.image, args.plan)):
            raise RunError("worker_arguments_invalid")
        raise SystemExit(worker(args))
    if not args.output_dir or not args.evaluator:
        raise RunError("output_dir_and_evaluator_required")
    if args.per_image_timeout < 1 or args.per_image_timeout > PER_IMAGE_TIMEOUT:
        raise RunError("per_image_timeout_out_of_bounds")
    if args.overall_timeout < 1 or args.overall_timeout > TOTAL_TIMEOUT:
        raise RunError("overall_timeout_out_of_bounds")
    if not PYTHON_VL.is_file() or not MODEL_DIR.is_dir():
        raise RunError("pinned_vl_runtime_or_model_missing")
    output = args.output_dir.expanduser().absolute()
    if output.exists():
        raise RunError("output_directory_already_exists")
    private_dir(output.parent)
    output.mkdir(mode=0o700)
    output.chmod(0o700)
    private_dir(output)
    manifest_path = _create_empty_manifest(output, args.label or "auto-vl-comparison")
    manifest: dict[str, Any] = {
        "schema_version": SCHEMA, "label": args.label or "auto-vl-comparison", "status": "running",
        "failure_stage": None, "failure_code": None,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "reference_data_read": False, "reference_data_used_for_runtime_or_planning": False,
        "baseline_manifest_sha256": None, "baseline_audit_sha256": None,
        "evaluator_path": None,
        "inputs_sha256": {"before": {}, "after": {}},
        "ocr_images_sha256": {"before": {}, "after": {}},
        "go_source_sha256": {"before": {}, "after": {}},
        "python_source_sha256": {"before": {}, "after": {}},
        "evaluator_sha256": {"before": None, "after": None},
        "model_tree_sha256": {"by_sample": {}, "unchanged": None},
        "timeout_seconds": {"per_image": args.per_image_timeout, "overall": args.overall_timeout, "go_evaluation": GO_TIMEOUT},
        "go_options": ["--passes=all", "--normalize-names=true", "--mrz-policy=augment", "--template-evidence=optional"],
        "samples": [],
    }
    started_all = time.monotonic()
    stage = "historical_baseline_preflight"
    try:
        historical_manifest, frozen = verify_historical_baseline()
        manifest["baseline_manifest_sha256"] = sha256(BASELINE_MANIFEST)
        manifest["baseline_audit_sha256"] = sha256(BASELINE_AUDIT)
        historical_rows = _source_rows(historical_manifest)
        input_before = {sample: sha256(path) for sample, path in SOURCES.items()}
        if input_before != historical_manifest["inputs_sha256"]["before"]:
            raise RunError("frozen_input_hash_mismatch")
        manifest["inputs_sha256"]["before"] = input_before

        env = environment()
        stage = "code_preflight"
        go_before = go_source_manifest(env)
        python_before = python_source_manifest()
        manifest["go_source_sha256"]["before"] = go_before
        manifest["python_source_sha256"]["before"] = python_before
        evaluator = args.evaluator.expanduser().resolve()
        if not evaluator.is_file() or not os.access(evaluator, os.X_OK):
            raise RunError("go_evaluator_not_executable")
        evaluator_before = sha256(evaluator)
        manifest["evaluator_path"] = str(evaluator)
        manifest["evaluator_sha256"]["before"] = evaluator_before

        # Exact current-Go parity must pass for all five images before a VL
        # inference child is launched. Baseline-v2 remains immutable.
        stage = "baseline_parity"
        samples: list[dict[str, Any]] = []
        for index, sample_id in enumerate(SOURCES, start=1):
            if time.monotonic() - started_all >= args.overall_timeout:
                raise RunError("overall_timeout_before_vl")
            frozen_row = historical_rows[sample_id]
            sample_dir = output / sample_id
            private_dir(sample_dir)
            old_dir = BASELINE / sample_id
            observations = old_dir / "observations.json"
            template = old_dir / "ocr-private/template-evidence.json"
            if not observations.is_file():
                raise RunError("baseline_observations_missing")
            template_path = template if template.is_file() else None
            current_extraction = sample_dir / "baseline-current.json"
            plan_path = sample_dir / "vl-plan.json"
            go_log = sample_dir / "baseline-go.log"
            run_logged(
                _go_command(evaluator, observations, current_extraction, template_path=template_path, plan_path=plan_path),
                cwd=REPO, env=env, timeout=GO_TIMEOUT, log_path=go_log,
            )
            frozen_extraction = old_dir / "go-extraction.json"
            if baseline_equivalence_json(load_json(current_extraction, "current_extraction_invalid")) != baseline_equivalence_json(
                load_json(frozen_extraction, "frozen_extraction_invalid")
            ):
                raise RunError(f"baseline_parity_failed_{sample_id}")
            plan = load_json(plan_path, "vl_plan_invalid")
            if plan.get("schema_version") != "passport-vl-field-plan/v1":
                raise RunError("vl_plan_schema_invalid")
            image_path = _canonical_image_path(sample_id, plan)
            image_sha = sha256(image_path)
            if image_sha != plan["image"]["sha256"]:
                raise RunError("vl_plan_image_changed_before_reader")
            manifest["ocr_images_sha256"]["before"][sample_id] = image_sha
            plan_bytes_sha = sha256(plan_path)
            sample = {
                "sample_id": sample_id, "status": "parity_passed",
                "source_sha256": input_before[sample_id],
                "ocr_image_path_kind": "registered_canonical" if image_path.name == "canonical-spread.png" else "source_exif_normalized",
                "ocr_image_sha256": plan["image"]["sha256"],
                "frozen_observations_sha256": sha256(observations),
                "frozen_extraction_sha256": sha256(frozen_extraction),
                "template_evidence_sha256": sha256(template_path) if template_path else None,
                "current_extraction_sha256": sha256(current_extraction),
                "plan_sha256_file": plan_bytes_sha,
                "plan_content_sha256": plan.get("plan_sha256"),
                "plan_region_count": len(plan.get("regions", [])),
                "plan_skipped_field_count": len(plan.get("skipped_fields", [])),
                "vl_image_path": str(image_path),
                "artifacts": {"baseline-current.json": sha256(current_extraction), "vl-plan.json": plan_bytes_sha},
            }
            samples.append(sample)
            print(f"{sample_id} baseline parity passed ({index}/5)", flush=True)
        manifest["samples"] = samples
        manifest["baseline_equivalence"] = "exact_except_field_evidence_set_order"

        model_hashes: dict[str, str] = {}
        preflight_model_sha: str | None = None
        if args.preflight_only:
            stage = "model_identity_preflight"
            identity_dir = output / "preflight"
            private_dir(identity_dir)
            run_logged(
                [str(PYTHON_VL), str(Path(__file__).resolve()), "--internal-worker", "--identity-only",
                 "--output-dir", str(identity_dir), "--model-dir", str(MODEL_DIR)],
                cwd=REPO, env=env, timeout=min(60, max(1, args.overall_timeout - (time.monotonic() - started_all))),
                log_path=identity_dir / "model-identity.log",
            )
            identity = load_json(identity_dir / "worker-status.json", "model_identity_status_invalid")
            if identity.get("status") != "completed" or identity.get("stage") != "model_identity":
                raise RunError("model_identity_preflight_failed")
            preflight_model_sha = identity.get("model_tree_sha256")
            if not isinstance(preflight_model_sha, str) or len(preflight_model_sha) != 64:
                raise RunError("model_identity_preflight_invalid")
            manifest["model_identity_preflight"] = {
                "model_tree_sha256": preflight_model_sha, "file_count": identity.get("file_count"),
            }
            planned = sum(int(sample.get("plan_region_count", 0)) for sample in samples)
            print(f"preflight passed; planned_regions={planned}; no VL predictions run", flush=True)
        else:
            stage = "vl_readers"
            manifest["status"] = "vl_running"
            _final_manifest(manifest_path, manifest)
            for index, sample in enumerate(samples, start=1):
                remaining = args.overall_timeout - (time.monotonic() - started_all)
                if remaining <= 0:
                    raise RunError("overall_timeout")
                sample_id = sample["sample_id"]
                sample_dir = output / sample_id
                image_path = Path(sample["vl_image_path"])
                log_path = sample_dir / "vl-worker.log"
                worker_command = [
                    str(PYTHON_VL), str(Path(__file__).resolve()), "--internal-worker",
                    "--worker-sample", sample_id, "--output-dir", str(sample_dir),
                    "--image", str(image_path), "--plan", str(sample_dir / "vl-plan.json"),
                    "--model-dir", str(MODEL_DIR),
                ]
                print(f"{sample_id} VL reader started ({index}/5)", flush=True)
                run_logged(worker_command, cwd=REPO, env=env, timeout=min(args.per_image_timeout, remaining), log_path=log_path)
                worker_status = load_json(sample_dir / "worker-status.json", "vl_worker_status_invalid")
                if worker_status.get("status") != "completed" or worker_status.get("sample_id") != sample_id:
                    raise RunError(f"vl_worker_incomplete_{sample_id}")
                model_sha = worker_status.get("model_tree_sha256")
                if not isinstance(model_sha, str) or len(model_sha) != 64:
                    raise RunError("vl_model_identity_invalid")
                model_hashes[sample_id] = model_sha
                sample["reader_artifacts_sha256"] = private_tree_manifest(sample_dir / "reader-private")
                evidence_path = sample_dir / "reader-private/evidence.json"
                for policy in ("union", "primary"):
                    arm_path = sample_dir / f"{policy}-extraction.json"
                    arm_log = sample_dir / f"go-{policy}.log"
                    run_logged(
                        _go_command(evaluator, BASELINE / sample_id / "observations.json", arm_path,
                                    template_path=(BASELINE / sample_id / "ocr-private/template-evidence.json")
                                    if (BASELINE / sample_id / "ocr-private/template-evidence.json").is_file() else None,
                                    evidence_path=evidence_path, model_sha=model_sha, policy=policy),
                        cwd=REPO, env=env, timeout=GO_TIMEOUT, log_path=arm_log,
                    )
                sample["status"] = "completed"
                sample["vl_model_sha256"] = model_sha
                sample["worker_elapsed_seconds"] = worker_status.get("elapsed_seconds")
                sample["reader_region_count"] = worker_status.get("reader_region_count")
                sample["artifacts"].update({
                    "reader-private/evidence.json": sha256(evidence_path),
                    "worker-status.json": sha256(sample_dir / "worker-status.json"),
                    "union-extraction.json": sha256(sample_dir / "union-extraction.json"),
                    "primary-extraction.json": sha256(sample_dir / "primary-extraction.json"),
                })
                print(f"{sample_id} completed ({index}/5)", flush=True)

        stage = "post_run_integrity"
        input_after = {sample: sha256(path) for sample, path in SOURCES.items()}
        image_after: dict[str, str] = {}
        for sample in samples:
            image_path = Path(sample["vl_image_path"])
            if not image_path.is_file():
                raise RunError("vl_image_missing_after_run")
            image_after[sample["sample_id"]] = sha256(image_path)
        go_after = go_source_manifest(env)
        python_after = python_source_manifest()
        evaluator_after = sha256(evaluator)
        manifest["inputs_sha256"]["after"] = input_after
        manifest["ocr_images_sha256"]["after"] = image_after
        manifest["go_source_sha256"]["after"] = go_after
        manifest["python_source_sha256"]["after"] = python_after
        manifest["evaluator_sha256"]["after"] = evaluator_after
        manifest["model_tree_sha256"] = {
            "by_sample": model_hashes, "unchanged": len(set(model_hashes.values())) == 1 if model_hashes else True,
        }
        if input_before != input_after:
            raise RunError("input_changed_during_run")
        if manifest["ocr_images_sha256"]["before"] != image_after:
            raise RunError("ocr_image_changed_during_run")
        if go_before != go_after:
            raise RunError("go_runtime_source_changed_during_run")
        if python_before != python_after:
            raise RunError("python_runtime_source_changed_during_run")
        if evaluator_before != evaluator_after:
            raise RunError("go_evaluator_changed_during_run")
        if not manifest["model_tree_sha256"]["unchanged"]:
            raise RunError("vl_model_tree_changed_between_images")
        manifest["status"] = "preflight_completed" if args.preflight_only else "completed"
        manifest["failure_stage"] = None
        manifest["failure_code"] = None
    except RunError as error:
        manifest["status"] = "failed"
        manifest["failure_stage"] = stage
        manifest["failure_code"] = error.code
        if not manifest["inputs_sha256"]["after"]:
            manifest["inputs_sha256"]["after"] = {sample: sha256(path) for sample, path in SOURCES.items() if path.is_file()}
    except Exception as error:
        manifest["status"] = "failed"
        manifest["failure_stage"] = stage
        manifest["failure_code"] = "unexpected_internal_error"
        if not manifest["inputs_sha256"]["after"]:
            manifest["inputs_sha256"]["after"] = {sample: sha256(path) for sample, path in SOURCES.items() if path.is_file()}
        write_json(output / "private-traceback.json", {
            "stage": stage, "exception_type": type(error).__name__, "traceback": traceback.format_exc(),
        })
    _final_manifest(manifest_path, manifest)
    if manifest["status"] not in {"completed", "preflight_completed"}:
        raise RunError(str(manifest.get("failure_code") or "run_failed"))
    if manifest["status"] == "completed":
        print("all five images completed; private manifest written", flush=True)
    else:
        print("preflight completed; private manifest written", flush=True)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", default="auto-vl-five-image-comparison")
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--evaluator", type=Path, help="new Go ocr-eval binary; runner does not build it")
    parser.add_argument("--per-image-timeout", type=int, default=PER_IMAGE_TIMEOUT)
    parser.add_argument("--overall-timeout", type=int, default=TOTAL_TIMEOUT)
    parser.add_argument("--preflight-only", action="store_true", help="validate five-image baseline parity and model identity without VL predictions")
    parser.add_argument("--internal-worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--identity-only", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--worker-sample", choices=tuple(SOURCES), help=argparse.SUPPRESS)
    parser.add_argument("--image", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--plan", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--model-dir", type=Path, default=MODEL_DIR, help=argparse.SUPPRESS)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        run(args)
        return 0
    except RunError as error:
        print(f"failed status={error.code}", file=sys.stderr, flush=True)
        return 2
    except SystemExit as error:
        return int(error.code or 0)
    except Exception:
        print("failed status=unexpected_internal_error", file=sys.stderr, flush=True)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
