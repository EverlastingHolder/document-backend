#!/usr/bin/env python3
"""Run the private automatic OCR -> Go plan -> VL -> Go review pipeline.

This is a local experiment entrypoint. It accepts one image and local model and
evaluator paths, writes all OCR-derived content to a new private directory, and
does not accept reference values or manual crop coordinates.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import traceback
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import VL_MODEL_DIR  # noqa: E402


DEFAULT_PYTHON_OCR = ROOT / ".venv" / "bin" / "python"
DEFAULT_PYTHON_VL = ROOT / ".venv-vl" / "bin" / "python"
DEFAULT_VL_MODEL_DIR = VL_MODEL_DIR
MAX_JOB_SECONDS = 360.0
TERMINATE_GRACE_SECONDS = 2.0
KILL_GRACE_SECONDS = 2.0
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_SAFE_IMAGE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


class PipelineFailure(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _model_tree_snapshot(model_dir: Path) -> dict[str, Any]:
    """Use the reader's model-tree fingerprint without importing Paddle."""
    if not model_dir.is_dir() or model_dir.is_symlink():
        raise PipelineFailure("vl_model_directory_missing")
    inference = model_dir / "inference.yml"
    if not inference.is_file() or not (model_dir / "config.json").is_file():
        raise PipelineFailure("vl_model_metadata_missing")
    try:
        if "model_name: PaddleOCR-VL-1.6-0.9B" not in inference.read_text(encoding="utf-8"):
            raise PipelineFailure("vl_model_name_mismatch")
    except OSError as error:
        raise PipelineFailure("vl_model_metadata_unreadable") from error
    files: dict[str, str] = {}
    for current, directories, filenames in os.walk(model_dir, followlinks=False):
        root = Path(current)
        directories.sort()
        filenames.sort()
        if any((root / name).is_symlink() for name in directories):
            raise PipelineFailure("vl_model_symlink_rejected")
        for name in filenames:
            path = root / name
            if path.is_symlink() or not path.is_file():
                raise PipelineFailure("vl_model_nonregular_file_rejected")
            files[path.relative_to(model_dir).as_posix()] = _sha256_file(path)
    if not files or not any(name.endswith((".safetensors", ".pdparams", ".bin")) for name in files):
        raise PipelineFailure("vl_model_weights_missing")
    digest = hashlib.sha256()
    for name, value in sorted(files.items()):
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(value.encode("ascii"))
        digest.update(b"\n")
    return {"tree_sha256": digest.hexdigest(), "files_sha256": files}


def _private_dir(path: Path, *, create: bool = True) -> None:
    if create:
        path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if not path.is_dir() or path.is_symlink():
        raise PipelineFailure("private_directory_invalid")
    path.chmod(0o700)


def _require_private_file(path: Path, code: str) -> None:
    if not path.is_file() or path.is_symlink():
        raise PipelineFailure(code)
    if path.stat().st_mode & 0o077:
        raise PipelineFailure("private_file_permissions_invalid")


