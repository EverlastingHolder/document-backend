"""Private automatic field-crop reader using a Go-produced field plan.

This module reads only the supplied image and plan. It has no gold/reference
input, no manual crop manifest, and does not normalize or select OCR text.
"""
from __future__ import annotations

import copy
import hashlib
import importlib.metadata
import json
import math
import os
from pathlib import Path
import re
import sys
import time
import traceback
from typing import Any, Callable

from PIL import Image, ImageOps

REPO_ROOT = Path(__file__).resolve().parents[2]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from ocr.experiments.local_paths import VL_MODEL_DIR  # noqa: E402


SCHEMA_PLAN = "passport-vl-field-plan/v1"
SCHEMA_EVIDENCE = "passport-vl-field-evidence/v1"
MODEL_ID = "PaddleOCR-VL-1.6-0.9B"
PIPELINE_VERSION = "v1.6"
GEOMETRY_SCOPE = "whole_crop_synthetic_prompt_box"
MAX_REGIONS = 6
MAX_NEW_TOKENS = 512
EXPECTED_PACKAGES = {"paddleocr": "3.7.0", "paddlepaddle": "3.2.1", "paddlex": "3.7.2"}
DEFAULT_MODEL_DIR = VL_MODEL_DIR
ALLOWED_FIELDS = frozenset({
    "surname", "given_name", "patronymic", "gender", "birth_place", "issuer",
})
# Recipe names are intentionally closed. The planner can choose a recipe, but
# the reader never interprets it as coordinates or executes recipe text.
ALLOWED_RECIPES = frozenset({
    "registered_template_crop/v1", "baseline_candidate_rows/v1",
    "birth_date_neighbor/v1", "authority_text_block/v1", "coherent_name_rows/v1",
})
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_ASCII_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


