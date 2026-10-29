#!/usr/bin/env python3
"""Posthoc score the completed, four-case native CPU VL ROI control."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
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
import unicodedata
from typing import Any

TASK = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
CONTROL = TASK / "work/ocr-roi-vl-control"
RUNNER = CONTROL / "run_vl_roi_control.py"
RUN_DIR = CONTROL / "private/run"
RUN_MANIFEST = RUN_DIR / "run-manifest.json"
ROI_WORK = TASK / "work/ocr-roi-diagnostic"
ROI_RUNNER = REPO / "ocr/research/tools/ocr-roi-diagnostic/run_roi_diagnostic.py"
CROP_MANIFEST = ROI_WORK / "crops.json"
ROI_RAW_REPORT = TASK / "outputs/ocr-roi-diagnostic/roi-diagnostic-score.json"
ROI_PRIVATE_PINS = ROI_WORK / "posthoc-integrity.json"
BASELINE_MANIFEST = TASK / "work/ocr-five-image-eval/baseline-v2/manifest.json"
BASELINE_AUDIT = TASK / "work/ocr-five-image-eval/baseline-v2-integrity-audit.json"
BASELINE_AUDIT_SHA256 = "9b3b25aa085b18096294cd05c94f50cf58c5c984d6e69edfcff08afc1363a0a3"
OLD_REFERENCE = TASK / "work/ocr-passport-tests/ground-truth.json"
NEW_REFERENCE = TASK / "work/ocr-five-image-eval/reference-new-images.json"
OUTPUT_DIR = TASK / "outputs/ocr-roi-vl-control"
PRIVATE_INTEGRITY = CONTROL / "posthoc-integrity.json"
MODEL_DIR = VL_MODEL_DIR
EXPECTED_CASES = (
    ("sample01", "issuer"),
    ("sample01", "birth_place"),
    ("sample02", "given_name"),
    ("sample04", "issuer"),
)
SAMPLES = ("sample01", "sample02", "sample03", "sample04", "sample05")
FIELDS = ("issuer", "birth_place", "given_name", "gender")
PACKAGE_VERSIONS = {"paddleocr": "3.7.0", "paddlepaddle": "3.2.1", "paddlex": "3.7.2"}
MODEL_NAME = "PaddleOCR-VL-1.6-0.9B"
PIPELINE_VERSION = "v1.6"
GEOMETRY_SCOPE = "whole_crop_synthetic_prompt_box"


class ScoreError(ValueError):
    """Stable error code without OCR content, paths, or reference values."""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _hash_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise ScoreError(code) from error


def _json_bytes(data: bytes, code: str) -> Any:
    try:
        return json.loads(data.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError) as error:
        raise ScoreError(code) from error


def _valid_hash(value: Any) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-fA-F]{64}", value) is not None


def canonical(value: str, field: str) -> str:
    value = " ".join(unicodedata.normalize("NFC", value).strip().split()).casefold()
    if field == "gender":
        return {
            "m": "m", "m.": "m", "м": "m", "м.": "m", "муж": "m", "муж.": "m",
            "f": "f", "f.": "f", "ж": "f", "ж.": "f", "жен": "f", "жен.": "f",
        }.get(value, value)
    return value


def _sample_key(sample_id: str) -> str:
    if not isinstance(sample_id, str) or not re.fullmatch(r"sample0[1-5]", sample_id):
        raise ScoreError("sample_id_invalid")
    return sample_id


def _reference_entry_hashes(entry: Any, source_root: Path) -> set[str]:
    if not isinstance(entry, dict):
        raise ScoreError("reference_entry_invalid")
    hashes: set[str] = set()
    for name in ("source_sha256", "sha256"):
        value = entry.get(name)
        if _valid_hash(value):
            hashes.add(value.lower())
    for name in ("source_image", "image", "source"):
        nested = entry.get(name)
        if isinstance(nested, dict):
            value = nested.get("sha256") or nested.get("source_sha256")
            if _valid_hash(value):
                hashes.add(value.lower())
    for name in ("source_image_path", "source_path", "image_path"):
        value = entry.get(name)
        if isinstance(value, str):
            path = Path(value).expanduser()
            if not path.is_absolute():
                path = source_root / path
            if path.is_file():
                hashes.add(sha256(path.resolve()).lower())
    return hashes


def _index_reference(reference: Any, source_root: Path, source_hashes: dict[str, str],
                     allowed: set[str]) -> dict[str, dict[str, Any]]:
    images = reference.get("images") if isinstance(reference, dict) else None
    if not isinstance(images, (dict, list)):
        raise ScoreError("reference_images_invalid")
    wanted = {source_hashes[sample].lower() for sample in allowed}
    matches: dict[str, list[dict[str, Any]]] = {sample: [] for sample in allowed}
    records = images.items() if isinstance(images, dict) else ((str(index), entry) for index, entry in enumerate(images))
    all_hashes = {value.lower() for value in source_hashes.values()}
    for _key, entry in records:
        hashes = _reference_entry_hashes(entry, source_root)
        if len(hashes) > 1:
            raise ScoreError("reference_entry_hash_ambiguous")
        for digest in hashes:
            if digest in all_hashes and digest not in wanted:
                raise ScoreError("reference_sample_group_mismatch")
            for sample in allowed:
                if digest == source_hashes[sample].lower():
                    matches[sample].append(entry)
    if any(len(entries) != 1 for entries in matches.values()):
        raise ScoreError("reference_hash_mapping_invalid")
    return {sample: entries[0] for sample, entries in matches.items()}


def _expected(reference_field: Any, field: str) -> str:
    if not isinstance(reference_field, dict) or not isinstance(reference_field.get("literal"), str):
        raise ScoreError("reference_field_invalid")
    literal = reference_field["literal"]
    normalized = reference_field.get("normalized")
    if isinstance(normalized, str) and normalized.strip() and canonical(normalized, field) == canonical(literal, field):
        return normalized
    return literal


def _status(value: Any, expected: str, field: str) -> str:
    if not isinstance(value, str) or not value.strip():
        return "abstain"
    return "correct" if canonical(value, field) == canonical(expected, field) else "wrong"


def _load_roi_module():
    if str(REPO) not in sys.path:
        sys.path.insert(0, str(REPO))
    if str(ROI_WORK) not in sys.path:
        sys.path.insert(0, str(ROI_WORK))
    os.environ.setdefault("PASSPORT_BACKEND_REPO_ROOT", str(REPO))
    import run_roi_diagnostic as roi
    return roi


def _load_control_module():
    if str(CONTROL) not in sys.path:
        sys.path.insert(0, str(CONTROL))
    import run_vl_roi_control as control
    return control


def _validate_ready_manifest(path: Path) -> tuple[dict[str, Any], dict[tuple[str, str], dict[str, Any]], dict[str, str], dict[str, Any]]:
    if not path.is_file():
        raise ScoreError("run_manifest_missing")
    manifest_raw = path.read_bytes()
    manifest = _json_bytes(manifest_raw, "run_manifest_invalid")
    if (not isinstance(manifest, dict) or manifest.get("schema_version") != "passport-roi-vl-control/v1"
            or manifest.get("purpose") != "four_fixed_manual_roi_diagnostic_only"
            or manifest.get("status") != "completed" or manifest.get("error_code") is not None
            or manifest.get("reference_read") is not False or manifest.get("gold_read") is not False):
        raise ScoreError("run_not_completed_or_reference_boundary_invalid")
    config = manifest.get("configured")
    if (not isinstance(config, dict) or config.get("pipeline_version") != PIPELINE_VERSION
            or config.get("model_name") != MODEL_NAME or config.get("backend") != "native"
            or config.get("device") != "cpu" or config.get("use_layout_detection") is not False
            or config.get("prompt_label") != "ocr" or config.get("max_new_tokens") != 512
            or config.get("use_doc_orientation_classify") is not False
            or config.get("use_doc_unwarping") is not False
            or config.get("geometry_scope") != GEOMETRY_SCOPE):
        raise ScoreError("configured_control_mismatch")
    runtime = manifest.get("runtime")
    if (not isinstance(runtime, dict) or runtime.get("sys_prefix_matches") is not True
            or runtime.get("packages") != PACKAGE_VERSIONS):
        raise ScoreError("runtime_identity_invalid")

    source_before, source_after = manifest.get("source_pins_before"), manifest.get("source_pins_after")
    if not isinstance(source_before, dict) or source_before != source_after:
        raise ScoreError("source_pin_before_after_mismatch")
    expected_files = {
        "runner_sha256": RUNNER,
        "roi_helper_sha256": ROI_RUNNER,
        "path_helper_sha256": REPO / "ocr/experiments/local_paths.py",
        "crop_manifest_sha256": CROP_MANIFEST,
    }
    for key, file_path in expected_files.items():
        if not _valid_hash(source_before.get(key)) or not file_path.is_file() or sha256(file_path) != source_before[key]:
            raise ScoreError("source_pin_current_hash_mismatch")
    if source_before.get("crop_manifest_sha256") != sha256(CROP_MANIFEST):
        raise ScoreError("crop_manifest_pin_mismatch")

    model_before, model_after = manifest.get("model_tree_before"), manifest.get("model_tree_after")
    if (not isinstance(model_before, dict) or not isinstance(model_after, dict)
            or model_before.get("files_sha256") != model_after.get("files_sha256")):
        raise ScoreError("model_tree_before_after_mismatch")
    control = _load_control_module()
    try:
        model_now = control._model_tree_snapshot(MODEL_DIR)
    except Exception as error:
        raise ScoreError("model_tree_current_validation_failed") from error
    if model_now != model_before or model_now != model_after:
        raise ScoreError("model_tree_current_hash_mismatch")

    bindings = manifest.get("cases")
    if not isinstance(bindings, list) or len(bindings) != 4:
        raise ScoreError("manifest_case_count_invalid")
    bound: dict[tuple[str, str], dict[str, Any]] = {}
    for binding in bindings:
        if not isinstance(binding, dict):
            raise ScoreError("manifest_case_binding_invalid")
        key = (binding.get("sample_id"), binding.get("field_id"))
        if key not in EXPECTED_CASES or key in bound:
            raise ScoreError("manifest_case_set_invalid")
        bound[key] = binding
    if set(bound) != set(EXPECTED_CASES):
        raise ScoreError("manifest_case_set_invalid")

    worker_statuses = manifest.get("worker_statuses")
    if not isinstance(worker_statuses, list) or len(worker_statuses) != len(EXPECTED_CASES):
        raise ScoreError("worker_status_count_invalid")
    expected_case_ids = {f"{sample}-{field}" for sample, field in EXPECTED_CASES}
    status_by_id: dict[str, dict[str, Any]] = {}
    for status in worker_statuses:
        if (not isinstance(status, dict) or status.get("status") != "completed"
                or status.get("case_id") not in expected_case_ids or status.get("case_id") in status_by_id):
            raise ScoreError("worker_status_set_or_completion_invalid")
        result_hashes = status.get("result_sha256")
        if (not isinstance(result_hashes, dict)
                or not all(_valid_hash(result_hashes.get(name)) for name in ("vendor_result_sha256", "case_result_sha256"))):
            raise ScoreError("worker_result_hashes_invalid")
        status_by_id[status["case_id"]] = status
    if set(status_by_id) != expected_case_ids:
        raise ScoreError("worker_status_set_or_completion_invalid")

    # Input image/crop identity is revalidated before any OCR-result JSON is opened.
    roi = _load_roi_module()
    crop_items = roi._load_manifest(CROP_MANIFEST)
    control = _load_control_module()
    selected = control._select_cases(crop_items)
    if set(selected) != set(EXPECTED_CASES):
        raise ScoreError("crop_manifest_case_set_invalid")
    source_hashes: dict[str, str] = {}
    for item in crop_items:
        sample_id = _sample_key(item.get("sample_id"))
        previous = source_hashes.setdefault(sample_id, item["source_sha256"])
        if previous != item["source_sha256"]:
            raise ScoreError("sample_source_sha_mismatch")
    if set(source_hashes) != set(SAMPLES):
        raise ScoreError("crop_source_sample_set_invalid")
    validated_inputs: dict[tuple[str, str], dict[str, Any]] = {}
    for key in EXPECTED_CASES:
        item = selected[key]
        source_path, crop_path, _rgb, actual_binding = roi._validate_input(item, CROP_MANIFEST)
        binding = bound[key]
        safe_binding = {
            "sample_id": item["sample_id"], "field_id": item["field_id"],
            "source_sha256": actual_binding["source_sha256"], "crop_sha256": actual_binding["crop_sha256"],
            "source_dimensions_exif_normalized": actual_binding["source_dimensions_exif_normalized"],
            "crop_dimensions_exif_normalized": actual_binding["crop_dimensions_exif_normalized"],
            "crop_box_source_xyxy": actual_binding["crop_box_source_xyxy"],
            "geometry_provenance": actual_binding["geometry_provenance"],
        }
        if binding != safe_binding:
            raise ScoreError("case_source_crop_binding_mismatch")
        if source_hashes[item["sample_id"]] != item["source_sha256"]:
            raise ScoreError("sample_source_sha_mismatch")
        validated_inputs[key] = {"source_path": source_path, "crop_path": crop_path,
                                 "source_sha256": item["source_sha256"], "crop_sha256": item["crop_sha256"]}
    # Ensure all four result files match manifest hashes before parsing any OCR text.
    case_files: dict[tuple[str, str], tuple[Path, Path]] = {}
    for sample, field in EXPECTED_CASES:
        case_id = f"{sample}-{field}"
        case_dir = RUN_DIR / case_id
        vendor_path, case_path = case_dir / "vendor-result.json", case_dir / "case-result.json"
        expected_hashes = status_by_id[case_id]["result_sha256"]
        if (not vendor_path.is_file() or not case_path.is_file()
                or sha256(vendor_path) != expected_hashes["vendor_result_sha256"]
                or sha256(case_path) != expected_hashes["case_result_sha256"]):
            raise ScoreError("case_result_hash_mismatch")
        case_files[(sample, field)] = (vendor_path, case_path)

    if _hash_bytes(path.read_bytes()) != _hash_bytes(manifest_raw):
        raise ScoreError("run_manifest_changed_during_validation")
    return manifest, bound, source_hashes, {"control": control, "roi": roi,
        "selected": selected, "validated_inputs": validated_inputs, "case_files": case_files,
        "status_by_id": status_by_id, "model_snapshot": model_now,
        "bindings": bound, "run_manifest_sha256": _hash_bytes(manifest_raw)}


def _load_reference_data(source_hashes: dict[str, str]) -> dict[str, dict[str, Any]]:
    pins = _json(ROI_PRIVATE_PINS, "roi_reference_pins_missing")
    if pins.get("schema_version") != "passport-roi-diagnostic-private-integrity/v1":
        raise ScoreError("roi_reference_pins_invalid")
    ref_pins = pins.get("reference_files_sha256")
    if not isinstance(ref_pins, dict):
        raise ScoreError("roi_reference_pins_invalid")
    old_raw, new_raw = OLD_REFERENCE.read_bytes(), NEW_REFERENCE.read_bytes()
    if (_hash_bytes(old_raw) != ref_pins.get("old_sha256")
            or _hash_bytes(new_raw) != ref_pins.get("new_sha256")):
        raise ScoreError("reference_sha_pin_mismatch")
    old, new = _json_bytes(old_raw, "old_reference_invalid"), _json_bytes(new_raw, "new_reference_invalid")
    refs = _index_reference(old, OLD_REFERENCE.parent, source_hashes, {"sample01", "sample02"})
    refs |= _index_reference(new, NEW_REFERENCE.parent, source_hashes, {"sample03", "sample04", "sample05"})
    return refs


def _load_prior_reader_statuses() -> dict[tuple[str, str], dict[str, str]]:
    report = _json(ROI_RAW_REPORT, "roi_reader_score_missing")
    if report.get("schema_version") != "passport-roi-diagnostic-score/v1" or report.get("status") != "completed":
        raise ScoreError("roi_reader_score_invalid")
    rows = {}
    for row in report.get("samples", []):
        if not isinstance(row, dict):
            raise ScoreError("roi_reader_score_row_invalid")
        key = (row.get("sample_id"), row.get("field_id"))
        if key in rows:
            raise ScoreError("roi_reader_score_duplicate")
        statuses = {name: row.get(name) for name in ("rapidocr", "tesseract", "baseline_go")}
        if any(value not in {"correct", "wrong", "abstain"} for value in statuses.values()):
            raise ScoreError("roi_reader_score_status_invalid")
        rows[key] = statuses
    if not set(EXPECTED_CASES).issubset(rows):
        raise ScoreError("roi_reader_score_case_missing")
    return rows


def _load_baseline_extractions() -> dict[str, dict[str, Any]]:
    if (BASELINE_MANIFEST.resolve() != (TASK / "work/ocr-five-image-eval/baseline-v2/manifest.json").resolve()
            or BASELINE_AUDIT.resolve() != (TASK / "work/ocr-five-image-eval/baseline-v2-integrity-audit.json").resolve()
            or not BASELINE_AUDIT.is_file() or sha256(BASELINE_AUDIT) != BASELINE_AUDIT_SHA256):
        raise ScoreError("historical_baseline_audit_pin_mismatch")
    scorer_path = REPO / "ocr/research/tools/ocr-source-tiles/score_source_tile_comparison.py"
    if not scorer_path.is_file():
        raise ScoreError("historical_baseline_guard_missing")
    import importlib.util
    spec = importlib.util.spec_from_file_location("source_tile_historical_baseline_guard", scorer_path)
    if spec is None or spec.loader is None:
        raise ScoreError("historical_baseline_guard_unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    try:
        manifest, paths = module.verify_historical_baseline()
    except Exception as error:
        raise ScoreError("historical_baseline_guard_failed") from error
    if manifest.get("schema_version") != "passport-five-image-inference/v1":
        raise ScoreError("historical_baseline_schema_invalid")
    rows = {}
    for number in range(1, 6):
        sample = f"sample-{number:02d}"
        extraction_path = paths.get(sample, {}).get("go-extraction.json")
        if not isinstance(extraction_path, Path):
            raise ScoreError("historical_baseline_extraction_missing")
        extraction = _json(extraction_path, "historical_baseline_extraction_invalid")
        if not isinstance(extraction, dict):
            raise ScoreError("historical_baseline_extraction_invalid")
        rows[sample] = extraction
    return rows


def _case_text(case_result: Any, vendor_result: Any, control: Any,
               expected_model_tree_sha256: str) -> tuple[str, bool | None]:
    if not isinstance(case_result, dict) or not isinstance(vendor_result, dict):
        raise ScoreError("case_result_root_invalid")
    if (case_result.get("schema_version") != "passport-roi-vl-control-case/v1"
            or case_result.get("geometry_scope") != GEOMETRY_SCOPE
            or case_result.get("geometry_note") != "layout disabled; PaddleX supplies one synthetic whole-crop prompt box; no word geometry is claimed"):
        raise ScoreError("case_geometry_scope_invalid")
    runtime = case_result.get("runtime")
    settings = runtime.get("observed_model_settings") if isinstance(runtime, dict) else None
    if (not isinstance(runtime, dict) or runtime.get("device") != "cpu" or runtime.get("cuda_compiled") is not False
            or runtime.get("pipeline_version") != PIPELINE_VERSION or runtime.get("model_name") != MODEL_NAME
            or runtime.get("model_tree_sha256") != expected_model_tree_sha256
            or not isinstance(settings, dict) or settings.get("use_layout_detection") is not False
            or settings.get("use_doc_preprocessor") is not False
            or case_result.get("model_init_seconds") is None or case_result.get("predict_seconds") is None
            or case_result.get("generated_length_cap") != 512):
        raise ScoreError("case_runtime_or_layout_settings_invalid")
    data = vendor_result.get("res") if set(vendor_result) == {"res"} and isinstance(vendor_result.get("res"), dict) else vendor_result
    vendor_settings = data.get("model_settings") if isinstance(data, dict) else None
    if (not isinstance(vendor_settings, dict) or vendor_settings.get("use_layout_detection") is not False
            or vendor_settings.get("use_doc_preprocessor") is not False):
        raise ScoreError("vendor_observed_settings_invalid")
    try:
        vendor_blocks = control._candidate_blocks(vendor_result)
    except Exception as error:
        raise ScoreError("vendor_blocks_invalid") from error
    if case_result.get("candidate_blocks_in_vendor_order") != vendor_blocks:
        raise ScoreError("vendor_case_candidate_mismatch")
    if case_result.get("vendor_result") != vendor_result:
        raise ScoreError("vendor_case_result_mismatch")
    cap_status = case_result.get("generated_length_at_or_above_cap")
    if cap_status not in {True, False, None}:
        raise ScoreError("generation_cap_status_invalid")
    # Preserve every vendor block and their order. No Markdown removal, token
    # filtering, geometry selection, or gold-dependent block choice is applied.
    return " ".join(block["block_content"] for block in vendor_blocks), cap_status


def _write_private(path: Path, text: str) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(text)


def _score(manifest: dict[str, Any], meta: dict[str, Any], source_hashes: dict[str, str],
           baseline_extractions: dict[str, dict[str, Any]]) -> dict[str, Any]:
    refs = _load_reference_data(source_hashes)
    old_report = _load_prior_reader_statuses()
    if set(baseline_extractions) != {f"sample-{number:02d}" for number in range(1, 6)}:
        raise ScoreError("historical_baseline_sample_set_invalid")
    rows = []
    counts = {reader: {state: 0 for state in ("correct", "wrong", "abstain")}
              for reader in ("paddleocr_vl", "rapidocr_ppocrv5", "tesseract_rus", "baseline_go")}
    for sample, field in EXPECTED_CASES:
        reference = refs[sample].get("fields")
        if not isinstance(reference, dict):
            raise ScoreError("reference_fields_missing")
        expected = _expected(reference.get(field), field)
        vendor_path, case_path = meta["case_files"][(sample, field)]
        vendor_result = _json(vendor_path, "vendor_result_invalid")
        case_result = _json(case_path, "case_result_invalid")
        if (case_result.get("sample_id") != sample or case_result.get("field_id") != field
                or case_result.get("input_identity") != meta["bindings"][(sample, field)]):
            raise ScoreError("case_identity_or_input_binding_mismatch")
        case_text, cap_status = _case_text(
            case_result, vendor_result, meta["control"], meta["model_snapshot"]["tree_sha256"],
        )
        vlm_status = _status(case_text, expected, field)
        prior = old_report[(sample, field)]
        baseline_sample = f"sample-{int(sample[-2:]):02d}"
        baseline_fields = baseline_extractions[baseline_sample].get("fields")
        if not isinstance(baseline_fields, dict):
            raise ScoreError("baseline_field_map_invalid")
        baseline_field = baseline_fields.get(field)
        baseline_value = baseline_field.get("value") if isinstance(baseline_field, dict) else None
        baseline_status = _status(baseline_value, expected, field)
        if baseline_status != prior["baseline_go"]:
            raise ScoreError("baseline_status_parity_mismatch")
        row = {
            "sample_id": sample,
            "field_id": field,
            "paddleocr_vl": vlm_status,
            "rapidocr_ppocrv5_raw": prior["rapidocr"],
            "tesseract_rus_raw": prior["tesseract"],
            "baseline_go_selected": baseline_status,
            "generated_length_at_or_above_512": cap_status,
        }
        rows.append(row)
        counts["paddleocr_vl"][vlm_status] += 1
        counts["rapidocr_ppocrv5"][prior["rapidocr"]] += 1
        counts["tesseract_rus"][prior["tesseract"]] += 1
        counts["baseline_go"][baseline_status] += 1
    return {
        "schema_version": "passport-roi-vl-control-score/v1",
        "status": "completed",
        "dataset_role": "four_fixed_manual_rois_from_five_known_development_photos",
        "method": "whole-field exact after NFC, case-folding, whitespace normalization, and closed gender labels",
        "control": {
            "model": MODEL_NAME, "pipeline_version": PIPELINE_VERSION,
            "runtime": "native CPU", "layout_detection": False,
            "document_preprocessor": False, "prompt_label": "ocr",
            "geometry_scope": "synthetic whole-crop prompt box; no word geometry claimed",
            "candidate_policy": "all vendor blocks joined in original order; no block/word selection or Markdown stripping",
        },
        "limitations": [
            "Four manually selected ROIs are diagnostic only and do not establish held-out or production accuracy.",
            "These four field/photo pairs do not estimate end-to-end accuracy over the 50 required-field slots.",
            "RapidOCR/Tesseract counts are standalone raw-reader strings; baseline Go is its selected full-pipeline value.",
            "A null generation-length flag means the serializer did not expose that metadata; it does not guarantee an uncapped full-length response.",
        ],
        "cases": rows,
        "counts": counts,
    }


def _markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Four-case PaddleOCR-VL ROI control", "",
        "Diagnostic only: four manually selected crops from five known development photos. All vendor text blocks are joined in original order and scored as one whole field; no word geometry or text selection is claimed.", "",
        "| Sample | Field | PaddleOCR-VL | RapidOCR PP-OCRv5 raw | Tesseract `rus` raw | Baseline Go selected | Cap flag |",
        "|---|---|---|---|---|---|---|",
    ]
    for row in report["cases"]:
        cap_flag = "null" if row["generated_length_at_or_above_512"] is None else str(row["generated_length_at_or_above_512"]).lower()
        lines.append(f"| {row['sample_id']} | {row['field_id']} | {row['paddleocr_vl']} | {row['rapidocr_ppocrv5_raw']} | {row['tesseract_rus_raw']} | {row['baseline_go_selected']} | {cap_flag} |")
    lines += ["", "Counts (correct / wrong / abstain):", "", "| Output | Correct | Wrong | Abstain |", "|---|---:|---:|---:|"]
    labels = {"paddleocr_vl": "PaddleOCR-VL", "rapidocr_ppocrv5": "RapidOCR PP-OCRv5 raw",
              "tesseract_rus": "Tesseract rus raw", "baseline_go": "Baseline Go selected"}
    for reader, values in report["counts"].items():
        lines.append(f"| {labels[reader]} | {values['correct']} | {values['wrong']} | {values['abstain']} |")
    lines += ["", "The existing baseline and standalone OCR outputs are included for the same four field/photo pairs. These different output stages are not interchangeable model benchmarks.",
              "", "These four field/photo pairs do not estimate end-to-end accuracy over the 50 required-field slots.",
              "", "A null cap flag means the serializer exposed no token count; output length is not inferred from text, and null does not guarantee a full-length response."]
    return "\n".join(lines) + "\n"


def run(run_manifest: Path = RUN_MANIFEST, output_dir: Path = OUTPUT_DIR) -> dict[str, Any]:
    # No reference file or OCR-result JSON is read until the full run, fixed
    # four-case set, source pins, model tree, crop bindings, and file hashes pass.
    manifest, bindings, source_hashes, meta = _validate_ready_manifest(run_manifest.resolve())
    if (OLD_REFERENCE.resolve() != (TASK / "work/ocr-passport-tests/ground-truth.json").resolve()
            or NEW_REFERENCE.resolve() != (TASK / "work/ocr-five-image-eval/reference-new-images.json").resolve()):
        raise ScoreError("reference_profile_path_mismatch")
    if output_dir.exists() or PRIVATE_INTEGRITY.exists():
        raise ScoreError("output_already_exists")
    baseline_extractions = _load_baseline_extractions()
    report = _score(manifest, meta, source_hashes, baseline_extractions)
    # Recheck inputs, source pins, model files, manifest and per-case hashes
    # immediately before writing any report.
    if sha256(run_manifest) != meta["run_manifest_sha256"]:
        raise ScoreError("run_manifest_changed_during_score")
    for key, item in meta["selected"].items():
        paths = meta["validated_inputs"][key]
        if (sha256(paths["source_path"]) != paths["source_sha256"]
                or sha256(paths["crop_path"]) != paths["crop_sha256"]):
            raise ScoreError("input_changed_during_score")
    current_model = meta["control"]._model_tree_snapshot(MODEL_DIR)
    if current_model != meta["model_snapshot"]:
        raise ScoreError("model_changed_during_score")
    current_source_pins = meta["control"]._source_file_pins()
    if current_source_pins != manifest["source_pins_before"] or current_source_pins != manifest["source_pins_after"]:
        raise ScoreError("source_pin_changed_during_score")
    for (sample, field), case_paths in meta["case_files"].items():
        expected_hashes = meta["status_by_id"][f"{sample}-{field}"]["result_sha256"]
        if (sha256(case_paths[0]) != expected_hashes["vendor_result_sha256"]
                or sha256(case_paths[1]) != expected_hashes["case_result_sha256"]):
            raise ScoreError("case_result_changed_during_score")
    ref_pins = _json(ROI_PRIVATE_PINS, "roi_reference_pins_missing").get("reference_files_sha256", {})
    if (sha256(OLD_REFERENCE) != ref_pins.get("old_sha256")
            or sha256(NEW_REFERENCE) != ref_pins.get("new_sha256")):
        raise ScoreError("reference_changed_during_score")
    output_dir.mkdir(parents=True, mode=0o700)
    output_dir.chmod(0o700)
    _write_private(output_dir / "vl-roi-control-score.json", json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    _write_private(output_dir / "vl-roi-control-score.md", _markdown(report))
    _write_private(PRIVATE_INTEGRITY, json.dumps({
        "schema_version": "passport-roi-vl-control-private-integrity/v1",
        "run_manifest_sha256": sha256(run_manifest),
        "crop_manifest_sha256": sha256(CROP_MANIFEST),
        "case_result_sha256": {
            case_id: {"vendor_result_sha256": sha256(RUN_DIR / case_id / "vendor-result.json"),
                      "case_result_sha256": sha256(RUN_DIR / case_id / "case-result.json")}
            for case_id in sorted(f"{sample}-{field}" for sample, field in EXPECTED_CASES)
        },
        "reference_files_sha256": {"old": sha256(OLD_REFERENCE), "new": sha256(NEW_REFERENCE)},
        "model_tree_sha256": meta["model_snapshot"]["tree_sha256"],
    }, ensure_ascii=False, indent=2) + "\n")
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-manifest", type=Path, default=RUN_MANIFEST)
    parser.add_argument("--output-dir", type=Path, default=OUTPUT_DIR)
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        report = run(args.run_manifest.expanduser().resolve(), args.output_dir.expanduser().resolve())
        print(f"completed status=completed cases={len(report['cases'])}")
        return 0
    except ScoreError as error:
        print(f"failed status={error}", file=sys.stderr)
        return 2
    except Exception:
        print("failed status=unexpected_internal_error", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