def _write_private_json(path: Path, value: Any) -> None:
    _private_dir(path.parent)
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        os.chmod(temporary, 0o600)
        json.dump(value, stream, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    path.chmod(0o600)


def _read_json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise PipelineFailure(code) from error


def _safe_child_env(*, go: bool = False) -> dict[str, str]:
    allowed = ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL")
    env = {key: os.environ[key] for key in allowed if key in os.environ}
    env.update({
        "HF_HUB_OFFLINE": "1",
        "TRANSFORMERS_OFFLINE": "1",
        "PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK": "True",
        "PYTHONUNBUFFERED": "1",
        "TOKENIZERS_PARALLELISM": "false",
    })
    if go:
        env.update({"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})
    return env


def _kill_process_group(process: subprocess.Popen[bytes]) -> None:
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=TERMINATE_GRACE_SECONDS)
    except subprocess.TimeoutExpired:
        pass
    # The direct child can exit after SIGTERM while an OCR/model grandchild in
    # its process group survives. Always issue SIGKILL to the group after the
    # grace period, even when wait() reports that the direct child is done.
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=KILL_GRACE_SECONDS)
    except subprocess.TimeoutExpired as error:
        raise PipelineFailure("child_did_not_exit_after_kill") from error


def _run_process(
    argv: list[str], *, cwd: Path, env: dict[str, str], stdout_path: Path,
    stderr_path: Path, timeout: float, stage: str,
) -> float:
    if timeout <= 0:
        raise PipelineFailure("total_timeout")
    _private_dir(stdout_path.parent)
    started = time.monotonic()
    with stdout_path.open("wb") as stdout_file, stderr_path.open("wb") as stderr_file:
        os.chmod(stdout_path, 0o600)
        os.chmod(stderr_path, 0o600)
        try:
            process = subprocess.Popen(
                argv, cwd=cwd, env=env, stdout=stdout_file, stderr=stderr_file,
                start_new_session=True,
            )
        except OSError as error:
            raise PipelineFailure(f"{stage}_start_failed") from error
        try:
            return_code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            _kill_process_group(process)
            raise PipelineFailure(f"{stage}_timeout") from error
    elapsed = time.monotonic() - started
    if return_code != 0:
        raise PipelineFailure(f"{stage}_failed")
    return elapsed


def _source_pins() -> dict[str, str]:
    files = (
        Path(__file__).resolve(),
        ROOT / "ocr/experiments/template_field_pipeline.py",
        ROOT / "ocr/experiments/registration_first_pipeline.py",
        ROOT / "ocr/experiments/region_pipeline.py",
        ROOT / "ocr/experiments/field_roi_pipeline.py",
        ROOT / "ocr/experiments/mrz_pipeline.py",
        ROOT / "ocr/experiments/page_registration.py",
        ROOT / "ocr/experiments/auto_vl_pipeline.py",
    )
    try:
        return {path.relative_to(ROOT).as_posix(): _sha256_file(path) for path in files}
    except OSError as error:
        raise PipelineFailure("pipeline_source_missing") from error


def _require_file(path: Path, *, executable: bool = False, code: str) -> Path:
    candidate = path.expanduser()
    if candidate.is_symlink() and not executable:
        raise PipelineFailure(code)
    resolved = candidate.resolve()
    if not resolved.is_file():
        raise PipelineFailure(code)
    if executable and not os.access(resolved, os.X_OK):
        raise PipelineFailure(code)
    # Keep Python venv launchers such as .venv/bin/python intact. Replacing
    # that symlink with its interpreter target makes Python miss pyvenv.cfg.
    return candidate.absolute() if executable else resolved


def _within(path: Path, parent: Path) -> bool:
    try:
        path.resolve().relative_to(parent.resolve())
    except ValueError:
        return False
    return True


def _child_argv(args: argparse.Namespace, stage: str, private_dir: Path, *, raster: Path | None = None, plan: Path | None = None) -> list[str]:
    common = [
        "--image-id", args.image_id,
        "--output-dir", str(args.output_dir), "--evaluator", str(args.evaluator),
        "--det-model", str(args.det_model), "--rec-model", str(args.rec_model),
        "--vl-model-dir", str(args.vl_model_dir), "--vl-policy", args.vl_policy,
        "--python-ocr", str(args.python_ocr), "--python-vl", str(args.python_vl),
    ]
    if stage == "ocr":
        return [
            str(args.python_ocr), str(Path(__file__).resolve()), "--stage", "ocr",
            "--image", str(args.image),
            "--stage-dir", str(private_dir), *common,
        ]
    if stage == "vl":
        assert raster is not None and plan is not None
        return [
            str(args.python_vl), str(Path(__file__).resolve()), "--stage", "vl",
            "--image", str(raster), "--plan-file", str(plan),
            "--stage-dir", str(private_dir), *common,
        ]
    raise PipelineFailure("unknown_child_stage")


def _evaluator_argv(args: argparse.Namespace, *, observations: Path, template: Path | None,
                    output: Path, plan_output: Path | None = None, vl_evidence: Path | None = None,
                    model_sha: str | None = None) -> list[str]:
    argv = [
        str(args.evaluator), "--input", str(observations), "--output", str(output),
        "--passes=all", "--normalize-names=true", "--mrz-policy=augment",
        f"--vl-policy={args.vl_policy}",
    ]
    if template is not None:
        argv.extend(["--template-evidence", str(template)])
    if plan_output is not None:
        argv.extend(["--vl-plan-output", str(plan_output)])
    if vl_evidence is not None:
        if model_sha is None:
            raise PipelineFailure("vl_model_sha_missing")
        argv.extend(["--vl-evidence", str(vl_evidence), "--vl-model-sha256", model_sha])
    return argv


def _run_python_stage(runner: Callable[..., float], argv: list[str], *, stage: str,
                      stage_dir: Path, stdout_path: Path, stderr_path: Path,
                      timeout: float) -> float:
    try:
        return runner(
            argv, cwd=ROOT, env=_safe_child_env(), stdout_path=stdout_path,
            stderr_path=stderr_path, timeout=timeout, stage=stage,
        )
    except PipelineFailure as error:
        status_path = stage_dir / "stage-status.json"
        if status_path.is_file():
            status = _read_json(status_path, f"{stage}_status_invalid")
            code = status.get("error_code") if isinstance(status, dict) else None
            if isinstance(code, str):
                raise PipelineFailure(code) from error
        raise


def run_pipeline(args: argparse.Namespace, *, process_runner: Callable[..., float] | None = None) -> dict[str, Any]:
    """Execute one automatic image route; `process_runner` is a test seam."""
    deadline = time.monotonic() + MAX_JOB_SECONDS
    runner = process_runner or _run_process
    if args.vl_policy not in {"primary", "union"}:
        raise PipelineFailure("vl_policy_invalid")
    image = _require_file(args.image, code="source_image_missing")
    evaluator = _require_file(args.evaluator, executable=True, code="evaluator_missing")
    det_model = _require_file(args.det_model, code="detector_model_missing")
    rec_model = _require_file(args.rec_model, code="recognizer_model_missing")
    python_ocr = _require_file(args.python_ocr, executable=True, code="ocr_python_missing")
    python_vl = _require_file(args.python_vl, executable=True, code="vl_python_missing")
    args.image, args.evaluator, args.det_model, args.rec_model = image, evaluator, det_model, rec_model
    args.python_ocr, args.python_vl = python_ocr, python_vl
    args.output_dir = args.output_dir.expanduser().absolute()
    args.vl_model_dir = args.vl_model_dir.expanduser().absolute()
    model_dir = args.vl_model_dir
    if not isinstance(args.image_id, str) or not _SAFE_IMAGE_ID.fullmatch(args.image_id):
        raise PipelineFailure("image_id_invalid")
    output = args.output_dir.expanduser().resolve()
    if output.exists() or output.is_symlink():
        raise PipelineFailure("output_directory_must_be_new")
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    _private_dir(output)

    pins_before = _source_pins()
    input_pins = {
        "image": _sha256_file(image), "detector": _sha256_file(det_model),
        "recognizer": _sha256_file(rec_model), "evaluator": _sha256_file(evaluator),
        "python_ocr": _sha256_file(python_ocr), "python_vl": _sha256_file(python_vl),
    }
    source_pins_before = {"pipeline": pins_before, "inputs": input_pins}
    manifest: dict[str, Any] = {
        "schema_version": "passport-auto-vl-run/v1", "status": "running",
        "policy": args.vl_policy,
        "source_pins_before": source_pins_before,
        "phases": {"ocr": {"status": "pending"}, "baseline_and_plan": {"status": "pending"},
                   "vl": {"status": "pending"}, "final_extraction": {"status": "pending"}},
        "artifact_sha256": {},
    }
    manifest_path = output / "run-manifest.json"
    _write_private_json(manifest_path, manifest)
    try:
        ocr_dir = output / "ocr"
        _private_dir(ocr_dir)
        ocr_argv = _child_argv(args, "ocr", ocr_dir)
        ocr_elapsed = _run_python_stage(
            runner, ocr_argv, stage="ocr", stage_dir=ocr_dir,
            stdout_path=ocr_dir / "worker.stdout.log", stderr_path=ocr_dir / "worker.stderr.log",
            timeout=deadline - time.monotonic(),
        )
        ocr_result = _read_json(ocr_dir / "stage-result.json", "ocr_stage_result_missing")
        if not isinstance(ocr_result, dict) or ocr_result.get("status") != "completed":
            detail = _read_json(ocr_dir / "stage-status.json", "ocr_stage_status_invalid")
            raise PipelineFailure(detail.get("error_code", "ocr_stage_failed") if isinstance(detail, dict) else "ocr_stage_failed")
        observations = ocr_dir / "observations.json"
        raster = Path(ocr_result.get("raster_path", "")).resolve()
        template_raw = ocr_result.get("template_evidence_path")
        template = Path(template_raw).resolve() if isinstance(template_raw, str) and template_raw else None
        if not _within(raster, ocr_dir) and raster != image:
            raise PipelineFailure("raster_path_outside_job")
        if template is not None and not _within(template, ocr_dir):
            raise PipelineFailure("template_evidence_path_invalid")
        _require_private_file(observations, "observations_missing_or_unprivate")
        if _sha256_file(observations) != ocr_result.get("observations_sha256"):
            raise PipelineFailure("observations_integrity_mismatch")
        _require_private_file(raster, "raster_missing_or_unprivate")
        if not raster.is_file() or _sha256_file(raster) != ocr_result.get("raster_sha256"):
            raise PipelineFailure("raster_integrity_mismatch")
        if template is not None:
            _require_private_file(template, "template_evidence_missing_or_unprivate")
        artifact_pins = {"observations": _sha256_file(observations), "raster": _sha256_file(raster)}
        if template is not None:
            artifact_pins["template_evidence"] = _sha256_file(template)
        manifest["artifact_sha256"]["after_ocr"] = dict(artifact_pins)
        manifest["phases"]["ocr"] = {
            "status": "completed", "elapsed_seconds": ocr_elapsed,
            "observations_sha256": ocr_result["observations_sha256"],
            "registration_status": ocr_result["registration_status"],
            "raster": ocr_result["raster"],
            "template_evidence_sha256": ocr_result.get("template_evidence_sha256"),
        }
        _write_private_json(manifest_path, manifest)

        baseline = output / "baseline.json"
        plan_path = output / "vl-plan.json"
        go_env = _safe_child_env(go=True)
        baseline_argv = _evaluator_argv(
            args, observations=observations, template=template,
            output=baseline, plan_output=plan_path,
        )
        go_elapsed = runner(
            baseline_argv, cwd=ROOT, env=go_env, stdout_path=output / "go-plan.stdout.log",
            stderr_path=output / "go-plan.stderr.log", timeout=deadline - time.monotonic(), stage="go_plan",
        )
        _require_private_file(baseline, "go_baseline_missing_or_unprivate")
        _require_private_file(plan_path, "go_plan_missing_or_unprivate")
        if not baseline.is_file() or not plan_path.is_file():
            raise PipelineFailure("go_plan_outputs_missing")
        manifest["phases"]["baseline_and_plan"] = {
            "status": "completed", "elapsed_seconds": go_elapsed,
            "baseline_sha256": _sha256_file(baseline), "plan_sha256": _sha256_file(plan_path),
        }
        artifact_pins["baseline"] = _sha256_file(baseline)
        artifact_pins["vl_plan"] = _sha256_file(plan_path)
        manifest["artifact_sha256"]["after_go_plan"] = dict(artifact_pins)
        _write_private_json(manifest_path, manifest)

        vl_dir = output / "vl"
        _private_dir(vl_dir)
        vl_argv = _child_argv(args, "vl", vl_dir, raster=raster, plan=plan_path)
        vl_elapsed = _run_python_stage(
            runner, vl_argv, stage="vl", stage_dir=vl_dir,
            stdout_path=vl_dir / "worker.stdout.log", stderr_path=vl_dir / "worker.stderr.log",
            timeout=deadline - time.monotonic(),
        )
        vl_result = _read_json(vl_dir / "stage-result.json", "vl_stage_result_missing")
        if not isinstance(vl_result, dict) or vl_result.get("status") != "completed":
            detail = _read_json(vl_dir / "stage-status.json", "vl_stage_status_invalid")
            raise PipelineFailure(detail.get("error_code", "vl_stage_failed") if isinstance(detail, dict) else "vl_stage_failed")
        evidence_path = vl_dir / "evidence.json"
        _require_private_file(evidence_path, "vl_evidence_missing_or_unprivate")
        if _sha256_file(evidence_path) != vl_result.get("evidence_sha256"):
            raise PipelineFailure("vl_evidence_integrity_mismatch")
        model_sha = vl_result.get("model_tree_sha256")
        if not isinstance(model_sha, str) or not _HEX64.fullmatch(model_sha):
            raise PipelineFailure("vl_model_identity_mismatch")
        manifest["phases"]["vl"] = {
            "status": "completed", "elapsed_seconds": vl_elapsed,
            "evidence_sha256": vl_result["evidence_sha256"], "model_tree_sha256": model_sha,
            "region_statuses": vl_result["region_statuses"],
        }
        artifact_pins["vl_evidence"] = _sha256_file(evidence_path)
        manifest["artifact_sha256"]["after_vl"] = dict(artifact_pins)
        _write_private_json(manifest_path, manifest)

        extraction = output / "extraction.json"
        final_argv = _evaluator_argv(
            args, observations=observations, template=template, output=extraction,
            vl_evidence=evidence_path, model_sha=model_sha,
        )
        final_elapsed = runner(
            final_argv, cwd=ROOT, env=go_env, stdout_path=output / "go-final.stdout.log",
            stderr_path=output / "go-final.stderr.log", timeout=deadline - time.monotonic(), stage="go_final",
        )
        _require_private_file(extraction, "final_extraction_missing_or_unprivate")
        if not extraction.is_file():
            raise PipelineFailure("final_extraction_missing")
        artifact_pins["final_extraction"] = _sha256_file(extraction)
        manifest["phases"]["final_extraction"] = {
            "status": "completed", "elapsed_seconds": final_elapsed,
            "extraction_sha256": _sha256_file(extraction),
        }

        pins_after = _source_pins()
        inputs_after = {
            "image": _sha256_file(image), "detector": _sha256_file(det_model),
            "recognizer": _sha256_file(rec_model), "evaluator": _sha256_file(evaluator),
            "python_ocr": _sha256_file(python_ocr), "python_vl": _sha256_file(python_vl),
        }
        vl_after = _model_tree_snapshot(model_dir)
        if pins_after != pins_before:
            raise PipelineFailure("pipeline_source_changed_during_job")
        if inputs_after != input_pins:
            raise PipelineFailure("input_changed_during_job")
        if vl_after["tree_sha256"] != model_sha:
            raise PipelineFailure("vl_model_changed_during_job")
        if _sha256_file(raster) != ocr_result["raster_sha256"]:
            raise PipelineFailure("raster_changed_during_job")
        if template is not None and _sha256_file(template) != ocr_result["template_evidence_sha256"]:
            raise PipelineFailure("template_evidence_changed_during_job")
        if (_sha256_file(observations) != artifact_pins["observations"]
                or _sha256_file(baseline) != artifact_pins["baseline"]
                or _sha256_file(plan_path) != artifact_pins["vl_plan"]
                or _sha256_file(evidence_path) != artifact_pins["vl_evidence"]
                or _sha256_file(extraction) != artifact_pins["final_extraction"]):
            raise PipelineFailure("pipeline_artifact_changed_during_job")
        manifest["artifact_sha256"]["after_final"] = dict(artifact_pins)
        manifest["source_pins_after"] = {
            "pipeline": pins_after, "inputs": inputs_after,
            "vl_model_tree_sha256": vl_after["tree_sha256"],
        }
        manifest["status"] = "completed"
        manifest["elapsed_seconds"] = MAX_JOB_SECONDS - max(0.0, deadline - time.monotonic())
        _write_private_json(manifest_path, manifest)
        return manifest
    except Exception as error:
        code = error.code if isinstance(error, PipelineFailure) else "pipeline_failed"
        manifest["status"] = "failed"
        manifest["error_code"] = code
        manifest["elapsed_seconds"] = MAX_JOB_SECONDS - max(0.0, deadline - time.monotonic())
        _write_private_json(manifest_path, manifest)
        _write_private_json(output / "failure.json", {
            "error_code": code, "exception_type": type(error).__name__,
            "traceback": traceback.format_exc(),
        })
        raise PipelineFailure(code) from error


def _private_stage_status(path: Path, status: str, code: str | None = None) -> None:
    value = {"status": status}
    if code:
        value["error_code"] = code
    _write_private_json(path / "stage-status.json", value)


def _run_ocr_stage(args: argparse.Namespace) -> int:
    stage_dir = args.stage_dir
    _private_dir(stage_dir)
    try:
        if str(ROOT) not in sys.path:
            sys.path.insert(0, str(ROOT))
        from ocr.experiments.template_field_pipeline import build_template_passport

        result = build_template_passport(
            args.image, args.image_id, args.det_model, args.rec_model,
            private_output_dir=stage_dir,
        )
        status = result.get("status")
        if status not in {"accepted", "fallback"}:
            raise PipelineFailure("ocr_registration_or_page_status_unusable")
        observations = result.get("observations")
        if not isinstance(observations, dict) or not isinstance(observations.get("image"), dict):
            raise PipelineFailure("ocr_observations_invalid")
        observations_path = stage_dir / "observations.json"
        _write_private_json(observations_path, observations)
        if status == "accepted":
            raster = Path(result.get("canonical_image_path", "")).resolve()
            template_plan = result.get("template_plan")
            if not isinstance(template_plan, dict):
                raise PipelineFailure("template_plan_missing")
            template_value = result.get("template_evidence_path")
            template_path = Path(template_value).resolve() if isinstance(template_value, str) and template_value else None
            if template_path is not None and not _within(template_path, stage_dir):
                raise PipelineFailure("template_evidence_path_invalid")
            raster_role = "registered_canonical_raster"
        else:
            raster = args.image.resolve()
            template_path = None
            raster_role = "source_image"
        raster_sha = _sha256_file(raster)
        identity = observations["image"]
        if identity.get("sha256") != raster_sha or identity.get("coordinate_frame") != "exif_normalized_pixels":
            raise PipelineFailure("ocr_raster_identity_mismatch")
        if template_path is not None and not template_path.is_file():
            raise PipelineFailure("template_evidence_missing")
        _write_private_json(stage_dir / "stage-result.json", {
            "status": "completed", "registration_status": status,
            "observations_sha256": _sha256_file(observations_path),
            "raster_path": str(raster), "raster_sha256": raster_sha,
            "raster": {
                "id": identity.get("id"), "sha256": raster_sha,
                "width": identity.get("width"), "height": identity.get("height"),
                "coordinate_frame": identity.get("coordinate_frame"), "role": raster_role,
            },
            "template_evidence_path": str(template_path) if template_path else None,
            "template_evidence_sha256": _sha256_file(template_path) if template_path else None,
        })
        _private_stage_status(stage_dir, "completed")
        return 0
    except Exception as error:
        code = error.code if isinstance(error, PipelineFailure) else "ocr_stage_failed"
        _write_private_json(stage_dir / "failure.json", {
            "error_code": code, "exception_type": type(error).__name__,
            "traceback": traceback.format_exc(),
        })
        _private_stage_status(stage_dir, "failed", code)
        return 1


def _run_vl_stage(args: argparse.Namespace) -> int:
    stage_dir = args.stage_dir
    _private_dir(stage_dir)
    try:
        if str(ROOT) not in sys.path:
            sys.path.insert(0, str(ROOT))
        from ocr.experiments.auto_vl_pipeline import read_auto_vl_fields

        plan = _read_json(args.plan_file, "vl_plan_invalid")
        evidence = read_auto_vl_fields(
            args.image, plan, private_output_dir=stage_dir, model_dir=args.vl_model_dir,
        )
        evidence_path = stage_dir / "evidence.json"
        if not evidence_path.is_file():
            _write_private_json(evidence_path, evidence)
        reader = evidence.get("reader") if isinstance(evidence, dict) else None
        if not isinstance(reader, dict):
            raise PipelineFailure("vl_evidence_reader_invalid")
        model_sha = reader.get("model_tree_sha256")
        if not isinstance(model_sha, str) or not _HEX64.fullmatch(model_sha):
            raise PipelineFailure("vl_model_identity_invalid")
        region_statuses = [region.get("status") for region in evidence.get("regions", []) if isinstance(region, dict)]
        _write_private_json(stage_dir / "stage-result.json", {
            "status": "completed", "evidence_sha256": _sha256_file(evidence_path),
            "model_tree_sha256": model_sha, "region_statuses": region_statuses,
        })
        _private_stage_status(stage_dir, "completed")
        return 0
    except Exception as error:
        code = error.code if isinstance(error, PipelineFailure) else getattr(error, "code", "vl_stage_failed")
        if not isinstance(code, str):
            code = "vl_stage_failed"
        _write_private_json(stage_dir / "failure.json", {
            "error_code": code, "exception_type": type(error).__name__,
            "traceback": traceback.format_exc(),
        })
        _private_stage_status(stage_dir, "failed", code)
        return 1


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--image-id", required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--evaluator", type=Path, required=True)
    parser.add_argument("--det-model", type=Path, required=True)
    parser.add_argument("--rec-model", type=Path, required=True)
    parser.add_argument("--vl-model-dir", type=Path, required=True)
    parser.add_argument("--vl-policy", choices=("primary", "union"), default="primary")
    parser.add_argument("--python-ocr", type=Path, default=DEFAULT_PYTHON_OCR)
    parser.add_argument("--python-vl", type=Path, default=DEFAULT_PYTHON_VL)
    parser.add_argument("--stage", choices=("ocr", "vl"), help=argparse.SUPPRESS)
    parser.add_argument("--stage-dir", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--plan-file", type=Path, help=argparse.SUPPRESS)
    return parser


def main(argv: list[str] | None = None) -> int:
    os.umask(0o077)
    args = _parser().parse_args(argv)
    if args.stage:
        if args.stage_dir is None:
            return 2
        if args.stage == "ocr":
            return _run_ocr_stage(args)
        if args.plan_file is None:
            return 2
        return _run_vl_stage(args)
    try:
        run_pipeline(args)
    except Exception as error:
        # Keep the public status line bounded and free of image, OCR, or field values.
        code = error.code if isinstance(error, PipelineFailure) else "pipeline_failed"
        print(f"run_status=failed error_code={code}")
        return 1
    print("run_status=completed stages=ocr,go_plan,vl,go_final review_required=true")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