class AutoVLFailure(RuntimeError):
    """Safe coded failure; exception details belong only in private diagnostics."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def model_tree_snapshot(model_dir: str | Path) -> dict[str, Any]:
    """Fingerprint a local model without importing Paddle or contacting a hub."""
    root = Path(model_dir)
    if not root.is_dir() or root.is_symlink():
        raise AutoVLFailure("local_model_directory_missing")
    if not (root / "inference.yml").is_file() or not (root / "config.json").is_file():
        raise AutoVLFailure("local_model_metadata_missing")
    try:
        if f"model_name: {MODEL_ID}" not in (root / "inference.yml").read_text(encoding="utf-8"):
            raise AutoVLFailure("local_model_name_mismatch")
    except OSError as error:
        raise AutoVLFailure("local_model_metadata_unreadable") from error
    files: dict[str, str] = {}
    for current, directories, filenames in os.walk(root, followlinks=False):
        current_path = Path(current)
        directories.sort()
        filenames.sort()
        if any((current_path / name).is_symlink() for name in directories):
            raise AutoVLFailure("model_tree_symlink_rejected")
        for name in filenames:
            path = current_path / name
            if path.is_symlink() or not path.is_file():
                raise AutoVLFailure("model_tree_nonregular_file_rejected")
            files[path.relative_to(root).as_posix()] = _sha256_file(path)
    if not files or not any(name.endswith((".safetensors", ".pdparams", ".bin")) for name in files):
        raise AutoVLFailure("local_model_weights_missing")
    digest = hashlib.sha256()
    for name, value in sorted(files.items()):
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(value.encode("ascii"))
        digest.update(b"\n")
    return {"file_count": len(files), "tree_sha256": digest.hexdigest(), "files_sha256": files}


def validate_plan(plan: Any) -> dict[str, Any]:
    if not isinstance(plan, dict) or set(plan) != {
        "schema_version", "image", "baseline_sha256", "regions", "skipped_fields", "plan_sha256"
    }:
        raise AutoVLFailure("plan_shape_invalid")
    if plan["schema_version"] != SCHEMA_PLAN:
        raise AutoVLFailure("plan_schema_unsupported")
    image = plan["image"]
    if not isinstance(image, dict) or set(image) != {"id", "sha256", "width", "height", "coordinate_frame"}:
        raise AutoVLFailure("plan_image_shape_invalid")
    if not isinstance(image["id"], str) or not _ASCII_ID.fullmatch(image["id"]):
        raise AutoVLFailure("plan_image_id_invalid")
    if not isinstance(image["sha256"], str) or not _HEX64.fullmatch(image["sha256"]):
        raise AutoVLFailure("plan_image_sha_invalid")
    if any(isinstance(image[k], bool) or not isinstance(image[k], int) or image[k] <= 0 for k in ("width", "height")):
        raise AutoVLFailure("plan_dimensions_invalid")
    if image["coordinate_frame"] != "exif_normalized_pixels":
        raise AutoVLFailure("plan_coordinate_frame_invalid")
    if not isinstance(plan["baseline_sha256"], str) or not _HEX64.fullmatch(plan["baseline_sha256"]):
        raise AutoVLFailure("plan_baseline_sha_invalid")
    if not isinstance(plan["plan_sha256"], str) or not _HEX64.fullmatch(plan["plan_sha256"]):
        raise AutoVLFailure("plan_sha_invalid")
    regions, skipped = plan["regions"], plan["skipped_fields"]
    if not isinstance(regions, list) or len(regions) > MAX_REGIONS:
        raise AutoVLFailure("plan_region_count_invalid")
    if not isinstance(skipped, list):
        raise AutoVLFailure("plan_skipped_fields_invalid")
    seen_ids: set[str] = set()
    seen_fields: set[str] = set()
    for region in regions:
        if not isinstance(region, dict) or set(region) != {"id", "field_id", "recipe", "crop", "anchor_line_ids"}:
            raise AutoVLFailure("plan_region_shape_invalid")
        rid, field_id, recipe = region["id"], region["field_id"], region["recipe"]
        if not isinstance(rid, str) or not _ASCII_ID.fullmatch(rid) or not rid.startswith("vl-"):
            raise AutoVLFailure("plan_region_id_invalid")
        if not isinstance(field_id, str) or field_id not in ALLOWED_FIELDS:
            raise AutoVLFailure("plan_field_invalid")
        if rid != "vl-" + field_id:
            raise AutoVLFailure("plan_region_id_field_mismatch")
        if field_id in seen_fields or rid in seen_ids:
            raise AutoVLFailure("plan_region_duplicate")
        if not isinstance(recipe, str) or recipe not in ALLOWED_RECIPES:
            raise AutoVLFailure("plan_recipe_invalid")
        crop = region["crop"]
        if not isinstance(crop, list) or len(crop) != 4 or any(isinstance(v, bool) or not isinstance(v, int) for v in crop):
            raise AutoVLFailure("plan_crop_invalid")
        x0, y0, x1, y1 = crop
        if not (0 <= x0 < x1 <= image["width"] and 0 <= y0 < y1 <= image["height"]):
            raise AutoVLFailure("plan_crop_out_of_bounds")
        anchors = region["anchor_line_ids"]
        if not isinstance(anchors, list) or not anchors or any(not isinstance(a, str) or not _ASCII_ID.fullmatch(a) for a in anchors):
            raise AutoVLFailure("plan_anchors_invalid")
        if len(set(anchors)) != len(anchors):
            raise AutoVLFailure("plan_anchor_duplicate")
        seen_ids.add(rid)
        seen_fields.add(field_id)
    skipped_fields: set[str] = set()
    for item in skipped:
        if not isinstance(item, dict) or set(item) != {"field_id", "reason"}:
            raise AutoVLFailure("plan_skipped_field_shape_invalid")
        if item["field_id"] not in ALLOWED_FIELDS or item["field_id"] in skipped_fields or item["field_id"] in seen_fields:
            raise AutoVLFailure("plan_skipped_field_invalid")
        if not isinstance(item["reason"], str) or not item["reason"] or len(item["reason"]) > 256:
            raise AutoVLFailure("plan_skipped_reason_invalid")
        skipped_fields.add(item["field_id"])
    if seen_fields | skipped_fields != ALLOWED_FIELDS:
        raise AutoVLFailure("plan_field_coverage_invalid")
    return copy.deepcopy(plan)


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


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


def _plan_object_snapshot(plan: dict[str, Any]) -> str:
    try:
        payload = json.dumps(plan, ensure_ascii=True, sort_keys=True, separators=(",", ":"), allow_nan=False)
    except (TypeError, ValueError) as error:
        raise AutoVLFailure("plan_not_json_serializable") from error
    return hashlib.sha256(payload.encode("ascii")).hexdigest()


def _runtime_identity() -> dict[str, Any]:
    if Path(sys.prefix).resolve() != (REPO_ROOT / ".venv-vl").resolve():
        raise AutoVLFailure("pinned_venv_prefix_mismatch")
    versions: dict[str, str] = {}
    for package, expected in EXPECTED_PACKAGES.items():
        try:
            actual = importlib.metadata.version(package)
        except importlib.metadata.PackageNotFoundError as error:
            raise AutoVLFailure("pinned_package_missing") from error
        if actual != expected:
            raise AutoVLFailure("pinned_package_version_mismatch")
        versions[package] = actual
    return {"python": sys.version.split()[0], "packages": versions}


def _configure_offline_environment(private_dir: Path) -> None:
    scratch = private_dir / "runtime-cache"
    _private_dir(scratch)
    os.environ.update({
        "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
        "PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK": "True",
        "PADDLE_PDX_CACHE_HOME": str(scratch / "paddlex"),
        "HF_HOME": str(scratch / "huggingface"),
        "HUGGINGFACE_HUB_CACHE": str(scratch / "huggingface" / "hub"),
        "TRANSFORMERS_CACHE": str(scratch / "transformers"),
        "TOKENIZERS_PARALLELISM": "false",
    })


def _new_pipeline(model_dir: Path) -> Any:
    runtime = _runtime_identity()
    del runtime
    import paddle
    from paddleocr import PaddleOCRVL

    if paddle.is_compiled_with_cuda():
        raise AutoVLFailure("cuda_build_not_allowed")
    paddle.set_device("cpu")
    device = paddle.device.get_device()
    if not isinstance(device, str) or not device.startswith("cpu"):
        raise AutoVLFailure("cpu_device_not_selected")
    return PaddleOCRVL(
        pipeline_version=PIPELINE_VERSION, device="cpu", vl_rec_backend="native",
        vl_rec_model_name=MODEL_ID, vl_rec_model_dir=str(model_dir),
        vl_rec_api_key=None, vl_rec_server_url=None, use_layout_detection=False,
        use_doc_orientation_classify=False, use_doc_unwarping=False,
        use_chart_recognition=False, use_seal_recognition=False,
    )


def _vendor_result(prediction: Any, path: Path) -> dict[str, Any]:
    if hasattr(prediction, "save_to_json"):
        try:
            prediction.save_to_json(save_path=str(path))
        finally:
            if path.exists():
                path.chmod(0o600)
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise AutoVLFailure("vendor_result_invalid_json") from error
        if isinstance(value, dict):
            return value
    if isinstance(prediction, dict):
        return prediction
    raise AutoVLFailure("vendor_result_shape_unsupported")


def _result_data(result: dict[str, Any]) -> dict[str, Any]:
    data = result.get("res") if set(result) == {"res"} and isinstance(result.get("res"), dict) else result
    if not isinstance(data, dict):
        raise AutoVLFailure("vendor_result_root_invalid")
    settings = data.get("model_settings")
    if not isinstance(settings, dict) or settings.get("use_layout_detection") is not False:
        raise AutoVLFailure("runtime_layout_detection_not_disabled")
    if settings.get("use_doc_preprocessor") is not False:
        raise AutoVLFailure("runtime_document_preprocessor_not_disabled")
    return data


def _raw_blocks(data: dict[str, Any]) -> list[dict[str, str]]:
    blocks = data.get("parsing_res_list")
    if blocks is None:
        return []
    if not isinstance(blocks, list):
        raise AutoVLFailure("vendor_result_blocks_invalid")
    result: list[dict[str, str]] = []
    for block in blocks:
        if not isinstance(block, dict) or not isinstance(block.get("block_content"), str):
            raise AutoVLFailure("vendor_result_block_text_missing")
        if block["block_content"].strip():
            result.append({"text_raw": block["block_content"]})
    return result


def _cap_reached(value: Any) -> bool | None:
    found: list[int] = []
    def visit(node: Any) -> None:
        if isinstance(node, dict):
            length = node.get("generated_length")
            if isinstance(length, int) and not isinstance(length, bool) and length >= 0:
                found.append(length)
            for child in node.values():
                visit(child)
        elif isinstance(node, list):
            for child in node:
                visit(child)
    visit(value)
    return any(length >= MAX_NEW_TOKENS for length in found) if found else None


def read_auto_vl_fields(
    image_path: str | Path,
    plan: dict[str, Any],
    *,
    private_output_dir: str | Path,
    model_dir: str | Path = DEFAULT_MODEL_DIR,
    pipeline: Any | None = None,
) -> dict[str, Any]:
    """Read the plan's at most six crops with one local PaddleOCR-VL pipeline.

    `pipeline` is an injection seam for fast unit tests; production callers omit
    it. It must implement PaddleOCR-VL's `predict` API and return native vendor
    predictions. This function accepts no expected text, reference data, or
    manually supplied crops.
    """
    input_plan_snapshot = _plan_object_snapshot(plan)
    frozen_plan = validate_plan(plan)
    plan_digest = frozen_plan["plan_sha256"]
    image_file = Path(image_path)
    try:
        source_sha = _sha256_file(image_file)
    except OSError as error:
        raise AutoVLFailure("source_image_unreadable") from error
    if source_sha != frozen_plan["image"]["sha256"]:
        raise AutoVLFailure("source_image_sha_mismatch")
    try:
        with Image.open(image_file) as source:
            normalized = ImageOps.exif_transpose(source).convert("RGB")
            normalized.load()
    except Exception as error:
        raise AutoVLFailure("source_image_decode_failed") from error
    width, height = normalized.size
    if (width, height) != (frozen_plan["image"]["width"], frozen_plan["image"]["height"]):
        raise AutoVLFailure("source_image_dimensions_mismatch")

    out_dir = Path(private_output_dir)
    _private_dir(out_dir)
    model_root = Path(model_dir)
    model_before = model_tree_snapshot(model_root)
    model_identity = model_before["tree_sha256"]
    engine = pipeline
    if frozen_plan["regions"] and engine is None:
        _configure_offline_environment(out_dir)
        _runtime_identity()
        engine = _new_pipeline(model_root)
    reader = {
        "model_id": MODEL_ID, "model_tree_sha256": model_identity,
        "backend": "native", "device": "cpu", "pipeline_version": PIPELINE_VERSION,
        "use_layout_detection": False, "use_doc_preprocessor": False,
    }
    evidence_regions: list[dict[str, Any]] = []
    for region in frozen_plan["regions"]:
        region_dir = out_dir / region["id"]
        _private_dir(region_dir)
        x0, y0, x1, y1 = region["crop"]
        crop_path = region_dir / "crop.png"
        normalized.crop((x0, y0, x1, y1)).save(crop_path, format="PNG")
        crop_path.chmod(0o600)
        before_crop = _sha256_file(crop_path)
        started = time.monotonic()
        status, blocks, cap = "error", [], None
        try:
            predictions = list(engine.predict(
                str(crop_path), use_layout_detection=False,
                prompt_label="ocr", max_new_tokens=MAX_NEW_TOKENS,
            ))
            if len(predictions) != 1:
                raise AutoVLFailure("vendor_result_shape_unsupported")
            result_path = region_dir / "vendor-result.json"
            vendor = _vendor_result(predictions[0], result_path)
            _write_private_json(result_path, vendor)
            data = _result_data(vendor)
            blocks = _raw_blocks(data)
            cap = _cap_reached(vendor)
            if cap is True:
                # A capped generation is incomplete evidence; retain the raw
                # vendor file privately but do not present partial text as OK.
                blocks = []
                status = "error"
                _write_private_json(region_dir / "failure.json", {"error_code": "generation_cap_reached"})
            else:
                status = "ok" if blocks and any(block["text_raw"].strip() for block in blocks) else "empty"
        except Exception as error:
            safe = error.code if isinstance(error, AutoVLFailure) else "reader_region_failed"
            _write_private_json(region_dir / "failure.json", {
                "error_code": safe, "exception_type": type(error).__name__,
                "traceback": traceback.format_exc(),
            })
            status, blocks, cap = "error", [], None
        elapsed = time.monotonic() - started
        if not math.isfinite(elapsed) or elapsed < 0:
            raise AutoVLFailure("elapsed_time_invalid")
        if _sha256_file(crop_path) != before_crop:
            raise AutoVLFailure("crop_changed_during_inference")
        evidence_regions.append({
            "region_id": region["id"], "status": status, "blocks": blocks,
            "region": copy.deepcopy(region),
            "geometry_scope": GEOMETRY_SCOPE, "cap_reached": cap,
            "elapsed_seconds": elapsed,
        })

    # Recheck all data identities after inference before returning evidence.
    if _sha256_file(image_file) != source_sha:
        raise AutoVLFailure("source_image_changed_during_job")
    if frozen_plan["plan_sha256"] != plan_digest:
        raise AutoVLFailure("plan_changed_during_job")
    if _plan_object_snapshot(plan) != input_plan_snapshot:
        raise AutoVLFailure("source_plan_changed_during_job")
    model_after = model_tree_snapshot(model_root)
    if model_before["files_sha256"] != model_after["files_sha256"]:
        raise AutoVLFailure("local_model_tree_changed_during_job")
    evidence = {
        "schema_version": SCHEMA_EVIDENCE,
        "image": copy.deepcopy(frozen_plan["image"]),
        "plan_sha256": plan_digest,
        "reader": reader,
        "regions": evidence_regions,
    }
    _write_private_json(out_dir / "evidence.json", evidence)
    return evidence


__all__ = ["AutoVLFailure", "read_auto_vl_fields", "validate_plan", "model_tree_snapshot"]
