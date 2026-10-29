#!/usr/bin/env python3
"""Posthoc score the completed automatic PaddleOCR-VL five-image run.

All source, baseline-parity, model, evidence, arm-output, and run-status checks
finish before this script opens either reference file.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import importlib.util
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
from typing import Any


TASK = PRIVATE_RESEARCH_ROOT
WORK = TASK / "work/ocr-auto-vl-eval"
REPO = REPO_ROOT
BASELINE = TASK / "work/ocr-five-image-eval/baseline-v2"
BASELINE_MANIFEST = BASELINE / "manifest.json"
BASELINE_AUDIT = TASK / "work/ocr-five-image-eval/baseline-v2-integrity-audit.json"
HISTORICAL_SCORER = REPO / "ocr/research/tools/ocr-source-tiles/score_source_tile_comparison.py"
OLD_GOLD = TASK / "work/ocr-passport-tests/ground-truth.json"
NEW_GOLD = TASK / "work/ocr-five-image-eval/reference-new-images.json"
SOURCES = {
    "sample-01": TASK / "work/ocr-passport-tests/inputs/image-01.jpg",
    "sample-02": TASK / "work/ocr-passport-tests/inputs/image-02.jpg",
    "sample-03": TASK / "work/ocr-new-examples/inputs/sample03.jpg",
    "sample-04": TASK / "work/ocr-new-examples/inputs/sample04.jpg",
    "sample-05": TASK / "work/ocr-new-examples/inputs/sample05.jpg",
}
SAMPLES = tuple(SOURCES)
FIELDS = ("surname", "given_name", "gender", "birth_date", "birth_place", "series", "number", "issue_date", "issuer", "department_code")
OPTIONAL_FIELDS = ("patronymic",)
ALL_FIELDS = FIELDS + OPTIONAL_FIELDS
SCHEMA = "passport-auto-vl-five-image-run/v1"
SCORE_SCHEMA = "passport-auto-vl-five-image-score/v1"
COMPARISON_AUDIT_SCHEMA = "passport-auto-vl-after-v2-pre-roi-change-audit/v1"
COMPARISON_RUN_DIR = WORK / "after-v2"
COMPARISON_AUDIT = WORK / "after-v2-pre-roi-change-audit.json"
COMPARISON_AUDIT_SHA256 = "eb49d292b737739da0a79604c9f57d8bf4a2865a569f0840bf02a9ab7bd086e5"
APPROVED_GO_SOURCE_CHANGE = "internal/passport/vl_field_plan.go"
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
EXPECTED_GO_PACKAGES = {"passport-backend/cmd/ocr-eval", "passport-backend/internal/mrz", "passport-backend/internal/passport"}
ARTIFACTS = (
    "baseline-current.json", "vl-plan.json", "reader-private/evidence.json", "worker-status.json",
    "union-extraction.json", "primary-extraction.json",
)


class ScoreError(ValueError):
    """Safe error containing only a stable code."""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
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
        raise ScoreError(code) from error


def _historical_scorer():
    spec = importlib.util.spec_from_file_location("source_tile_score_for_auto_vl_score", HISTORICAL_SCORER)
    if spec is None or spec.loader is None:
        raise ScoreError("historical_scorer_unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def go_source_manifest() -> dict[str, str]:
    env = {**os.environ, "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off"}
    try:
        result = subprocess.run(
            [str(GO), "list", "-deps", "-json", "./cmd/ocr-eval"], cwd=REPO,
            env=env, text=True, capture_output=True, timeout=30, check=True,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise ScoreError("go_dependency_closure_failed") from error
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
    if packages != EXPECTED_GO_PACKAGES:
        raise ScoreError("go_dependency_closure_unexpected")
    return {str(path): sha256(path) for path in sorted(files) if path.is_file()}


def python_source_manifest() -> dict[str, str]:
    paths = {
        (REPO / "ocr/research/tools/ocr-auto-vl-eval/run_comparison.py").resolve(),
        HISTORICAL_SCORER.resolve(),
        (REPO / "ocr/experiments/auto_vl_pipeline.py").resolve(),
        (REPO / "ocr/experiments/local_paths.py").resolve(),
    }
    if any(not path.is_file() for path in paths):
        raise ScoreError("python_runtime_source_missing")
    return {str(path): sha256(path) for path in sorted(paths)}


def validate_python_source_pins(before: Any, after: Any, current: dict[str, str]) -> None:
    expected = {
        str((REPO / "ocr/research/tools/ocr-auto-vl-eval/run_comparison.py").resolve()),
        str(HISTORICAL_SCORER.resolve()),
        str((REPO / "ocr/experiments/auto_vl_pipeline.py").resolve()),
        str((REPO / "ocr/experiments/local_paths.py").resolve()),
    }
    if (not isinstance(before, dict) or not isinstance(after, dict)
            or set(before) != expected or set(after) != expected or set(current) != expected
            or before != after or current != after):
        raise ScoreError("python_source_integrity_invalid")


def validate_historical_go_scope(old: Any, current: Any) -> None:
    """Permit only the pre-audited planner source change in an old run."""
    if not isinstance(old, dict) or not isinstance(current, dict) or set(old) != set(current):
        raise ScoreError("historical_go_source_scope_invalid")
    changed = [path for path in old if old[path] != current[path]]
    expected = str((REPO / APPROVED_GO_SOURCE_CHANGE).resolve())
    if changed != [expected] and set(changed) != {expected}:
        raise ScoreError("historical_go_source_scope_invalid")


def validate_comparison_run(manifest_path: Path, audit_path: Path, audit_sha256: str,
                            current_manifest: dict[str, Any]) -> tuple[dict[str, Any], dict[str, dict[str, Any]], dict[str, dict[str, dict[str, Any]]]]:
    """Validate the immutable pre-ROI run without reading either reference."""
    if (manifest_path.resolve() != COMPARISON_RUN_DIR.resolve() / "manifest.json"
            or audit_path.resolve() != COMPARISON_AUDIT.resolve()
            or audit_sha256 != COMPARISON_AUDIT_SHA256
            or not audit_path.is_file() or sha256(audit_path) != audit_sha256):
        raise ScoreError("historical_comparison_audit_binding_invalid")
    audit = load_json(audit_path, "historical_comparison_audit_invalid")
    old_manifest = load_json(manifest_path, "historical_comparison_manifest_invalid")
    if (not isinstance(audit, dict) or audit.get("schema_version") != COMPARISON_AUDIT_SCHEMA
            or audit.get("status") != "guard_passed" or audit.get("reference_data_read") is not False
            or audit.get("reference_data_used_for_runtime_or_planning") is not False
            or audit.get("run_manifest_sha256") != sha256(manifest_path)
            or audit.get("approved_future_source_change_scope") != {
                "all_other_runtime_paths_must_remain_unchanged": True,
                "runtime_paths": [APPROVED_GO_SOURCE_CHANGE],
                "tests_are_outside_runtime_closure": True,
            }):
        raise ScoreError("historical_comparison_audit_scope_invalid")
    pins = audit.get("identity_pins")
    if not isinstance(pins, dict):
        raise ScoreError("historical_comparison_pins_invalid")
    go_before = old_manifest.get("go_source_sha256", {}).get("before")
    go_after = old_manifest.get("go_source_sha256", {}).get("after")
    if go_before != go_after or go_before != pins.get("go_source_sha256"):
        raise ScoreError("historical_comparison_source_before_after_invalid")
    current_go = current_manifest.get("go_source_sha256", {}).get("after")
    validate_historical_go_scope(go_before, current_go)

    # Verify all stored audit identity pins against the immutable run itself.
    if (old_manifest.get("schema_version") != SCHEMA or old_manifest.get("status") != "completed"
            or old_manifest.get("reference_data_read") is not False
            or old_manifest.get("reference_data_used_for_runtime_or_planning") is not False
            or old_manifest.get("baseline_equivalence") != "exact_except_field_evidence_set_order"
            or old_manifest.get("baseline_manifest_sha256") != audit.get("baseline_manifest_sha256")
            or old_manifest.get("baseline_audit_sha256") != audit.get("baseline_audit_sha256")
            or old_manifest.get("inputs_sha256", {}).get("before") != pins.get("input_sha256")
            or old_manifest.get("ocr_images_sha256", {}).get("before") != pins.get("ocr_image_sha256")
            or old_manifest.get("python_source_sha256", {}).get("before") != pins.get("python_inference_source_sha256")
            or old_manifest.get("evaluator_path") != pins.get("evaluator_path")
            or old_manifest.get("evaluator_sha256", {}).get("before") != pins.get("evaluator_sha256")
            or old_manifest.get("model_tree_sha256") != pins.get("model_tree_sha256")):
        raise ScoreError("historical_comparison_identity_pin_mismatch")
    if (old_manifest.get("inputs_sha256", {}).get("before") != current_manifest.get("inputs_sha256", {}).get("before")
            or old_manifest.get("ocr_images_sha256", {}).get("before") != current_manifest.get("ocr_images_sha256", {}).get("before")
            or old_manifest.get("model_tree_sha256") != current_manifest.get("model_tree_sha256")
            or old_manifest.get("python_source_sha256", {}).get("after") != current_manifest.get("python_source_sha256", {}).get("after")
            or old_manifest.get("go_options") != current_manifest.get("go_options")):
        raise ScoreError("historical_comparison_run_mismatch")

    audited_rows = pins.get("samples")
    old_rows = {row.get("sample_id"): row for row in old_manifest.get("samples", []) if isinstance(row, dict)}
    new_rows = {row.get("sample_id"): row for row in current_manifest.get("samples", []) if isinstance(row, dict)}
    if set(audited_rows or {}) != set(SAMPLES) or set(old_rows) != set(SAMPLES) or set(new_rows) != set(SAMPLES):
        raise ScoreError("historical_comparison_sample_set_invalid")
    for sample in SAMPLES:
        audited = audited_rows[sample]
        old_row = old_rows[sample]
        if (audited.get("source_sha256") != old_row.get("source_sha256")
                or audited.get("ocr_image_sha256") != old_row.get("ocr_image_sha256")
                or audited.get("vl_model_sha256") != old_row.get("vl_model_sha256")
                or audited.get("artifacts") != old_row.get("artifacts")
                or audited.get("reader_artifacts_sha256") != old_row.get("reader_artifacts_sha256")
                or old_row.get("source_sha256") != new_rows[sample].get("source_sha256")
                or old_row.get("vl_model_sha256") != new_rows[sample].get("vl_model_sha256")):
            raise ScoreError("historical_comparison_sample_pin_mismatch")

    # Reuse the full current-run gate against the historical Go snapshot. The
    # source scope was independently checked above and Python/current inputs are
    # still required to match live pins exactly.
    original_go_manifest = go_source_manifest
    try:
        globals()["go_source_manifest"] = lambda: go_before
        old_manifest_validated, _old_baseline, old_arms = validate_completed_run(manifest_path)
    finally:
        globals()["go_source_manifest"] = original_go_manifest
    return old_manifest_validated, _old_baseline, old_arms


def _validate_artifact(path: Path, digest: Any) -> None:
    if not path.is_file() or not isinstance(digest, str) or len(digest) != 64 or sha256(path) != digest:
        raise ScoreError("run_artifact_hash_mismatch")
    if path.stat().st_mode & 0o077:
        raise ScoreError("run_artifact_permissions_invalid")


def _tree_hashes(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for current, directories, filenames in os.walk(root, followlinks=False):
        current_path = Path(current)
        directories.sort()
        filenames.sort()
        if any((current_path / name).is_symlink() for name in directories):
            raise ScoreError("reader_artifact_symlink_rejected")
        for name in filenames:
            path = current_path / name
            if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
                raise ScoreError("reader_artifact_permissions_invalid")
            result[path.relative_to(root.parent).as_posix()] = sha256(path)
    return result


def _validate_extraction_binding(extraction: Any, image_id: str) -> None:
    if not isinstance(extraction, dict) or extraction.get("image_id") != image_id:
        raise ScoreError("extraction_image_binding_invalid")
    fields = extraction.get("fields")
    if not isinstance(fields, dict) or set(fields) != set(ALL_FIELDS):
        raise ScoreError("extraction_fields_invalid")


def validate_completed_run(manifest_path: Path) -> tuple[dict[str, Any], dict[str, dict[str, Any]], dict[str, dict[str, Any]]]:
    manifest = load_json(manifest_path, "run_manifest_invalid")
    if (not isinstance(manifest, dict) or manifest.get("schema_version") != SCHEMA
            or manifest.get("status") != "completed"
            or manifest.get("baseline_equivalence") != "exact_except_field_evidence_set_order"
            or manifest.get("reference_data_read") is not False
            or manifest.get("reference_data_used_for_runtime_or_planning") is not False):
        raise ScoreError("run_not_completed_or_reference_boundary_invalid")

    # Revalidate the historical approved baseline using its own dependency-aware
    # validator. This reads no reference files.
    try:
        historical = _historical_scorer()
        historical_manifest, frozen = historical.verify_historical_baseline()
    except Exception as error:
        raise ScoreError("historical_baseline_integrity_invalid") from error
    if manifest.get("baseline_manifest_sha256") != sha256(BASELINE_MANIFEST) or manifest.get("baseline_audit_sha256") != sha256(BASELINE_AUDIT):
        raise ScoreError("historical_baseline_binding_mismatch")
    historical_rows = {
        row.get("sample_id"): row for row in historical_manifest.get("samples", []) if isinstance(row, dict)
    }
    if set(historical_rows) != set(SAMPLES):
        raise ScoreError("historical_baseline_sample_set_invalid")

    input_hashes = manifest.get("inputs_sha256")
    if not isinstance(input_hashes, dict) or input_hashes.get("before") != input_hashes.get("after"):
        raise ScoreError("input_integrity_invalid")
    expected_inputs = historical_manifest.get("inputs_sha256", {}).get("before")
    if input_hashes.get("before") != expected_inputs or set(expected_inputs or {}) != set(SOURCES):
        raise ScoreError("input_baseline_binding_invalid")
    for sample, source in SOURCES.items():
        if not source.is_file() or sha256(source) != input_hashes["before"].get(sample):
            raise ScoreError("source_image_hash_mismatch")
    ocr_images = manifest.get("ocr_images_sha256")
    if (not isinstance(ocr_images, dict) or ocr_images.get("before") != ocr_images.get("after")
            or set(ocr_images.get("before", {})) != set(SAMPLES)):
        raise ScoreError("ocr_image_integrity_invalid")

    go_before = manifest.get("go_source_sha256", {}).get("before")
    go_after = manifest.get("go_source_sha256", {}).get("after")
    python_before = manifest.get("python_source_sha256", {}).get("before")
    python_after = manifest.get("python_source_sha256", {}).get("after")
    if not isinstance(go_before, dict) or go_before != go_after or go_source_manifest() != go_after:
        raise ScoreError("go_source_integrity_invalid")
    validate_python_source_pins(python_before, python_after, python_source_manifest())
    evaluator_hashes = manifest.get("evaluator_sha256")
    if (not isinstance(evaluator_hashes, dict) or evaluator_hashes.get("before") != evaluator_hashes.get("after")
            or not isinstance(evaluator_hashes.get("after"), str) or len(evaluator_hashes["after"]) != 64):
        raise ScoreError("evaluator_integrity_invalid")
    evaluator_path_value = manifest.get("evaluator_path")
    if not isinstance(evaluator_path_value, str):
        raise ScoreError("evaluator_path_invalid")
    evaluator_path = Path(evaluator_path_value)
    if not evaluator_path.is_file() or not os.access(evaluator_path, os.X_OK) or sha256(evaluator_path) != evaluator_hashes["after"]:
        raise ScoreError("evaluator_binary_binding_invalid")
    model_hashes = manifest.get("model_tree_sha256")
    if not isinstance(model_hashes, dict) or model_hashes.get("unchanged") is not True:
        raise ScoreError("model_tree_integrity_invalid")
    model_by_sample = model_hashes.get("by_sample")
    if not isinstance(model_by_sample, dict) or set(model_by_sample) != set(SAMPLES) or len(set(model_by_sample.values())) != 1:
        raise ScoreError("model_tree_sample_binding_invalid")

    rows = manifest.get("samples")
    if not isinstance(rows, list) or len(rows) != len(SAMPLES):
        raise ScoreError("run_sample_set_invalid")
    row_map = {row.get("sample_id"): row for row in rows if isinstance(row, dict)}
    if set(row_map) != set(SAMPLES) or len(row_map) != len(SAMPLES):
        raise ScoreError("run_sample_set_invalid")
    baseline_extractions: dict[str, dict[str, Any]] = {}
    arm_extractions: dict[str, dict[str, dict[str, Any]]] = {"union": {}, "primary": {}}
    root = manifest_path.resolve().parent
    for sample in SAMPLES:
        row = row_map[sample]
        if row.get("status") != "completed" or row.get("source_sha256") != input_hashes["before"][sample]:
            raise ScoreError("run_sample_incomplete_or_unbound")
        sample_dir = root / sample
        frozen_row = historical_rows[sample]
        observations_path = BASELINE / sample / "observations.json"
        frozen_extraction_path = BASELINE / sample / "go-extraction.json"
        if (row.get("frozen_observations_sha256") != frozen_row.get("observation_sha256")
                or row.get("frozen_extraction_sha256") != frozen_row.get("extraction_sha256")
                or not observations_path.is_file() or sha256(observations_path) != row.get("frozen_observations_sha256")):
            raise ScoreError("frozen_baseline_artifact_binding_invalid")
        template_path = BASELINE / sample / "ocr-private/template-evidence.json"
        expected_template_hash = sha256(template_path) if template_path.is_file() else None
        if row.get("template_evidence_sha256") != expected_template_hash:
            raise ScoreError("frozen_template_evidence_binding_invalid")
        artifacts = row.get("artifacts")
        if not isinstance(artifacts, dict) or not set(ARTIFACTS).issubset(artifacts):
            raise ScoreError("run_artifact_map_invalid")
        for relative in ARTIFACTS:
            _validate_artifact(sample_dir / relative, artifacts[relative])
        reader_artifact_pins = row.get("reader_artifacts_sha256")
        if not isinstance(reader_artifact_pins, dict) or _tree_hashes(sample_dir / "reader-private") != reader_artifact_pins:
            raise ScoreError("reader_artifact_tree_mismatch")

        plan = load_json(sample_dir / "vl-plan.json", "vl_plan_invalid")
        evidence = load_json(sample_dir / "reader-private/evidence.json", "vl_evidence_invalid")
        worker = load_json(sample_dir / "worker-status.json", "vl_worker_status_invalid")
        if (plan.get("schema_version") != "passport-vl-field-plan/v1"
                or plan.get("image", {}).get("sha256") != row.get("ocr_image_sha256")
                or plan.get("plan_sha256") != row.get("plan_content_sha256")
                or len(plan.get("regions", [])) != row.get("plan_region_count")):
            raise ScoreError("vl_plan_binding_invalid")
        source_path = SOURCES[sample]
        expected_image_path = source_path if sha256(source_path) == plan["image"].get("sha256") else (
            BASELINE / sample / "ocr-private/canonical-spread.png"
        )
        if (not expected_image_path.is_file() or sha256(expected_image_path) != plan["image"].get("sha256")
                or ocr_images["before"].get(sample) != plan["image"].get("sha256")
                or row.get("vl_image_path") != str(expected_image_path)):
            raise ScoreError("vl_image_binding_invalid")
        if (evidence.get("schema_version") != "passport-vl-field-evidence/v1"
                or evidence.get("image") != plan.get("image")
                or evidence.get("plan_sha256") != plan.get("plan_sha256")
                or evidence.get("reader", {}).get("model_tree_sha256") != model_by_sample.get(sample)
                or len(evidence.get("regions", [])) != len(plan.get("regions", []))):
            raise ScoreError("vl_evidence_binding_invalid")
        if (worker.get("status") != "completed" or worker.get("sample_id") != sample
                or worker.get("evidence_sha256") != artifacts["reader-private/evidence.json"]
                or worker.get("model_tree_sha256") != model_by_sample.get(sample)
                or worker.get("planned_region_count") != len(plan.get("regions", []))
                or worker.get("reader_region_count") != len(evidence.get("regions", []))):
            raise ScoreError("vl_worker_binding_invalid")

        frozen_path = frozen_extraction_path
        current_path = sample_dir / "baseline-current.json"
        if sha256(frozen_path) != row.get("frozen_extraction_sha256"):
            raise ScoreError("frozen_extraction_binding_invalid")
        baseline = load_json(current_path, "baseline_extraction_invalid")
        frozen = load_json(frozen_path, "frozen_extraction_invalid")
        if baseline_equivalence_json(baseline) != baseline_equivalence_json(frozen):
            raise ScoreError("baseline_parity_artifact_mismatch")
        _validate_extraction_binding(baseline, plan["image"]["id"])
        baseline_extractions[sample] = baseline
        for policy in ("union", "primary"):
            path = sample_dir / f"{policy}-extraction.json"
            extraction = load_json(path, "arm_extraction_invalid")
            _validate_extraction_binding(extraction, plan["image"]["id"])
            arm_extractions[policy][sample] = extraction

    required_baseline = {key: 0 for key in ("correct", "wrong", "abstain")}
    # This validates baseline metrics after references are loaded; here we only
    # enforce source identity and all outputs. Return before any gold is read.
    del required_baseline
    return manifest, baseline_extractions, arm_extractions


def selected_metrics(historical: Any, extraction: dict[str, Any], reference: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return historical.score_extraction(extraction, reference)


def counts(metrics: dict[str, dict[str, Any]], fields: tuple[str, ...]) -> dict[str, int]:
    return {
        "correct": sum(metrics[field]["selected_status"] == "correct" for field in fields),
        "wrong": sum(metrics[field]["selected_status"] == "wrong" for field in fields),
        "abstain": sum(metrics[field]["selected_status"] == "abstain" for field in fields),
        "manual_review_flags": sum(metrics[field]["manual_review_required"] for field in fields),
        "correct_candidate_available": sum(metrics[field]["correct_candidate_available"] for field in fields),
    }


def delta(before: dict[str, int], after: dict[str, int]) -> dict[str, int]:
    return {key: after[key] - before[key] for key in before}


def change(before: str, after: str) -> str:
    if before != "correct" and after == "correct":
        return "gain"
    if before == "correct" and after != "correct":
        return "loss"
    if before == "wrong" and after == "abstain":
        return "wrong_to_abstain"
    if before == "abstain" and after == "wrong":
        return "abstain_to_wrong"
    return "unchanged"


def _safe_route(row: dict[str, Any], plan: dict[str, Any], evidence: dict[str, Any]) -> dict[str, Any]:
    regions = plan.get("regions", [])
    skipped = plan.get("skipped_fields", [])
    read_status = {item.get("region_id"): item.get("status") for item in evidence.get("regions", []) if isinstance(item, dict)}
    return {
        "planned_fields": [item.get("field_id") for item in regions if isinstance(item, dict)],
        "skipped_fields": [{"field_id": item.get("field_id"), "reason": item.get("reason")} for item in skipped if isinstance(item, dict)],
        "reader_region_statuses": [
            {"field_id": item.get("field_id"), "status": read_status.get(item.get("id"), "missing")}
            for item in regions if isinstance(item, dict)
        ],
    }


def _arm_report(historical: Any, arm: str, baseline: dict[str, dict[str, Any]],
                extractions: dict[str, dict[str, Any]], references: dict[str, dict[str, Any]],
                source_hashes: dict[str, str], manifest: dict[str, Any], plans: dict[str, dict[str, Any]],
                evidences: dict[str, dict[str, Any]]) -> dict[str, Any]:
    per_sample = []
    total_before = {key: 0 for key in ("correct", "wrong", "abstain", "manual_review_flags", "correct_candidate_available")}
    total_after = dict(total_before)
    field_totals = {field: {"gain": 0, "loss": 0, "wrong_to_abstain": 0, "abstain_to_wrong": 0, "unchanged": 0,
                            "candidate_available_before": 0, "candidate_available_after": 0} for field in FIELDS}
    for sample in SAMPLES:
        reference = references[source_hashes[sample]]
        before = selected_metrics(historical, baseline[sample], reference)
        after = selected_metrics(historical, extractions[sample], reference)
        before_counts = counts(before, FIELDS)
        after_counts = counts(after, FIELDS)
        for key in total_before:
            total_before[key] += before_counts[key]
            total_after[key] += after_counts[key]
        per_field = {}
        for field in FIELDS:
            old = before[field]
            new = after[field]
            transition = change(old["selected_status"], new["selected_status"])
            field_totals[field][transition] += 1
            field_totals[field]["candidate_available_before"] += int(old["correct_candidate_available"])
            field_totals[field]["candidate_available_after"] += int(new["correct_candidate_available"])
            per_field[field] = {
                "before_status": old["selected_status"], "after_status": new["selected_status"], "change": transition,
                "manual_review_before": old["manual_review_required"], "manual_review_after": new["manual_review_required"],
                "correct_candidate_before": old["correct_candidate_available"],
                "correct_candidate_after": new["correct_candidate_available"],
            }
        row = next(item for item in manifest["samples"] if item["sample_id"] == sample)
        route = _safe_route(row, plans[sample], evidences[sample])
        per_sample.append({
            "sample_id": sample,
            "required_before": before_counts, "required_after": after_counts,
            "required_delta": delta(before_counts, after_counts), "fields": per_field,
            "optional_patronymic": {
                "before_status": before["patronymic"]["selected_status"],
                "after_status": after["patronymic"]["selected_status"],
                "manual_review_before": before["patronymic"]["manual_review_required"],
                "manual_review_after": after["patronymic"]["manual_review_required"],
                "correct_candidate_before": before["patronymic"]["correct_candidate_available"],
                "correct_candidate_after": after["patronymic"]["correct_candidate_available"],
            },
            "route": route,
        })
    return {
        "policy": arm,
        "before_required": total_before,
        "after_required": total_after,
        "required_delta": delta(total_before, total_after),
        "optional_patronymic": {
            "before": counts_for_patronymic(historical, baseline, references, source_hashes),
            "after": counts_for_patronymic(historical, extractions, references, source_hashes),
        },
        "per_field": field_totals,
        "samples": per_sample,
    }


def counts_for_patronymic(historical: Any, rows: dict[str, dict[str, Any]], references: dict[str, dict[str, Any]], source_hashes: dict[str, str]) -> dict[str, int]:
    total = {key: 0 for key in ("correct", "wrong", "abstain", "manual_review_flags", "correct_candidate_available")}
    for sample in SAMPLES:
        metrics = selected_metrics(historical, rows[sample], references[source_hashes[sample]])
        item = counts(metrics, OPTIONAL_FIELDS)
        for key in total:
            total[key] += item[key]
    return total


def score(manifest_path: Path, output_dir: Path, comparison_manifest_path: Path | None = None,
          comparison_audit_path: Path | None = None, comparison_audit_sha256: str | None = None) -> None:
    if any(value is None for value in (comparison_manifest_path, comparison_audit_path, comparison_audit_sha256)):
        raise ScoreError("historical_comparison_arguments_required")
    manifest, baseline, arms = validate_completed_run(manifest_path)
    historical_run, _historical_baseline, historical_arms = validate_comparison_run(
        comparison_manifest_path, comparison_audit_path, comparison_audit_sha256, manifest,
    )
    historical = _historical_scorer()
    source_hashes = manifest["inputs_sha256"]["before"]

    # References are intentionally opened only after every run, baseline parity,
    # Go/input/model/source and artifact binding check above has passed.
    old_gold = load_json(OLD_GOLD, "old_reference_invalid")
    new_gold = load_json(NEW_GOLD, "new_reference_invalid")
    old_refs = historical.index_gold(old_gold, OLD_GOLD.parent, {source_hashes["sample-01"], source_hashes["sample-02"]})
    new_refs = historical.index_gold(new_gold, NEW_GOLD.parent, {source_hashes["sample-03"], source_hashes["sample-04"], source_hashes["sample-05"]})
    references = {**old_refs, **new_refs}
    if set(references) != set(source_hashes.values()):
        raise ScoreError("reference_source_binding_invalid")

    # This frozen aggregate is an integrity check; it does not select or tune
    # either VL policy.
    baseline_total = {key: 0 for key in ("correct", "wrong", "abstain")}
    for sample in SAMPLES:
        metric = selected_metrics(historical, baseline[sample], references[source_hashes[sample]])
        row = counts(metric, FIELDS)
        for key in baseline_total:
            baseline_total[key] += row[key]
    if baseline_total != {"correct": 27, "wrong": 9, "abstain": 14}:
        raise ScoreError("recomputed_frozen_baseline_metric_mismatch")

    plans: dict[str, dict[str, Any]] = {}
    evidences: dict[str, dict[str, Any]] = {}
    for sample in SAMPLES:
        sample_dir = manifest_path.resolve().parent / sample
        plans[sample] = load_json(sample_dir / "vl-plan.json", "vl_plan_invalid")
        evidences[sample] = load_json(sample_dir / "reader-private/evidence.json", "vl_evidence_invalid")
    arms_report = {
        policy: _arm_report(historical, policy, baseline, extractions, references, source_hashes, manifest, plans, evidences)
        for policy, extractions in arms.items()
    }
    paired_plans: dict[str, dict[str, Any]] = {}
    paired_evidences: dict[str, dict[str, Any]] = {}
    for sample in SAMPLES:
        sample_dir = manifest_path.resolve().parent / sample
        paired_plans[sample] = load_json(sample_dir / "vl-plan.json", "vl_plan_invalid")
        paired_evidences[sample] = load_json(sample_dir / "reader-private/evidence.json", "vl_evidence_invalid")
    paired_report = {
        policy: _arm_report(
            historical, policy, historical_arms[policy], arms[policy], references, source_hashes,
            manifest, paired_plans, paired_evidences,
        )
        for policy in ("union", "primary")
    }
    report = {
        "schema_version": SCORE_SCHEMA,
        "status": "completed",
        "frozen_baseline_integrity_audit_accepted": True,
        "paired_comparison_validated": True,
        "baseline_equivalence": "exact_except_field_evidence_set_order",
        "dataset_role": "development_photos_not_heldout",
        "limitation": "sample-03-repeats-an-existing-document; samples-04-and-05-are-other-documents-without-MRZ",
        "required_field_ids": list(FIELDS),
        "baseline_required": baseline_total,
        "arms": arms_report,
        "paired_after_v2": paired_report,
        "policy": {
            "arms_are_predeclared_global_policies": ["union", "primary"],
            "selected_correct_wrong_abstain_uses_selected_value_and_reference_only": True,
            "manual_review_is_overlapping_uncertain_flag": True,
            "candidate_availability_is_separate": True,
            "policy_selection_uses_no_per_field_reference_tuning": True,
            "all_vl_proposals_require_manual_review": True,
        },
        "scope": "five development images; no held-out quality claim",
    }
    output_dir = output_dir.expanduser().absolute()
    if output_dir.exists():
        raise ScoreError("output_directory_already_exists")
    output_dir.mkdir(parents=True, exist_ok=False, mode=0o700)
    output_dir.chmod(0o700)
    write_private(output_dir / "auto-vl-comparison.json", json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    write_private(output_dir / "auto-vl-comparison.md", render_markdown(report))
    print("completed samples=5 required_fields=10 arms=2")


def write_private(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Automatic VL field-crop comparison", "",
        "Five development images; results are not a held-out quality estimate. Sample-03 repeats an existing document; samples-04 and -05 are other documents without MRZ.",
        "Baseline parity is exact except for ordering in field evidence line_ids and pass_ids sets.",
        "Selected correct/wrong/abstain is based on the selected value. Manual-review flags overlap with these outcomes; correct-candidate availability is separate.",
        "Union and primary are predeclared global policies. No per-field policy was selected using references. All VL proposals require human review.", "",
        "| Sample | Policy | Before C/W/A | After C/W/A | Manual review before→after | Correct candidates before→after | Planned fields | Skipped fields |",
        "|---|---|---:|---:|---:|---:|---|---|",
    ]
    for policy, arm in report["arms"].items():
        for row in arm["samples"]:
            before, after = row["required_before"], row["required_after"]
            planned = ", ".join(row["route"]["planned_fields"]) or "none"
            skipped = ", ".join(item["field_id"] for item in row["route"]["skipped_fields"]) or "none"
            lines.append(
                f"| {row['sample_id']} | {policy} | {before['correct']}/{before['wrong']}/{before['abstain']} | "
                f"{after['correct']}/{after['wrong']}/{after['abstain']} | {before['manual_review_flags']}→{after['manual_review_flags']} | "
                f"{before['correct_candidate_available']}→{after['correct_candidate_available']} | {planned} | {skipped} |"
            )
        before, after = arm["before_required"], arm["after_required"]
        lines.append(
            f"| **Total** | **{policy}** | **{before['correct']}/{before['wrong']}/{before['abstain']}** | "
            f"**{after['correct']}/{after['wrong']}/{after['abstain']}** | {before['manual_review_flags']}→{after['manual_review_flags']} | "
            f"{before['correct_candidate_available']}→{after['correct_candidate_available']} | — | — |"
        )
    if report.get("paired_after_v2") is not None:
        lines += ["", "## Paired ROI comparison against the audited after-v2 run", "",
                  "This section compares each predeclared policy with the same policy from the audited pre-ROI run. It is separate from the frozen baseline comparison above.", "",
                  "| Sample | Policy | Before C/W/A | After C/W/A | Correct candidates before→after |", "|---|---|---:|---:|---:|"]
        for policy, arm in report["paired_after_v2"].items():
            for row in arm["samples"]:
                before, after = row["required_before"], row["required_after"]
                lines.append(f"| {row['sample_id']} | {policy} | {before['correct']}/{before['wrong']}/{before['abstain']} | {after['correct']}/{after['wrong']}/{after['abstain']} | {before['correct_candidate_available']}→{after['correct_candidate_available']} |")
            before, after = arm["before_required"], arm["after_required"]
            lines.append(f"| **Total** | **{policy}** | **{before['correct']}/{before['wrong']}/{before['abstain']}** | **{after['correct']}/{after['wrong']}/{after['abstain']}** | **{before['correct_candidate_available']}→{after['correct_candidate_available']}** |")
    lines += ["", "## Per-field transitions", "", "| Policy | Field ID | Gains | Losses | Wrong→abstain | Abstain→wrong | Correct candidates before→after |", "|---|---|---:|---:|---:|---:|---:|"]
    for policy, arm in report["arms"].items():
        for field, totals in arm["per_field"].items():
            lines.append(f"| {policy} | {field} | {totals['gain']} | {totals['loss']} | {totals['wrong_to_abstain']} | {totals['abstain_to_wrong']} | {totals['candidate_available_before']}→{totals['candidate_available_after']} |")
    lines += ["", "Optional patronymic is reported separately in JSON. Reports contain field and sample IDs with counts and statuses only; they omit recognized text, reference values, source paths, source hashes, and images."]
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--comparison-manifest", type=Path)
    parser.add_argument("--comparison-audit", type=Path)
    parser.add_argument("--comparison-audit-sha256")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        score(args.manifest.expanduser().resolve(), args.output_dir,
              args.comparison_manifest.expanduser().resolve() if args.comparison_manifest else None,
              args.comparison_audit.expanduser().resolve() if args.comparison_audit else None,
              args.comparison_audit_sha256)
        return 0
    except ScoreError as error:
        print(f"failed status={error}", file=sys.stderr)
        return 2
    except Exception:
        print("failed status=unexpected_internal_error", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
