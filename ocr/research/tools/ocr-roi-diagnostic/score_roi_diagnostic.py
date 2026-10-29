#!/usr/bin/env python3
"""Private posthoc scorer for the completed 14-task manual-ROI diagnostic.

The result, crop manifest, input images, runner, helper code and model pins are
validated before either reference file is opened. Public output contains only
sample IDs, field IDs, statuses and counts; OCR/reference text and paths stay
out of the report.
"""
from __future__ import annotations

import argparse
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
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
import unicodedata
from typing import Any

TASK_ROOT = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
RUNNER = REPO_ROOT / "ocr/research/tools/ocr-roi-diagnostic/run_roi_diagnostic.py"
RESULT_SCHEMA = "passport-roi-diagnostic-results/v1"
CROP_SCHEMA = "passport-roi-diagnostic-crops/v1"
PURPOSE = "manual_visual_diagnostic_only"
FIELDS = ("issuer", "birth_place", "given_name", "gender")
SAMPLES = ("sample01", "sample02", "sample03", "sample04", "sample05")
EXPECTED_TASKS = {
    *((sample, field) for sample in SAMPLES for field in ("issuer", "birth_place")),
    *((sample, field) for sample in ("sample02", "sample04") for field in ("given_name", "gender")),
}
SAMPLE_TO_BASELINE = {f"sample{index:02d}": f"sample-{index:02d}" for index in range(1, 6)}
TESSERACT = Path(os.environ.get("PASSPORT_TESSERACT_BIN", shutil.which("tesseract") or "tesseract"))
DEFAULT_TESSDATA = REPO / "ocr/experiments/models/tessdata_fast-4.1.0"
BASELINE_DIR = TASK_ROOT / "work/ocr-five-image-eval/baseline-v2"
BASELINE_MANIFEST = BASELINE_DIR / "manifest.json"
BASELINE_AUDIT = TASK_ROOT / "work/ocr-five-image-eval/baseline-v2-integrity-audit.json"
BASELINE_AUDIT_SHA256 = "9b3b25aa085b18096294cd05c94f50cf58c5c984d6e69edfcff08afc1363a0a3"
POSTHOC_INTEGRITY = TASK_ROOT / "work/ocr-roi-diagnostic/posthoc-integrity.json"
NORMALIZATION_INTEGRITY = TASK_ROOT / "work/ocr-roi-diagnostic/production-normalization-integrity.json"
VISUAL_LATIN_TO_CYR = {
    "A": "А", "B": "В", "C": "С", "E": "Е", "H": "Н", "K": "К",
    "M": "М", "O": "О", "P": "Р", "T": "Т", "X": "Х", "Y": "У",
}


class ScoreError(ValueError):
    """Safe error containing a stable code only."""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ScoreError(code) from exc


