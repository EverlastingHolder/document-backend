#!/usr/bin/env python3
"""Bounded, private PaddleOCR-VL control on four fixed manual diagnostic ROIs.

This is a diagnostic only. It does not inspect ground truth, select text by
expected values, normalize OCR text, or feed outputs into production.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import signal
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

_RESEARCH_SUPPORT = _CODE_ROOT / "ocr/research/tools/ocr-roi-diagnostic"
if str(_RESEARCH_SUPPORT) not in sys.path:
    sys.path.insert(0, str(_RESEARCH_SUPPORT))
import time
from typing import Any


TASK_ROOT = ARCHIVE_WORK_ROOT / "ocr-roi-vl-control"
MANIFEST_PATH = TASK_ROOT.parent / "ocr-roi-diagnostic" / "crops.json"
OUTPUT_ROOT = TASK_ROOT / "private"
VENV_ROOT = Path(os.environ.get("PASSPORT_OCR_VL_ROOT", REPO_ROOT / ".venv-vl"))
MODEL_DIR = VL_MODEL_DIR
ROI_HELPER_PATH = REPO_ROOT / "ocr/research/tools/ocr-roi-diagnostic/run_roi_diagnostic.py"
EXPECTED_PACKAGES = {"paddleocr": "3.7.0", "paddlepaddle": "3.2.1", "paddlex": "3.7.2"}
PIPELINE_VERSION = "v1.6"
MODEL_NAME = "PaddleOCR-VL-1.6-0.9B"
PER_CASE_TIMEOUT_SECONDS = 180.0
TOTAL_TIMEOUT_SECONDS = 900.0
TERMINATE_GRACE_SECONDS = 2.0
KILL_GRACE_SECONDS = 2.0
GEOMETRY_SCOPE = "whole_crop_synthetic_prompt_box"
MAX_NEW_TOKENS = 512
EXPECTED_CASES = (
    ("sample01", "issuer"),
    ("sample01", "birth_place"),
    ("sample02", "given_name"),
    ("sample04", "issuer"),
)
PINNED_FILES = {
    "runner_sha256": Path(__file__).resolve(),
    "roi_helper_sha256": ROI_HELPER_PATH,
    "path_helper_sha256": REPO_ROOT / "ocr/experiments/local_paths.py",
    "crop_manifest_sha256": MANIFEST_PATH,
}


class ControlError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _sha_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _source_file_pins() -> dict[str, str]:
    if any(not path.is_file() for path in PINNED_FILES.values()):
        raise ControlError("pinned_source_file_missing")
    return {key: _sha_file(path) for key, path in PINNED_FILES.items()}


def _verify_result_hashes(case_dir: Path, status: dict[str, Any]) -> dict[str, str]:
    try:
        actual = {
            "vendor_result_sha256": _sha_file(case_dir / "vendor-result.json"),
            "case_result_sha256": _sha_file(case_dir / "case-result.json"),
        }
    except OSError as error:
        raise ControlError("result_file_missing") from error
    if status.get("result_sha256") != actual:
        raise ControlError("result_hash_mismatch")
    return actual


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def _write_private_json(path: Path, value: Any) -> None:
    _private_dir(path.parent)
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        os.chmod(temporary, 0o600)
        json.dump(value, stream, ensure_ascii=False, sort_keys=True, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    path.chmod(0o600)


def _load_roi_module():
    """Import the existing pixel/hash validator; it never opens OCR or gold files."""
    if str(REPO_ROOT) not in sys.path:
        sys.path.insert(0, str(REPO_ROOT))
    if str(MANIFEST_PATH.parent) not in sys.path:
        sys.path.insert(0, str(MANIFEST_PATH.parent))
    os.environ.setdefault("PASSPORT_BACKEND_REPO_ROOT", str(REPO_ROOT))
    import run_roi_diagnostic as roi_diagnostic
    return roi_diagnostic


def _load_fixed_cases() -> tuple[Any, dict[tuple[str, str], dict[str, Any]]]:
    roi = _load_roi_module()
    items = roi._load_manifest(MANIFEST_PATH)
    return roi, _select_cases(items)


def _select_cases(items: list[dict[str, Any]]) -> dict[tuple[str, str], dict[str, Any]]:
    selected: dict[tuple[str, str], dict[str, Any]] = {}
    for item in items:
        key = (item.get("sample_id"), item.get("field_id"))
        if key not in EXPECTED_CASES:
            continue
        if key in selected:
            raise ControlError("fixed_case_duplicate")
        selected[key] = item
    if set(selected) != set(EXPECTED_CASES):
        raise ControlError("fixed_case_set_mismatch")
    if len(selected) != len(EXPECTED_CASES):
        raise ControlError("fixed_case_duplicate")
    return selected


def _validate_case(roi: Any, item: dict[str, Any]) -> tuple[Path, Path, dict[str, Any]]:
    try:
        source_path, crop_path, _rgb, binding = roi._validate_input(item, MANIFEST_PATH)
    except Exception as error:
        code = getattr(error, "code", None)
        raise ControlError(code if isinstance(code, str) else "input_validation_failed") from error
    # Keep only stable identity facts in the run record; paths remain local to
    # the child argv and are not emitted on stdout or copied into result JSON.
    safe_binding = {
        "sample_id": item["sample_id"],
        "field_id": item["field_id"],
        "source_sha256": binding["source_sha256"],
        "crop_sha256": binding["crop_sha256"],
        "source_dimensions_exif_normalized": binding["source_dimensions_exif_normalized"],
        "crop_dimensions_exif_normalized": binding["crop_dimensions_exif_normalized"],
        "crop_box_source_xyxy": binding["crop_box_source_xyxy"],
        "geometry_provenance": binding["geometry_provenance"],
    }
    return source_path, crop_path, safe_binding


def _model_tree_snapshot(model_dir: Path) -> dict[str, Any]:
    if not model_dir.is_dir() or model_dir.is_symlink():
        raise ControlError("local_model_directory_missing")
    if not (model_dir / "inference.yml").is_file() or not (model_dir / "config.json").is_file():
        raise ControlError("local_model_metadata_missing")
    inference_text = (model_dir / "inference.yml").read_text(encoding="utf-8")
    if f"model_name: {MODEL_NAME}" not in inference_text:
        raise ControlError("local_model_name_mismatch")

    files: dict[str, str] = {}
    for root, dirs, filenames in os.walk(model_dir, followlinks=False):
        root_path = Path(root)
        dirs.sort()
        filenames.sort()
        for directory in dirs:
            if (root_path / directory).is_symlink():
                raise ControlError("model_tree_symlink_rejected")
        for filename in filenames:
            path = root_path / filename
            if path.is_symlink() or not path.is_file():
                raise ControlError("model_tree_nonregular_file_rejected")
            files[path.relative_to(model_dir).as_posix()] = _sha_file(path)
    if not files or not any(name.endswith((".safetensors", ".pdparams", ".bin")) for name in files):
        raise ControlError("local_model_weights_missing")
    digest = hashlib.sha256()
    for relative_path, file_hash in sorted(files.items()):
        digest.update(relative_path.encode("utf-8"))
        digest.update(b"\0")
        digest.update(file_hash.encode("ascii"))
        digest.update(b"\n")
    return {"file_count": len(files), "tree_sha256": digest.hexdigest(), "files_sha256": files}


def _candidate_blocks(vendor_result: Any) -> list[dict[str, Any]]:
    if not isinstance(vendor_result, dict):
        raise ControlError("vendor_result_root_invalid")
    data = vendor_result.get("res") if set(vendor_result) == {"res"} and isinstance(vendor_result.get("res"), dict) else vendor_result
    blocks = data.get("parsing_res_list") if isinstance(data, dict) else None
    if not isinstance(blocks, list) or not blocks:
        raise ControlError("vendor_result_blocks_missing")
    candidates = []
    for block in blocks:
        if not isinstance(block, dict) or not isinstance(block.get("block_content"), str):
            raise ControlError("vendor_result_block_text_missing")
        candidate = {"block_content": block["block_content"]}
        if isinstance(block.get("block_label"), str):
            candidate["block_label"] = block["block_label"]
        if "block_bbox" in block:
            candidate["block_bbox"] = block["block_bbox"]  # unchanged vendor value
        candidates.append(candidate)
    return candidates


def _generated_length_cap_status(vendor_result: Any, cap: int = MAX_NEW_TOKENS) -> bool | None:
    """Use only explicit vendor token-count metadata; text length is not evidence."""
    found: list[int] = []

    def visit(value: Any) -> None:
        if isinstance(value, dict):
            length = value.get("generated_length")
            if isinstance(length, int) and not isinstance(length, bool) and length >= 0:
                found.append(length)
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(vendor_result)
    if not found:
        return None
    return any(length >= cap for length in found)


def _runtime_identity() -> dict[str, Any]:
    if Path(sys.prefix).resolve() != VENV_ROOT.resolve():
        raise ControlError("pinned_venv_prefix_mismatch")
    versions = {}
    for package, expected in EXPECTED_PACKAGES.items():
        try:
            actual = importlib.metadata.version(package)
        except importlib.metadata.PackageNotFoundError as error:
            raise ControlError("pinned_package_missing") from error
        if actual != expected:
            raise ControlError("pinned_package_version_mismatch")
        versions[package] = actual
    return {"python": sys.version.split()[0], "sys_prefix_matches": True, "packages": versions}


def _child_env(output_dir: Path) -> dict[str, str]:
    keep = ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL")
    env = {key: os.environ[key] for key in keep if key in os.environ}
    scratch = output_dir / "runtime-cache"
    _private_dir(scratch)
    env.update({
        "PADDLE_PDX_CACHE_HOME": str(scratch / "paddlex"),
        "PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK": "True",
        "HF_HOME": str(scratch / "huggingface"),
        "HUGGINGFACE_HUB_CACHE": str(scratch / "huggingface" / "hub"),
        "TRANSFORMERS_CACHE": str(scratch / "transformers"),
        "HF_HUB_OFFLINE": "1",
        "TRANSFORMERS_OFFLINE": "1",
        "PYTHONUNBUFFERED": "1",
        "TOKENIZERS_PARALLELISM": "false",
    })
    return env


def _kill_process_group(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=TERMINATE_GRACE_SECONDS)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=KILL_GRACE_SECONDS)
    except subprocess.TimeoutExpired as error:
        raise ControlError("worker_did_not_exit_after_kill") from error


def _run_child(case_key: tuple[str, str], output_dir: Path, env: dict[str, str], deadline: float) -> dict[str, Any]:
    case_id = f"{case_key[0]}-{case_key[1]}"
    case_dir = output_dir / case_id
    _private_dir(case_dir)
    stdout_path, stderr_path = case_dir / "worker.stdout.log", case_dir / "worker.stderr.log"
    result_path = case_dir / "vendor-result.json"
    status_path = case_dir / "worker-status.json"
    remaining = deadline - time.monotonic()
    timeout = min(PER_CASE_TIMEOUT_SECONDS, remaining)
    if timeout <= 0:
        raise ControlError("total_timeout")
    argv = [sys.executable, str(Path(__file__).resolve()), "--child", case_key[0], case_key[1]]
    started = time.monotonic()
    with stdout_path.open("wb") as stdout_file, stderr_path.open("wb") as stderr_file:
        os.chmod(stdout_path, 0o600)
        os.chmod(stderr_path, 0o600)
        try:
            process = subprocess.Popen(argv, cwd=REPO_ROOT, env=env, stdout=stdout_file, stderr=stderr_file, start_new_session=True)
        except OSError as error:
            raise ControlError("worker_start_failed") from error
        try:
            returncode = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            _kill_process_group(process)
            elapsed = round(time.monotonic() - started, 3)
            _write_private_json(status_path, {"status": "timeout", "error_code": "worker_timeout", "wall_seconds": elapsed})
            return {"case_id": case_id, "status": "timeout", "error_code": "worker_timeout", "wall_seconds": elapsed}
    elapsed = round(time.monotonic() - started, 3)
    if returncode != 0:
        child_status = None
        try:
            child_status = json.loads(status_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            pass
        code = child_status.get("error_code") if isinstance(child_status, dict) else None
        code = code if isinstance(code, str) else f"worker_exit_{returncode}"
        return {"case_id": case_id, "status": "failed", "error_code": code, "wall_seconds": elapsed}
    try:
        worker_status = json.loads(status_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ControlError("worker_status_missing_or_invalid") from error
    if not isinstance(worker_status, dict) or worker_status.get("status") != "completed":
        raise ControlError("worker_status_not_completed")
    result_hashes = _verify_result_hashes(case_dir, worker_status)
    worker_status["wall_seconds"] = elapsed
    _write_private_json(status_path, worker_status)
    return {"case_id": case_id, "status": "completed", "wall_seconds": elapsed,
            "model_init_seconds": worker_status.get("model_init_seconds"),
            "predict_seconds": worker_status.get("predict_seconds"),
            "result_sha256": result_hashes}


def _run_parent() -> int:
    started = time.monotonic()
    deadline = started + TOTAL_TIMEOUT_SECONDS
    source_pins_before = _source_file_pins()
    runtime = _runtime_identity()
    model_before = _model_tree_snapshot(MODEL_DIR)
    roi, selected = _load_fixed_cases()
    bindings: list[dict[str, Any]] = []
    validated: dict[tuple[str, str], tuple[Path, Path]] = {}
    for key in EXPECTED_CASES:
        item = selected[key]
        source_path, crop_path, binding = _validate_case(roi, item)
        validated[key] = (source_path, crop_path)
        bindings.append(binding)

    if OUTPUT_ROOT.exists():
        raise ControlError("private_output_already_exists")
    _private_dir(OUTPUT_ROOT)
    _private_dir(OUTPUT_ROOT / "run")
    output_dir = OUTPUT_ROOT / "run"
    env = _child_env(output_dir)
    run_manifest: dict[str, Any] = {
        "schema_version": "passport-roi-vl-control/v1",
        "purpose": "four_fixed_manual_roi_diagnostic_only",
        "status": "running",
        "reference_read": False,
        "gold_read": False,
        "cases": bindings,
        "runtime": runtime,
        "configured": {
            "pipeline_version": PIPELINE_VERSION,
            "model_name": MODEL_NAME,
            "model_dir_role": "preinstalled_local_model_cache",
            "backend": "native",
            "device": "cpu",
            "use_layout_detection": False,
            "prompt_label": "ocr",
            "max_new_tokens": MAX_NEW_TOKENS,
            "use_doc_orientation_classify": False,
            "use_doc_unwarping": False,
            "geometry_scope": GEOMETRY_SCOPE,
            "per_case_timeout_seconds": PER_CASE_TIMEOUT_SECONDS,
            "total_timeout_seconds": TOTAL_TIMEOUT_SECONDS,
            "network_policy": "offline_env_no_proxy_no_api_credentials_or_server_url",
        },
        "model_tree_before": model_before,
        "source_pins_before": source_pins_before,
        "worker_statuses": [],
    }
    manifest_file = output_dir / "run-manifest.json"
    _write_private_json(manifest_file, run_manifest)

    statuses = []
    final_status = "failed"
    error_code = None
    try:
        for key in EXPECTED_CASES:
            if time.monotonic() >= deadline:
                raise ControlError("total_timeout")
            status = _run_child(key, output_dir, env, deadline)
            statuses.append(status)
            run_manifest["worker_statuses"] = statuses
            _write_private_json(manifest_file, run_manifest)
            print(f"case={status['case_id']} status={status['status']} wall_seconds={status['wall_seconds']}", flush=True)
            if status["status"] != "completed":
                error_code = status.get("error_code", "worker_failed")
                break
            # Verify the bound source and crop stayed byte-identical through this
            # child; this records identity only, never OCR-derived decisions.
            item = selected[key]
            source_path, crop_path = validated[key]
            if _sha_file(source_path) != item["source_sha256"] or _sha_file(crop_path) != item["crop_sha256"]:
                error_code = "input_changed_during_job"
                break
        model_after = _model_tree_snapshot(MODEL_DIR)
        run_manifest["model_tree_after"] = model_after
        if model_before["files_sha256"] != model_after["files_sha256"]:
            error_code = "local_model_tree_changed_during_job"
        if error_code is None and len(statuses) == len(EXPECTED_CASES):
            final_status = "completed"
        elif error_code is None:
            error_code = "incomplete_case_set"
    except ControlError as error:
        error_code = error.code
        try:
            run_manifest["model_tree_after"] = _model_tree_snapshot(MODEL_DIR)
        except ControlError:
            run_manifest["model_tree_after"] = None
    run_manifest["source_pins_after"] = _source_file_pins()
    if source_pins_before != run_manifest["source_pins_after"] and error_code is None:
        error_code = "pinned_source_changed_during_job"
        final_status = "failed"
    run_manifest.update({
        "status": final_status,
        "error_code": error_code,
        "elapsed_seconds": round(time.monotonic() - started, 3),
        "worker_statuses": statuses,
    })
    _write_private_json(manifest_file, run_manifest)
    print(f"run_status={final_status} case_count={len(statuses)} elapsed_seconds={run_manifest['elapsed_seconds']}" + (f" error_code={error_code}" if error_code else ""), flush=True)
    return 0 if final_status == "completed" else 1


def _run_worker(sample_id: str, field_id: str) -> int:
    case_key = (sample_id, field_id)
    if case_key not in EXPECTED_CASES:
        raise ControlError("unexpected_child_case")
    started = time.perf_counter()
    roi, selected = _load_fixed_cases()
    item = selected[case_key]
    _source_path, crop_path, binding = _validate_case(roi, item)
    model_snapshot = _model_tree_snapshot(MODEL_DIR)
    runtime = _runtime_identity()

    import paddle
    from paddleocr import PaddleOCRVL

    if paddle.is_compiled_with_cuda():
        raise ControlError("cuda_build_not_allowed")
    paddle.set_device("cpu")
    device = paddle.device.get_device()
    if not device.startswith("cpu"):
        raise ControlError("cpu_device_not_selected")

    init_started = time.perf_counter()
    pipeline = PaddleOCRVL(
        pipeline_version=PIPELINE_VERSION,
        device="cpu",
        vl_rec_backend="native",
        vl_rec_model_name=MODEL_NAME,
        vl_rec_model_dir=str(MODEL_DIR),
        vl_rec_api_key=None,
        vl_rec_server_url=None,
        use_layout_detection=False,
        use_doc_orientation_classify=False,
        use_doc_unwarping=False,
        use_chart_recognition=False,
        use_seal_recognition=False,
    )
    init_seconds = time.perf_counter() - init_started

    predict_started = time.perf_counter()
    predictions = list(pipeline.predict(
        str(crop_path),
        use_layout_detection=False,
        prompt_label="ocr",
        max_new_tokens=MAX_NEW_TOKENS,
    ))
    predict_seconds = time.perf_counter() - predict_started
    if len(predictions) != 1 or not hasattr(predictions[0], "save_to_json"):
        raise ControlError("vendor_result_shape_unsupported")

    output_dir = OUTPUT_ROOT / "run" / f"{sample_id}-{field_id}"
    _private_dir(output_dir)
    raw_path = output_dir / "vendor-result.json"
    predictions[0].save_to_json(save_path=str(raw_path))
    raw_path.chmod(0o600)
    try:
        vendor_result = json.loads(raw_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ControlError("vendor_result_invalid_json") from error
    data = vendor_result.get("res") if set(vendor_result) == {"res"} and isinstance(vendor_result.get("res"), dict) else vendor_result
    model_settings = data.get("model_settings") if isinstance(data, dict) else None
    if not isinstance(model_settings, dict) or model_settings.get("use_layout_detection") is not False:
        raise ControlError("runtime_layout_detection_not_disabled")
    if model_settings.get("use_doc_preprocessor") is not False:
        raise ControlError("runtime_document_preprocessor_not_disabled")
    candidates = _candidate_blocks(vendor_result)
    length_cap_reached = _generated_length_cap_status(vendor_result)

    case_json = {
        "schema_version": "passport-roi-vl-control-case/v1",
        "sample_id": sample_id,
        "field_id": field_id,
        "input_identity": binding,
        "candidate_blocks_in_vendor_order": candidates,
        "generated_length_at_or_above_cap": length_cap_reached,
        "generated_length_cap": MAX_NEW_TOKENS,
        "generation_cap_note": "null means PaddleOCR-VL serializer did not expose generated_length; no truncation is inferred from text length",
        "geometry_scope": GEOMETRY_SCOPE,
        "geometry_note": "layout disabled; PaddleX supplies one synthetic whole-crop prompt box; no word geometry is claimed",
        "vendor_result": vendor_result,
        "runtime": {
            **runtime,
            "device": device,
            "cuda_compiled": False,
            "pipeline_version": PIPELINE_VERSION,
            "model_name": MODEL_NAME,
            "model_tree_sha256": model_snapshot["tree_sha256"],
            "observed_model_settings": {
                "use_layout_detection": model_settings["use_layout_detection"],
                "use_doc_preprocessor": model_settings["use_doc_preprocessor"],
            },
        },
        "model_init_seconds": round(init_seconds, 3),
        "predict_seconds": round(predict_seconds, 3),
        "worker_wall_seconds": round(time.perf_counter() - started, 3),
    }
    case_result_path = output_dir / "case-result.json"
    _write_private_json(case_result_path, case_json)
    _write_private_json(output_dir / "worker-status.json", {
        "status": "completed",
        "sample_id": sample_id,
        "field_id": field_id,
        "model_init_seconds": round(init_seconds, 3),
        "predict_seconds": round(predict_seconds, 3),
        "result_sha256": {
            "vendor_result_sha256": _sha_file(raw_path),
            "case_result_sha256": _sha_file(case_result_path),
        },
    })
    if hasattr(pipeline, "close"):
        pipeline.close()
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--child", nargs=2, metavar=("SAMPLE_ID", "FIELD_ID"))
    args = parser.parse_args(argv)
    try:
        if args.child:
            return _run_worker(args.child[0], args.child[1])
        return _run_parent()
    except ControlError as error:
        if args.child:
            output_dir = OUTPUT_ROOT / "run" / f"{args.child[0]}-{args.child[1]}"
            try:
                _write_private_json(output_dir / "worker-status.json", {"status": "failed", "error_code": error.code})
            except OSError:
                pass
        else:
            print(f"run_status=failed error_code={error.code}", flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