def _read_reference(path: Path, code: str) -> tuple[Any, str]:
    try:
        raw = path.read_bytes()
        parsed = json.loads(raw.decode("utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ScoreError(code) from exc
    return parsed, hashlib.sha256(raw).hexdigest()


def canonical(value: str, field: str) -> str:
    """Use conservative existing normalization; only gender has a closed map."""
    result = " ".join(unicodedata.normalize("NFC", value).strip().split()).casefold()
    if field == "gender":
        return {
            "m": "m", "m.": "m", "м": "m", "м.": "m", "муж": "m", "муж.": "m",
            "f": "f", "f.": "f", "ж": "f", "ж.": "f", "жен": "f", "жен.": "f",
        }.get(result, result)
    return result


def production_visual_canonical(value: str, field: str) -> str:
    """Apply only passport-backend's existing uppercase visual-letter map."""
    mapped = "".join(VISUAL_LATIN_TO_CYR.get(character, character) for character in value)
    return canonical(mapped, field)


def _edit_metrics(expected: str, observed: str) -> tuple[int, int, int]:
    """Return edit distance, missing expected chars and extra observed chars."""
    rows, columns = len(expected) + 1, len(observed) + 1
    costs = [[0] * columns for _ in range(rows)]
    missing = [[0] * columns for _ in range(rows)]
    extra = [[0] * columns for _ in range(rows)]
    for i in range(1, rows):
        costs[i][0], missing[i][0] = i, i
    for j in range(1, columns):
        costs[0][j], extra[0][j] = j, j
    for i in range(1, rows):
        for j in range(1, columns):
            substitution = 0 if expected[i - 1] == observed[j - 1] else 1
            candidates = [
                (costs[i - 1][j - 1] + substitution,
                 missing[i - 1][j - 1], extra[i - 1][j - 1]),
                (costs[i - 1][j] + 1, missing[i - 1][j] + 1, extra[i - 1][j]),
                (costs[i][j - 1] + 1, missing[i][j - 1], extra[i][j - 1] + 1),
            ]
            costs[i][j], missing[i][j], extra[i][j] = min(candidates, key=lambda item: item[0])
    return costs[-1][-1], missing[-1][-1], extra[-1][-1]


def _valid_hash(value: Any) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-fA-F]{64}", value) is not None


def _resolve(manifest_path: Path, value: str) -> Path:
    candidate = Path(value).expanduser()
    return candidate.resolve() if candidate.is_absolute() else (manifest_path.parent / candidate).resolve()


def _load_and_validate_runtime(result_path: Path, manifest_path: Path, *,
                               runner_path: Path = RUNNER,
                               repo: Path = REPO,
                               tessdata_dir: Path = DEFAULT_TESSDATA) -> tuple[dict[str, Any], dict[str, Any], dict[tuple[str, str], dict[str, Any]]]:
    """Validate every readiness and integrity condition without touching gold."""
    result_path, manifest_path = result_path.resolve(), manifest_path.resolve()
    if not result_path.is_file() or not manifest_path.is_file():
        raise ScoreError("runtime_artifact_missing")
    manifest_raw = manifest_path.read_bytes()
    try:
        manifest = json.loads(manifest_raw.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise ScoreError("crop_manifest_invalid") from exc
    if (not isinstance(manifest, dict) or manifest.get("schema_version") != CROP_SCHEMA
            or manifest.get("purpose") != PURPOSE):
        raise ScoreError("crop_manifest_schema_or_purpose_invalid")
    crops = manifest.get("crops")
    if not isinstance(crops, list) or len(crops) != 14:
        raise ScoreError("crop_manifest_task_count_invalid")
    crop_by_key: dict[tuple[str, str], dict[str, Any]] = {}
    for crop in crops:
        if not isinstance(crop, dict):
            raise ScoreError("crop_manifest_entry_invalid")
        key = (crop.get("sample_id"), crop.get("field_id"))
        if key not in EXPECTED_TASKS or key in crop_by_key:
            raise ScoreError("crop_manifest_task_set_invalid")
        for key_name in ("source_sha256", "crop_sha256"):
            if not _valid_hash(crop.get(key_name)):
                raise ScoreError("crop_manifest_hash_invalid")
        for key_name in ("source_image_path", "crop_image_path"):
            if not isinstance(crop.get(key_name), str) or not crop[key_name]:
                raise ScoreError("crop_manifest_path_invalid")
        crop_by_key[key] = crop
    if set(crop_by_key) != EXPECTED_TASKS:
        raise ScoreError("crop_manifest_task_set_invalid")

    result_raw = result_path.read_bytes()
    try:
        result = json.loads(result_raw.decode("utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ScoreError("runtime_result_invalid") from exc
    if (not isinstance(result, dict) or result.get("schema_version") != RESULT_SCHEMA
            or result.get("purpose") != PURPOSE or result.get("task_count") != 14
            or result.get("manifest_sha256") != hashlib.sha256(manifest_raw).hexdigest()):
        raise ScoreError("runtime_result_not_ready_or_manifest_mismatch")
    if result.get("posthoc_interpretation") != "none; raw reader outputs only; no gold/reference text loaded":
        raise ScoreError("runtime_reference_boundary_invalid")
    tasks = result.get("tasks")
    if not isinstance(tasks, list) or len(tasks) != 14:
        raise ScoreError("runtime_task_count_invalid")

    runtime = result.get("runtime")
    if not isinstance(runtime, dict):
        raise ScoreError("runtime_integrity_missing")
    for runtime_key, path in (
        ("runner_sha256", runner_path),
        ("reader_helper_sha256", repo / "ocr/experiments/field_roi_pipeline.py"),
        ("template_helper_sha256", repo / "ocr/experiments/template_field_pipeline.py"),
        ("path_helper_sha256", repo / "ocr/experiments/local_paths.py"),
    ):
        expected = runtime.get(runtime_key)
        if not _valid_hash(expected) or not path.is_file() or sha256(path) != expected:
            raise ScoreError("runtime_code_integrity_mismatch")

    # Match both pinned digests and current bytes, so a self-consistent but
    # unexpected model cannot pass merely by being named in the result.
    sys.path.insert(0, str(repo)) if str(repo) not in sys.path else None
    try:
        from ocr.experiments import field_roi_pipeline as roi
    except Exception as exc:
        raise ScoreError("runtime_model_helper_unavailable") from exc
    model_dir = repo / "ocr/experiments/models"
    expected_models = {
        "detector": (model_dir / roi.MODEL_FILENAMES["detector"], roi.DETECTOR_SHA256),
        "recognizer": (model_dir / roi.MODEL_FILENAMES["recognizer"], roi.RECOGNIZER_SHA256),
    }
    model_pins = runtime.get("rapidocr_model_sha256")
    if not isinstance(model_pins, dict):
        raise ScoreError("runtime_model_integrity_mismatch")
    for name, (path, pinned) in expected_models.items():
        if model_pins.get(name) != pinned or not path.is_file() or sha256(path) != pinned:
            raise ScoreError("runtime_model_integrity_mismatch")

    tess_hash = runtime.get("tesseract_binary_sha256")
    data_hash = runtime.get("tessdata_rus_sha256")
    traineddata = tessdata_dir / "rus.traineddata"
    if (runtime.get("tesseract_version") != "tesseract 5.5.3"
            or not _valid_hash(tess_hash) or not TESSERACT.is_file() or sha256(TESSERACT) != tess_hash
            or not _valid_hash(data_hash) or not traineddata.is_file() or sha256(traineddata) != data_hash):
        raise ScoreError("runtime_tesseract_integrity_mismatch")

    bound: dict[tuple[str, str], dict[str, Any]] = {}
    for task in tasks:
        if not isinstance(task, dict):
            raise ScoreError("runtime_task_invalid")
        key = (task.get("sample_id"), task.get("field_id"))
        if key not in EXPECTED_TASKS or key in bound or task.get("status") != "completed":
            raise ScoreError("runtime_task_set_or_completion_invalid")
        crop = crop_by_key[key]
        binding = task.get("input_binding")
        if not isinstance(binding, dict):
            raise ScoreError("runtime_input_binding_missing")
        source_path = _resolve(manifest_path, crop["source_image_path"])
        crop_path = _resolve(manifest_path, crop["crop_image_path"])
        if (binding.get("source_path") != str(source_path) or binding.get("crop_path") != str(crop_path)
                or binding.get("source_sha256") != crop["source_sha256"]
                or binding.get("crop_sha256") != crop["crop_sha256"]):
            raise ScoreError("runtime_input_binding_mismatch")
        for path, expected in ((source_path, crop["source_sha256"]), (crop_path, crop["crop_sha256"])):
            if not path.is_file() or sha256(path) != expected:
                raise ScoreError("runtime_input_hash_mismatch")
        bound[key] = task
    if set(bound) != EXPECTED_TASKS:
        raise ScoreError("runtime_task_set_or_completion_invalid")
    # Recheck the two sealed metadata files after validating their dependencies.
    if (sha256(manifest_path) != hashlib.sha256(manifest_raw).hexdigest()
            or sha256(result_path) != hashlib.sha256(result_raw).hexdigest()):
        raise ScoreError("runtime_artifact_changed_during_validation")
    return result, manifest, bound


def _reference_hashes(entry: Any, source_root: Path, key: str) -> set[str]:
    if not isinstance(entry, dict):
        raise ScoreError("reference_entry_invalid")
    hashes: set[str] = set()
    for hash_key in ("source_sha256", "sha256"):
        value = entry.get(hash_key)
        if _valid_hash(value):
            hashes.add(value)
    for nested_key in ("source_image", "image", "source"):
        nested = entry.get(nested_key)
        if isinstance(nested, dict):
            value = nested.get("sha256") or nested.get("source_sha256")
            if _valid_hash(value):
                hashes.add(value)
    for path_key in ("source_image_path", "source_path", "image_path"):
        value = entry.get(path_key)
        if isinstance(value, str):
            path = Path(value).expanduser()
            if not path.is_absolute():
                path = source_root / path
            if path.is_file():
                hashes.add(sha256(path.resolve()))
    return hashes


def index_reference(reference: Any, source_root: Path, source_hashes: dict[str, str],
                    allowed_samples: set[str]) -> dict[str, dict[str, Any]]:
    images = reference.get("images") if isinstance(reference, dict) else None
    if not isinstance(images, (dict, list)):
        raise ScoreError("reference_images_invalid")
    needed = {source_hashes[sample] for sample in allowed_samples}
    if len(needed) != len(allowed_samples):
        raise ScoreError("reference_source_hash_not_unique")
    matches: dict[str, list[dict[str, Any]]] = {sample: [] for sample in allowed_samples}
    records = images.items() if isinstance(images, dict) else ((str(i), item) for i, item in enumerate(images))
    for key, entry in records:
        hashes = _reference_hashes(entry, source_root, str(key))
        if len(hashes) > 1:
            raise ScoreError("reference_entry_hash_ambiguous")
        for digest in hashes:
            digest = digest.lower()
            if digest in {value.lower() for value in source_hashes.values()} and digest not in {source_hashes[sample].lower() for sample in allowed_samples}:
                raise ScoreError("reference_contains_wrong_sample_group")
            for sample in allowed_samples:
                if source_hashes[sample].lower() == digest:
                    matches[sample].append(entry)
    if any(len(entries) != 1 for entries in matches.values()):
        raise ScoreError("reference_hash_mapping_not_unique")
    return {sample: entries[0] for sample, entries in matches.items()}


def _expected(reference_field: Any, field: str) -> str:
    if not isinstance(reference_field, dict) or not isinstance(reference_field.get("literal"), str):
        raise ScoreError("reference_field_invalid")
    literal = reference_field["literal"]
    normalized = reference_field.get("normalized")
    if isinstance(normalized, str) and normalized.strip() and canonical(normalized, field) == canonical(literal, field):
        return normalized
    return literal


def _status(raw: Any, reader_status: Any, expected: str, field: str) -> str:
    if reader_status != "ok" or not isinstance(raw, str) or not raw.strip():
        return "abstain"
    return "correct" if canonical(raw, field) == canonical(expected, field) else "wrong"


def verify_historical_baseline() -> tuple[dict[str, Any], dict[str, dict[str, Path]]]:
    """Delegate to the scorer's audited historical-baseline integrity gate."""
    scorer_path = REPO_ROOT / "ocr/research/tools/ocr-source-tiles/score_source_tile_comparison.py"
    if not scorer_path.is_file():
        raise ScoreError("historical_baseline_guard_missing")
    import importlib.util
    spec = importlib.util.spec_from_file_location("score_source_tiles_roi_historical_baseline", scorer_path)
    if spec is None or spec.loader is None:
        raise ScoreError("historical_baseline_guard_unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    try:
        return module.verify_historical_baseline()
    except Exception as exc:
        raise ScoreError("historical_baseline_integrity_validation_failed") from exc


def _baseline_rows(baseline_manifest: Path, audit_path: Path | None) -> dict[str, dict[str, Any]]:
    """Read only frozen extraction artifacts after the fixed historical gate."""
    if baseline_manifest.resolve() != BASELINE_MANIFEST.resolve() or audit_path is None or audit_path.resolve() != BASELINE_AUDIT.resolve():
        raise ScoreError("baseline_profile_path_mismatch")
    manifest, paths = verify_historical_baseline()
    if manifest.get("schema_version") != "passport-five-image-inference/v1":
        raise ScoreError("historical_baseline_schema_invalid")
    rows = {}
    for sample_id in SAMPLE_TO_BASELINE.values():
        artifact_paths = paths.get(sample_id)
        extraction_path = artifact_paths.get("go-extraction.json") if isinstance(artifact_paths, dict) else None
        if not isinstance(extraction_path, Path):
            raise ScoreError("historical_baseline_extraction_missing")
        extraction = _json(extraction_path, "historical_baseline_extraction_invalid")
        if not isinstance(extraction, dict):
            raise ScoreError("historical_baseline_extraction_invalid")
        rows[sample_id] = {"sample_id": sample_id, "extraction": extraction}
    if set(rows) != set(SAMPLE_TO_BASELINE.values()):
        raise ScoreError("baseline_sample_set_invalid")
    return rows


def _reader_status(task: dict[str, Any], reader: str) -> str:
    statuses = task.get("reader_statuses")
    return statuses.get(reader) if isinstance(statuses, dict) else None


def _reader_raw(task: dict[str, Any], reader: str) -> Any:
    if reader == "rapidocr":
        return task.get("rapidocr_natural_full_field_text_raw")
    obj = task.get("tesseract")
    return obj.get("natural_full_field_text_raw") if isinstance(obj, dict) else None


def score_task(task: dict[str, Any], expected: str, field: str, baseline_value: Any) -> dict[str, Any]:
    rapid_status = _status(_reader_raw(task, "rapidocr"), _reader_status(task, "rapidocr"), expected, field)
    tess_status = _status(_reader_raw(task, "tesseract"), _reader_status(task, "tesseract"), expected, field)
    baseline_status = _status(baseline_value, "ok", expected, field)
    rapid_raw, tess_raw = _reader_raw(task, "rapidocr"), _reader_raw(task, "tesseract")
    if rapid_status == "abstain" or tess_status == "abstain":
        pair_agreement = "incomplete"
        consensus_status = "abstain"
    elif canonical(rapid_raw, field) == canonical(tess_raw, field):
        pair_agreement = "exact_agreement"
        consensus_status = "correct" if rapid_status == "correct" else "wrong"
    else:
        pair_agreement = "disagreement"
        consensus_status = "abstain"
    return {
        "rapidocr": rapid_status,
        "tesseract": tess_status,
        "baseline_go": baseline_status,
        "reader_pair_agreement": pair_agreement,
        "reader_pair_consensus_exact": consensus_status,
    }


def build_report(tasks: dict[tuple[str, str], dict[str, Any]],
                 references: dict[str, dict[str, Any]],
                 baseline: dict[str, dict[str, Any]]) -> dict[str, Any]:
    sample_rows = []
    totals = {
        f"{reader}:{field}": {status: 0 for status in ("correct", "wrong", "abstain")}
        for reader in ("rapidocr", "tesseract", "baseline_go") for field in FIELDS
    }
    pair_totals = {field: {
        "exact_agreement": 0, "disagreement": 0, "incomplete": 0,
        "consensus_correct": 0, "consensus_wrong": 0, "consensus_abstain": 0,
    } for field in FIELDS}
    for sample in SAMPLES:
        ref_fields = references[sample].get("fields")
        base_fields = baseline[SAMPLE_TO_BASELINE[sample]]["extraction"].get("fields")
        if not isinstance(ref_fields, dict) or not isinstance(base_fields, dict):
            raise ScoreError("field_map_invalid")
        for field in FIELDS:
            if (sample, field) not in tasks:
                continue
            task = tasks[(sample, field)]
            expected = _expected(ref_fields.get(field), field)
            base_field = base_fields.get(field)
            base_value = base_field.get("value") if isinstance(base_field, dict) else None
            scored = score_task(task, expected, field, base_value)
            sample_rows.append({"sample_id": sample, "field_id": field, **scored})
            for reader in ("rapidocr", "tesseract", "baseline_go"):
                totals[f"{reader}:{field}"][scored[reader]] += 1
            pair = pair_totals[field]
            pair[scored["reader_pair_agreement"]] += 1
            pair[f"consensus_{scored['reader_pair_consensus_exact']}"] += 1
    return {
        "schema_version": "passport-roi-diagnostic-score/v1",
        "status": "completed",
        "samples": sample_rows,
        "totals_by_reader_and_field": totals,
        "reader_pair_totals_by_field": pair_totals,
    }


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Strict raw-output exact diagnostic", "",
        "| Sample | Field | RapidOCR | Tesseract | Baseline Go | Reader pair | Exact consensus |",
        "|---|---|---|---|---|---|---|",
    ]
    for row in report["samples"]:
        lines.append(f"| {row['sample_id']} | {row['field_id']} | {row['rapidocr']} | {row['tesseract']} | {row['baseline_go']} | {row['reader_pair_agreement']} | {row['reader_pair_consensus_exact']} |")
    lines += ["", "Reader and baseline counts by field:", "", "| Reader / field | Correct | Wrong | Abstain |", "|---|---:|---:|---:|"]
    for key, counts in report["totals_by_reader_and_field"].items():
        lines.append(f"| {key} | {counts['correct']} | {counts['wrong']} | {counts['abstain']} |")
    lines += ["", "Reader pair counts by field:", "", "| Field | Exact agreement | Disagreement | Incomplete | Consensus correct | Consensus wrong | Consensus abstain |", "|---|---:|---:|---:|---:|---:|---:|"]
    for field, counts in report["reader_pair_totals_by_field"].items():
        lines.append(f"| {field} | {counts['exact_agreement']} | {counts['disagreement']} | {counts['incomplete']} | {counts['consensus_correct']} | {counts['consensus_wrong']} | {counts['consensus_abstain']} |")
    return "\n".join(lines) + "\n"


def _pair_result(left: Any, right: Any, left_status: str, right_status: str, field: str,
                 normalizer: Any) -> tuple[str, str]:
    if left_status == "abstain" or right_status == "abstain":
        return "incomplete", "abstain"
    if normalizer(left, field) == normalizer(right, field):
        return "exact_agreement", "correct" if left_status == "correct" else "wrong"
    return "disagreement", "abstain"


def _mismatch_shape(raw: str, expected: str, field: str, task: dict[str, Any], reader: str) -> dict[str, Any]:
    observed = production_visual_canonical(raw, field)
    target = canonical(expected, field)
    distance, missing, extra = _edit_metrics(target, observed)
    target_alnum = "".join(character for character in target if character.isalnum())
    observed_alnum = "".join(character for character in observed if character.isalnum())
    prefix_missing = len(target) - len(observed) if 1 <= len(target) - len(observed) <= 5 and target.endswith(observed) else 0
    suffix_missing = len(target) - len(observed) if 1 <= len(target) - len(observed) <= 5 and target.startswith(observed) else 0
    flags = {
        "punctuation_only": bool(target != observed and target_alnum == observed_alnum),
        "short_prefix_missing_1_to_5": prefix_missing > 0,
        "short_suffix_missing_1_to_5": suffix_missing > 0,
        "unmapped_ascii_latin_remains": (
            not (field == "gender" and observed in {"m", "f"})
            and any(("A" <= character <= "Z") or ("a" <= character <= "z") for character in observed)
        ),
        "edit_distance_1": distance == 1,
        "edit_distance_2": distance == 2,
        "edit_distance_3_plus": distance >= 3,
    }
    tess_left_gap = False
    if reader == "tesseract" and prefix_missing:
        tess = task.get("tesseract")
        words = tess.get("words") if isinstance(tess, dict) else None
        binding = task.get("input_binding")
        dimensions = binding.get("crop_dimensions_exif_normalized") if isinstance(binding, dict) else None
        if isinstance(words, list) and isinstance(dimensions, list) and dimensions and isinstance(dimensions[0], int) and dimensions[0] > 0:
            left_edges = [word["box_crop_xywh"][0] for word in words
                          if isinstance(word, dict) and isinstance(word.get("box_crop_xywh"), list)
                          and len(word["box_crop_xywh"]) == 4 and isinstance(word["box_crop_xywh"][0], int)]
            if left_edges:
                tess_left_gap = min(left_edges) > max(10, int(dimensions[0] * 0.05))
    flags["short_prefix_missing_with_tesseract_left_box_gap"] = tess_left_gap
    return {"flags": flags, "edit_distance": distance, "missing_characters": missing, "extra_characters": extra}


def build_normalization_supplement(tasks: dict[tuple[str, str], dict[str, Any]],
                                   references: dict[str, dict[str, Any]],
                                   raw_score: dict[str, Any]) -> dict[str, Any]:
    raw_rows = {(row.get("sample_id"), row.get("field_id")): row for row in raw_score.get("samples", []) if isinstance(row, dict)}
    if set(raw_rows) != EXPECTED_TASKS:
        raise ScoreError("raw_score_task_set_invalid")
    readers = ("rapidocr", "tesseract")
    totals: dict[str, dict[str, dict[str, int]]] = {}
    for reader in readers:
        totals[reader] = {field: {state: 0 for state in ("correct", "wrong", "abstain")} for field in FIELDS}
    pair_totals = {"raw": {}, "production_visual": {}}
    for mode in pair_totals:
        pair_totals[mode] = {field: {"exact_agreement": 0, "disagreement": 0, "incomplete": 0,
                                     "consensus_correct": 0, "consensus_wrong": 0, "consensus_abstain": 0} for field in FIELDS}
    change_totals = {reader: {"wrong_to_correct": 0, "correct_to_wrong": 0, "status_changed": 0,
                              "raw_correct": 0, "production_visual_correct": 0} for reader in readers}
    shape_totals = {reader: {"wrong_after_visual": 0, "punctuation_only": 0,
                             "short_prefix_missing_1_to_5": 0, "short_suffix_missing_1_to_5": 0,
                             "unmapped_ascii_latin_remains": 0,
                             "short_prefix_missing_with_tesseract_left_box_gap": 0,
                             "edit_distance_1": 0, "edit_distance_2": 0, "edit_distance_3_plus": 0,
                             "missing_characters_sum": 0, "extra_characters_sum": 0} for reader in readers}
    rows = []
    for sample in SAMPLES:
        ref_fields = references[sample].get("fields")
        if not isinstance(ref_fields, dict):
            raise ScoreError("reference_field_map_invalid")
        for field in FIELDS:
            key = (sample, field)
            if key not in EXPECTED_TASKS:
                continue
            task = tasks[key]
            expected = _expected(ref_fields.get(field), field)
            reader_states: dict[str, dict[str, str]] = {}
            reader_raw: dict[str, Any] = {}
            for reader in readers:
                raw_value = _reader_raw(task, reader)
                status = _reader_status(task, reader)
                raw_state = _status(raw_value, status, expected, field)
                if status == "ok" and isinstance(raw_value, str) and raw_value.strip():
                    visual_state = "correct" if production_visual_canonical(raw_value, field) == canonical(expected, field) else "wrong"
                else:
                    visual_state = "abstain"
                reader_states[reader] = {"raw": raw_state, "production_visual": visual_state}
                reader_raw[reader] = raw_value
                totals[reader][field][raw_state] += 1
                change = change_totals[reader]
                change["raw_correct"] += raw_state == "correct"
                change["production_visual_correct"] += visual_state == "correct"
                change["wrong_to_correct"] += raw_state == "wrong" and visual_state == "correct"
                change["correct_to_wrong"] += raw_state == "correct" and visual_state == "wrong"
                change["status_changed"] += raw_state != visual_state
                if visual_state == "wrong" and isinstance(raw_value, str) and raw_value.strip():
                    shape = _mismatch_shape(raw_value, expected, field, task, reader)
                    aggregate = shape_totals[reader]
                    aggregate["wrong_after_visual"] += 1
                    for name, enabled in shape["flags"].items():
                        if enabled:
                            aggregate[name] += 1
                    aggregate["missing_characters_sum"] += shape["missing_characters"]
                    aggregate["extra_characters_sum"] += shape["extra_characters"]
            raw_agreement, raw_consensus = _pair_result(
                reader_raw["rapidocr"], reader_raw["tesseract"],
                reader_states["rapidocr"]["raw"], reader_states["tesseract"]["raw"], field, canonical,
            )
            visual_agreement, visual_consensus = _pair_result(
                reader_raw["rapidocr"], reader_raw["tesseract"],
                reader_states["rapidocr"]["production_visual"], reader_states["tesseract"]["production_visual"], field, production_visual_canonical,
            )
            for mode, agreement, consensus in (("raw", raw_agreement, raw_consensus), ("production_visual", visual_agreement, visual_consensus)):
                pair_totals[mode][field][agreement] += 1
                pair_totals[mode][field][f"consensus_{consensus}"] += 1
            old = raw_rows[key]
            rows.append({
                "sample_id": sample, "field_id": field,
                "rapidocr_raw": reader_states["rapidocr"]["raw"],
                "rapidocr_production_visual": reader_states["rapidocr"]["production_visual"],
                "tesseract_raw": reader_states["tesseract"]["raw"],
                "tesseract_production_visual": reader_states["tesseract"]["production_visual"],
                "baseline_go_conservative": old["baseline_go"],
                "pair_raw": raw_agreement, "pair_raw_consensus": raw_consensus,
                "pair_production_visual": visual_agreement,
                "pair_production_visual_consensus": visual_consensus,
            })
    # Keep distinct raw and visual count tables; the raw table above is copied
    # before adding the visual table to avoid conflating the two denominators.
    raw_totals = {reader: {field: dict(counts) for field, counts in fields.items()} for reader, fields in totals.items()}
    visual_totals = {reader: {field: {state: 0 for state in ("correct", "wrong", "abstain")} for field in FIELDS} for reader in readers}
    for row in rows:
        field = row["field_id"]
        for reader in readers:
            visual_totals[reader][field][row[f"{reader}_production_visual"]] += 1
    return {
        "schema_version": "passport-roi-production-normalization-diagnostic/v1",
        "status": "completed",
        "normalization": "exact uppercase Latin lookalikes from internal/passport/normalize.go; no other substitutions, dictionary repair, or spelling correction",
        "samples": rows,
        "counts_by_reader_and_field": {"raw": raw_totals, "production_visual": visual_totals},
        "reader_status_changes": change_totals,
        "reader_pair_counts_by_field": pair_totals,
        "remaining_visual_mismatch_shapes": shape_totals,
    }


def render_normalization_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Production visual-normalization diagnostic", "",
        "This bounded posthoc comparison applies only the fixed uppercase visual Latin-to-Cyrillic map already used by the backend. Counts do not establish general reader quality.", "",
        "| Sample | Field | Rapid raw | Rapid visual | Tesseract raw | Tesseract visual | Baseline Go | Pair raw | Pair visual |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for row in report["samples"]:
        lines.append(f"| {row['sample_id']} | {row['field_id']} | {row['rapidocr_raw']} | {row['rapidocr_production_visual']} | {row['tesseract_raw']} | {row['tesseract_production_visual']} | {row['baseline_go_conservative']} | {row['pair_raw']} | {row['pair_production_visual']} |")
    for label, table in (("Raw counts", report["counts_by_reader_and_field"]["raw"]),
                         ("Production visual-normalized counts", report["counts_by_reader_and_field"]["production_visual"])):
        lines += ["", f"{label} (correct / wrong / abstain):", "", "| Reader / field | Correct | Wrong | Abstain |", "|---|---:|---:|---:|"]
        for reader, fields in table.items():
            for field, counts in fields.items():
                lines.append(f"| {reader} / {field} | {counts['correct']} | {counts['wrong']} | {counts['abstain']} |")
    lines += ["", "Status changes after the fixed map:", "", "| Reader | Wrong to correct | Correct to wrong | Status changed |", "|---|---:|---:|---:|"]
    for reader, counts in report["reader_status_changes"].items():
        lines.append(f"| {reader} | {counts['wrong_to_correct']} | {counts['correct_to_wrong']} | {counts['status_changed']} |")
    lines += ["", "Remaining mismatch shape counts (overlap is allowed; character values are omitted):", "", "| Reader | Wrong after map | Punctuation only | Short prefix missing | Short suffix missing | Unmapped Latin remains | Prefix plus Tesseract left-box gap | Edit distance 1 / 2 / 3+ | Missing chars | Extra chars |", "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"]
    for reader, counts in report["remaining_visual_mismatch_shapes"].items():
        lines.append(f"| {reader} | {counts['wrong_after_visual']} | {counts['punctuation_only']} | {counts['short_prefix_missing_1_to_5']} | {counts['short_suffix_missing_1_to_5']} | {counts['unmapped_ascii_latin_remains']} | {counts['short_prefix_missing_with_tesseract_left_box_gap']} | {counts['edit_distance_1']} / {counts['edit_distance_2']} / {counts['edit_distance_3_plus']} | {counts['missing_characters_sum']} | {counts['extra_characters_sum']} |")
    lines += ["", "The Tesseract left-box gap is a geometry clue for possible leading-prefix loss, not proof that a prefix was cropped."]
    return "\n".join(lines) + "\n"


def run_normalization_supplement(result_path: Path, manifest_path: Path, baseline_manifest: Path,
                                 baseline_audit: Path, old_reference: Path, new_reference: Path,
                                 output_dir: Path, *, runner_path: Path = RUNNER, repo: Path = REPO,
                                 tessdata_dir: Path = DEFAULT_TESSDATA) -> dict[str, Any]:
    _, crop_manifest, tasks = _load_and_validate_runtime(
        result_path, manifest_path, runner_path=runner_path, repo=repo, tessdata_dir=tessdata_dir,
    )
    _baseline_rows(baseline_manifest, baseline_audit)
    if (old_reference.resolve() != (TASK_ROOT / "work/ocr-passport-tests/ground-truth.json").resolve()
            or new_reference.resolve() != (TASK_ROOT / "work/ocr-five-image-eval/reference-new-images.json").resolve()):
        raise ScoreError("reference_profile_path_mismatch")
    pins = _json(POSTHOC_INTEGRITY, "posthoc_integrity_missing")
    if (pins.get("schema_version") != "passport-roi-diagnostic-private-integrity/v1"
            or pins.get("result_sha256") != sha256(result_path)
            or pins.get("manifest_sha256") != sha256(manifest_path)):
        raise ScoreError("posthoc_integrity_binding_invalid")
    old_reference = old_reference.resolve()
    new_reference = new_reference.resolve()
    old, old_hash = _read_reference(old_reference, "old_reference_invalid")
    new, new_hash = _read_reference(new_reference, "new_reference_invalid")
    expected_ref_pins = pins.get("reference_files_sha256")
    if (not isinstance(expected_ref_pins, dict) or expected_ref_pins.get("old_sha256") != old_hash
            or expected_ref_pins.get("new_sha256") != new_hash):
        raise ScoreError("posthoc_reference_binding_invalid")
    source_hashes: dict[str, str] = {}
    for crop in crop_manifest["crops"]:
        sample = crop["sample_id"]
        previous = source_hashes.setdefault(sample, crop["source_sha256"])
        if previous != crop["source_sha256"]:
            raise ScoreError("crop_source_hash_disagreement")
    references = index_reference(old, old_reference.parent, source_hashes, {"sample01", "sample02"})
    references |= index_reference(new, new_reference.parent, source_hashes, {"sample03", "sample04", "sample05"})
    raw_score_path = output_dir / "roi-diagnostic-score.json"
    raw_score = _json(raw_score_path, "raw_score_missing")
    if raw_score.get("status") != "completed" or raw_score.get("schema_version") != "passport-roi-diagnostic-score/v1":
        raise ScoreError("raw_score_not_completed")
    report = build_normalization_supplement(tasks, references, raw_score)
    json_path = output_dir / "production-normalization-diagnostic.json"
    markdown_path = output_dir / "production-normalization-diagnostic.md"
    if json_path.exists() or markdown_path.exists() or NORMALIZATION_INTEGRITY.exists():
        raise ScoreError("normalization_supplement_output_exists")
    _write_private(json_path, json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    _write_private(markdown_path, render_normalization_markdown(report))
    _write_private(NORMALIZATION_INTEGRITY, json.dumps({
        "schema_version": "passport-roi-production-normalization-private-integrity/v1",
        "result_sha256": sha256(result_path), "manifest_sha256": sha256(manifest_path),
        "reference_files_sha256": {"old_sha256": old_hash, "new_sha256": new_hash},
        "normalization_source_sha256": sha256(repo / "internal/passport/normalize.go"),
    }, ensure_ascii=False, indent=2) + "\n")
    return report


def _write_private(path: Path, content: str) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def run(result_path: Path, manifest_path: Path, baseline_manifest: Path,
        baseline_audit: Path, old_reference: Path, new_reference: Path,
        output_dir: Path, *, runner_path: Path = RUNNER, repo: Path = REPO,
        tessdata_dir: Path = DEFAULT_TESSDATA) -> dict[str, Any]:
    # Critical boundary: nothing below this comment reads either reference
    # until the 14 outputs, manifest binding, input hashes and runtime pins pass.
    _, crop_manifest, tasks = _load_and_validate_runtime(
        result_path, manifest_path, runner_path=runner_path, repo=repo, tessdata_dir=tessdata_dir,
    )
    baseline = _baseline_rows(baseline_manifest, baseline_audit)
    if POSTHOC_INTEGRITY.exists():
        raise ScoreError("private_integrity_output_already_exists")
    if output_dir.exists():
        raise ScoreError("output_directory_must_be_new")
    source_hashes: dict[str, str] = {}
    for crop in crop_manifest["crops"]:
        sample = crop["sample_id"]
        previous = source_hashes.setdefault(sample, crop["source_sha256"])
        if previous != crop["source_sha256"]:
            raise ScoreError("crop_source_hash_disagreement")
    if set(source_hashes) != set(SAMPLES):
        raise ScoreError("crop_source_sample_set_invalid")
    # Pin reference files only after the runtime result passes all readiness gates.
    old, old_hash = _read_reference(old_reference, "old_reference_invalid")
    new, new_hash = _read_reference(new_reference, "new_reference_invalid")
    old_index = index_reference(old, old_reference.resolve().parent, source_hashes, {"sample01", "sample02"})
    new_index = index_reference(new, new_reference.resolve().parent, source_hashes, {"sample03", "sample04", "sample05"})
    references = old_index | new_index
    report = build_report(tasks, references, baseline)
    report["reference_files_pinned"] = {"old_sha256": old_hash, "new_sha256": new_hash}
    # Private audit details are stripped from public output below.
    public_report = dict(report)
    public_report.pop("reference_files_pinned", None)
    output_dir.mkdir(parents=True, mode=0o700)
    output_dir.chmod(0o700)
    _write_private(output_dir / "roi-diagnostic-score.json", json.dumps(public_report, ensure_ascii=False, indent=2) + "\n")
    _write_private(output_dir / "roi-diagnostic-score.md", render_markdown(public_report))
    _write_private(POSTHOC_INTEGRITY, json.dumps({
        "schema_version": "passport-roi-diagnostic-private-integrity/v1",
        "manifest_sha256": sha256(manifest_path),
        "result_sha256": sha256(result_path),
        "reference_files_sha256": report["reference_files_pinned"],
    }, ensure_ascii=False, indent=2) + "\n")
    return public_report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results", type=Path, required=True, help="Completed roi-diagnostic-private.json")
    parser.add_argument("--manifest", type=Path, required=True, help="crops.json sealed by the ROI runner")
    parser.add_argument("--baseline-manifest", type=Path, required=True, help="Completed baseline-v2 manifest")
    parser.add_argument("--baseline-audit", type=Path, required=True, help="Root-approved baseline integrity audit")
    parser.add_argument("--old-reference", type=Path, required=True, help="Existing reference for samples 01 and 02")
    parser.add_argument("--new-reference", type=Path, required=True, help="Existing reference for samples 03 to 05")
    parser.add_argument("--output-dir", type=Path, required=True, help="New private output directory")
    parser.add_argument("--tessdata-dir", type=Path, default=DEFAULT_TESSDATA)
    parser.add_argument("--production-normalization-supplement", action="store_true",
                        help="Create the bounded production-map comparison alongside the existing raw report")
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        run_function = run_normalization_supplement if args.production_normalization_supplement else run
        report = run_function(
            args.results.expanduser().resolve(), args.manifest.expanduser().resolve(),
            args.baseline_manifest.expanduser().resolve(), args.baseline_audit.expanduser().resolve(),
            args.old_reference.expanduser().resolve(), args.new_reference.expanduser().resolve(),
            args.output_dir.expanduser().resolve(), tessdata_dir=args.tessdata_dir.expanduser().resolve(),
        )
        total = len(report["samples"])
        print(f"completed status=completed tasks={total} fields={len(FIELDS)}")
        return 0
    except ScoreError as error:
        print(f"failed status={error}", file=sys.stderr)
        return 2
    except Exception:
        print("failed status=unexpected_internal_error", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
