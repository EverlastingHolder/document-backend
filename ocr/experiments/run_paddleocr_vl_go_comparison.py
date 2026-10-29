#!/usr/bin/env python3
"""Adapt cached PaddleOCR-VL layout blocks and compare the same offline Go extractor.

No Paddle model is loaded. Private OCR text and Go outputs stay under the
restricted work root; the public report is an allowlist of statuses and counts.
"""
from __future__ import annotations

import hashlib
import json
import math
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[2]

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import ARCHIVE_OUTPUT_ROOT, ARCHIVE_WORK_ROOT  # noqa: E402
from ocr.experiments.run_algorithm_comparison import REQUIRED_FIELDS, score_extraction  # noqa: E402

SCHEMA = "passport-paddleocr-vl-go-comparison/v1"
OBSERVATIONS_SCHEMA = "passport-ocr-observations/v1"
SAMPLE_IDS = ("sample-01", "sample-02")
DEFAULT_TRIAL_ROOT = ARCHIVE_WORK_ROOT / "ocr-vl-work" / "private"
DEFAULT_CACHE_ROOT = ARCHIVE_WORK_ROOT / "ocr-field-roi-work"
DEFAULT_IMAGE_ROOT = ARCHIVE_WORK_ROOT / "ocr-passport-tests" / "inputs"
DEFAULT_GOLD = ARCHIVE_WORK_ROOT / "ocr-passport-tests" / "ground-truth.json"
DEFAULT_GO = ARCHIVE_WORK_ROOT / "ocr-vl-go-work" / "ocr-eval"
DEFAULT_WORK = ARCHIVE_WORK_ROOT / "ocr-vl-go-work"
DEFAULT_OUTPUT = ARCHIVE_OUTPUT_ROOT / "ocr-vl-go-comparison"
GO_FLAGS = ("--passes=all", "--normalize-names=true", "--mrz-policy", "augment")


class ComparisonError(RuntimeError):
    """Sanitized failure reason; never contains OCR or gold text."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ComparisonError(code) from error


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def _private_json(path: Path, payload: Any) -> None:
    _private_dir(path.parent)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def _normalized_image(path: Path) -> tuple[int, int, int]:
    """Return EXIF-transposed dimensions and orientation; no image is changed."""
    try:
        from PIL import Image, ImageOps
        with Image.open(path) as opened:
            orientation = int(opened.getexif().get(274, 1))
            normalized = ImageOps.exif_transpose(opened)
            return normalized.width, normalized.height, orientation
    except Exception as error:
        raise ComparisonError("source_image_metadata_unavailable") from error


def _quad(value: Any, width: int, height: int) -> list[list[float]]:
    if not isinstance(value, list) or len(value) != 4:
        raise ComparisonError("native_block_polygon_invalid")
    result: list[list[float]] = []
    for point in value:
        if not isinstance(point, list) or len(point) != 2:
            raise ComparisonError("native_block_polygon_invalid")
        x, y = point
        if isinstance(x, bool) or isinstance(y, bool) or not isinstance(x, (int, float)) or not isinstance(y, (int, float)):
            raise ComparisonError("native_block_polygon_invalid")
        if not math.isfinite(x) or not math.isfinite(y) or x < 0 or y < 0 or x > width or y > height:
            raise ComparisonError("native_block_polygon_out_of_bounds")
        result.append([float(x), float(y)])
    if len({(point[0], point[1]) for point in result}) != 4:
        raise ComparisonError("native_block_polygon_degenerate")
    return result


def build_adapter(
    *, sample_id: str, source_path: Path, source_hash: str, image_meta: dict[str, Any],
    result: dict[str, Any], worker_meta: dict[str, Any], result_sha256: str,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Map every textual native layout block one-to-one to a Go OCRLine."""
    if sample_id not in SAMPLE_IDS or image_meta.get("sample_id") != sample_id:
        raise ComparisonError("sample_identity_mismatch")
    if not source_path.is_file() or _sha256(source_path) != source_hash:
        raise ComparisonError("input_image_sha_mismatch")
    width, height, orientation = _normalized_image(source_path)
    if orientation != 1:
        raise ComparisonError("source_exif_orientation_not_identity")
    if result.get("width") != width or result.get("height") != height:
        raise ComparisonError("native_result_dimensions_mismatch")
    if image_meta.get("sha256_pre") != source_hash or image_meta.get("sha256_post") != source_hash:
        raise ComparisonError("input_manifest_sha_mismatch")
    if worker_meta.get("returncode") != 0:
        raise ComparisonError("native_worker_not_successful")
    configured = worker_meta.get("configured")
    disabled_values = {False, "vendor_default_false"}
    if not isinstance(configured, dict) or configured.get("use_doc_orientation_classify") not in disabled_values or configured.get("use_doc_unwarping") not in disabled_values:
        raise ComparisonError("native_preprocessor_transform_not_disabled")
    if configured.get("pipeline_version") != "v1.6":
        raise ComparisonError("native_pipeline_pin_mismatch")
    blocks = result.get("parsing_res_list")
    if not isinstance(blocks, list) or not blocks:
        raise ComparisonError("native_blocks_missing")

    lines: list[dict[str, Any]] = []
    map_rows: list[dict[str, Any]] = []
    all_x: list[float] = []
    all_y: list[float] = []
    block_ids: set[str] = set()
    for block in blocks:
        if not isinstance(block, dict):
            raise ComparisonError("native_block_invalid")
        text = block.get("block_content")
        label = block.get("block_label")
        block_id = block.get("block_id")
        if not isinstance(text, str) or not text.strip():
            continue
        if not isinstance(label, str) or not label or block_id is None:
            raise ComparisonError("native_block_provenance_missing")
        block_key = str(block_id)
        if block_key in block_ids:
            raise ComparisonError("native_block_id_duplicate")
        block_ids.add(block_key)
        quad = _quad(block.get("block_polygon_points"), width, height)
        all_x.extend(point[0] for point in quad)
        all_y.extend(point[1] for point in quad)
        line_id = f"vl-block-{block_key}"
        # score=0 is only a required-schema sentinel; PaddleOCR-VL provides no
        # line confidence and the Go extractor does not use this value.
        lines.append({
            "id": line_id, "pass_id": "original", "text_raw": text,
            "score": 0.0, "quad": quad,
        })
        map_rows.append({
            "line_id": line_id, "native_block_id": block_key, "block_label": label,
            "block_bbox": block.get("block_bbox"), "whole_block_polygon": quad,
            "text_sha256": hashlib.sha256(text.encode("utf-8")).hexdigest(),
            "geometry_scope": "whole_layout_block",
        })
    if not lines:
        raise ComparisonError("no_textual_native_blocks")

    worker_versions = worker_meta.get("versions") or {}
    configured_seconds = worker_meta.get("predict_seconds")
    elapsed = float(configured_seconds) if isinstance(configured_seconds, (int, float)) and math.isfinite(configured_seconds) and configured_seconds >= 0 else 0.0
    if elapsed == 0.0:
        raise ComparisonError("native_predict_duration_unavailable")
    document = {
        "bounds": [min(all_x), min(all_y), max(all_x), max(all_y)],
        "method": "ocr_bbox_envelope", "quality": "candidate", "seam_y": None,
    }
    identity = [[1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [0.0, 0.0, 1.0]]
    observation = {
        "schema_version": OBSERVATIONS_SCHEMA,
        "image": {
            "id": sample_id, "width": width, "height": height, "sha256": source_hash,
            "coordinate_frame": "exif_normalized_pixels",
        },
        "document": document,
        "passes": [{
            "id": "original", "kind": "original", "crop": [0, 0, width, height],
            "rotation_degrees": 0, "transform_to_image": identity,
            "elapsed_seconds": elapsed, "status": "ok",
        }],
        "lines": lines,
        "runtime": {
            "source_engine": "paddleocr_vl", "model": "PaddleOCR-VL-1.6",
            "pipeline_version": "v1.6", "paddleocr_version": worker_versions.get("paddleocr", "3.7.0"),
            "paddlepaddle_version": worker_versions.get("paddle", "3.2.1"),
            "geometry_scope": "layout_block", "score_policy": "unavailable_zero_sentinel_not_confidence",
            "adapter_version": "paddleocr-vl-go-block-adapter/v1",
            "physical_contour_detected": False, "page_seam_detected": False,
            "independent_mrz_pass_available": False,
            "native_result_sha256": result_sha256,
            "elapsed_seconds_boundary": "native_worker_predict_seconds_from_cached_manifest; historical, not measured in Go run",
        },
    }
    sidecar = {
        "schema_version": "paddleocr-vl-go-adapter-map/v1", "sample_id": sample_id,
        "input_sha256": source_hash, "native_result_sha256": result_sha256,
        "score_reason": "required observations-v1 numeric field; constant zero sentinel; unavailable is not a confidence value",
        "document_bounds_reason": "envelope of accepted textual layout-block polygons only; not a physical document contour",
        "seam_reason": "no seam metadata emitted by PaddleOCR-VL",
        "line_to_native_block": map_rows,
    }
    return observation, sidecar


def _canonical(value: Any, parent_key: str = "") -> Any:
    if isinstance(value, dict):
        return {key: _canonical(item, key) for key, item in sorted(value.items())}
    if isinstance(value, list):
        items = [_canonical(item) for item in value]
        if parent_key in {"line_ids", "pass_ids"}:
            return sorted(items, key=lambda item: json.dumps(item, sort_keys=True, ensure_ascii=False))
        return items
    return value


def _field_metrics(extraction: dict[str, Any], gold_fields: dict[str, Any]) -> dict[str, Any]:
    # Reuse the incumbent comparator's exact normalization and field rules.
    return score_extraction(extraction, gold_fields)


def _go_command(go_path: Path, observation_path: Path, output_path: Path) -> list[str]:
    return [str(go_path), "--input", str(observation_path), "--output", str(output_path), *GO_FLAGS]


def _safe_go(command: list[str], *, cwd: Path, timeout: float, runner: Callable[..., Any]) -> tuple[Any, float]:
    start = time.monotonic()
    try:
        completed = runner(command, cwd=cwd, timeout=timeout, capture_output=True, text=True, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ComparisonError("go_extractor_failed_or_timed_out") from error
    if completed.returncode != 0:
        raise ComparisonError("go_extractor_failed")
    return completed, time.monotonic() - start


def _evaluate_runs(
    *, trial_root: Path, cache_root: Path, image_root: Path, gold_path: Path, go_path: Path,
    work_root: Path, output_dir: Path, runner: Callable[..., Any] = subprocess.run,
    timeout_seconds: float = 30.0,
) -> dict[str, Any]:
    started = time.monotonic()
    _private_dir(work_root)
    _private_dir(output_dir)
    input_manifest = _json(trial_root / "input-manifest.json", "native_input_manifest_invalid")
    run_manifest = _json(trial_root / "run-manifest.json", "native_run_manifest_invalid")
    pins = run_manifest.get("pins") if isinstance(run_manifest, dict) else None
    if not isinstance(pins, dict) or pins.get("paddleocr") != "3.7.0" or pins.get("paddlepaddle") != "3.2.1" or pins.get("pipeline_version") != "v1.6":
        raise ComparisonError("native_trial_pins_mismatch")
    manifest_items = input_manifest.get("samples") if isinstance(input_manifest, dict) else None
    if not isinstance(manifest_items, list):
        raise ComparisonError("native_input_manifest_invalid")
    by_id = {row.get("sample_id"): row for row in manifest_items if isinstance(row, dict)}
    observations_by_id: dict[str, dict[str, Any]] = {}
    adapter_maps: dict[str, dict[str, Any]] = {}
    source_hashes: dict[str, str] = {}
    input_manifest_items: dict[str, dict[str, Any]] = {}
    native_durations: dict[str, float] = {}
    extraction_durations: dict[str, dict[str, float]] = {}
    go_run_records: list[dict[str, Any]] = []
    historical_cache_parity: dict[str, bool] = {}

    # Prepare/cache adapter input and re-run baseline Go before reading gold.
    for sample_id in SAMPLE_IDS:
        item = by_id.get(sample_id)
        if not isinstance(item, dict):
            raise ComparisonError("native_sample_manifest_incomplete")
        input_manifest_items[sample_id] = item
        source_path = image_root / ("image-01.jpg" if sample_id == "sample-01" else "image-02.jpg")
        manifest_source = item.get("source_path")
        if not isinstance(manifest_source, str) or Path(manifest_source).resolve() != source_path.resolve():
            raise ComparisonError("input_manifest_source_path_mismatch")
        source_hash = _sha256(source_path)
        manifest_hash = (run_manifest.get("input_hashes_private") or {}).get(sample_id)
        if manifest_hash != source_hash:
            raise ComparisonError("native_run_manifest_sha_mismatch")
        result_path = trial_root / sample_id / f"{sample_id}_res.json"
        result_sha = _sha256(result_path)
        result_data = _json(result_path, "native_result_invalid")
        worker_meta = _json(trial_root / sample_id / "worker-metadata.json", "native_worker_metadata_invalid")
        worker_meta["paddleocr_version"] = pins["paddleocr"]
        observation, mapping = build_adapter(
            sample_id=sample_id, source_path=source_path, source_hash=source_hash,
            image_meta=item, result=result_data, worker_meta=worker_meta, result_sha256=result_sha,
        )
        # Core schema validation is exercised by the production Go decoder.
        sample_work = work_root / sample_id
        _private_dir(sample_work)
        observation_path = sample_work / "observations.json"
        baseline_output = sample_work / "baseline-extraction.json"
        _private_json(observation_path, observation)
        _private_json(sample_work / "adapter-map.json", mapping)
        command = _go_command(go_path, observation_path, baseline_output)
        remaining = timeout_seconds - (time.monotonic() - started)
        if remaining <= 0:
            raise ComparisonError("comparison_timeout")
        vl_process, vl_go_seconds = _safe_go(command, cwd=ROOT, timeout=min(remaining, 15.0), runner=runner)
        go_run_records.append({"sample_id": sample_id, "arm": "paddleocr_vl", "exit_code": vl_process.returncode, "elapsed_seconds": round(vl_go_seconds, 6)})
        vl_extraction = _json(baseline_output, "vl_go_output_invalid")

        cached_observation_path = cache_root / sample_id / "baseline/observations.json"
        cached_extraction_path = cache_root / sample_id / "baseline/extraction.json"
        cached_observation = _json(cached_observation_path, "cached_baseline_observations_invalid")
        cached_meta = cached_observation.get("image", {})
        if cached_meta.get("sha256") != source_hash or cached_meta.get("id") != sample_id:
            raise ComparisonError("cached_baseline_image_mismatch")
        baseline_output_path = sample_work / "incumbent-rerun-extraction.json"
        command = _go_command(go_path, cached_observation_path, baseline_output_path)
        remaining = timeout_seconds - (time.monotonic() - started)
        if remaining <= 0:
            raise ComparisonError("comparison_timeout")
        incumbent_process, incumbent_go_seconds = _safe_go(command, cwd=ROOT, timeout=min(remaining, 15.0), runner=runner)
        go_run_records.append({"sample_id": sample_id, "arm": "incumbent_replay", "exit_code": incumbent_process.returncode, "elapsed_seconds": round(incumbent_go_seconds, 6)})
        rerun = _json(baseline_output_path, "incumbent_go_output_invalid")
        cached_extraction = _json(cached_extraction_path, "cached_baseline_extraction_invalid")
        historical_cache_parity[sample_id] = _canonical(rerun) == _canonical(cached_extraction)
        observations_by_id[sample_id] = {"vl": vl_extraction, "incumbent": rerun, "cached": cached_extraction}
        adapter_maps[sample_id] = mapping
        source_hashes[sample_id] = source_hash
        native_durations[sample_id] = float(worker_meta["predict_seconds"])
        extraction_durations[sample_id] = {"vl": vl_go_seconds, "incumbent": incumbent_go_seconds}

    # Gold is intentionally read only after both VL and incumbent Go arms exist.
    gold = _json(gold_path, "gold_invalid")
    gold_images = gold.get("images") if isinstance(gold, dict) else None
    if not isinstance(gold_images, dict):
        raise ComparisonError("gold_schema_invalid")
    matches_by_hash: dict[str, list[dict[str, Any]]] = {}
    for entry in gold_images.values():
        if not isinstance(entry, dict) or not isinstance(entry.get("source_image_path"), str):
            continue
        path = Path(entry["source_image_path"])
        if path.is_file():
            matches_by_hash.setdefault(_sha256(path), []).append(entry)
    mapped: dict[str, dict[str, Any]] = {}
    for sample_id, digest in source_hashes.items():
        matches = matches_by_hash.get(digest, [])
        if len(matches) != 1:
            raise ComparisonError("gold_source_mapping_not_unique")
        mapped[sample_id] = matches[0]

    result_samples: list[dict[str, Any]] = []
    for sample_id in SAMPLE_IDS:
        gold_fields = mapped[sample_id].get("fields")
        if not isinstance(gold_fields, dict):
            raise ComparisonError("gold_fields_invalid")
        scored = {
            arm: _field_metrics(observations_by_id[sample_id][arm], gold_fields)
            for arm in ("incumbent", "vl")
        }
        result_samples.append({
            "sample_id": sample_id,
            "native_textual_block_count": len(adapter_maps[sample_id]["line_to_native_block"]),
            "native_predict_seconds_historical": round(native_durations[sample_id], 3),
            "historical_cached_extraction_semantic_parity": historical_cache_parity[sample_id],
            "arms": {arm: {
                "required_fields": scored[arm]["totals"]["required_expected_present"],
                "required_exact_selected": scored[arm]["totals"]["required_suggested_correct"],
                "required_wrong_suggestion": scored[arm]["totals"]["required_wrong_suggestion"],
                "required_abstain": scored[arm]["totals"]["required_missing_or_abstained"],
                "required_false_recognized": scored[arm]["totals"]["required_false_recognized"],
                "optional_patronymic_state": scored[arm]["fields"]["patronymic"]["state"],
                "optional_patronymic_exact_selected": scored[arm]["fields"]["patronymic"]["suggested_correct"],
                "go_extraction_seconds": round(extraction_durations[sample_id][arm], 6),
                "fields": {
                    field_id: {
                        "status": (
                            "correct" if scored[arm]["fields"][field_id]["suggested_correct"] else
                            "abstain" if not scored[arm]["fields"][field_id]["value_present"] or scored[arm]["fields"][field_id]["state"] == "missing" else
                            "wrong"
                        ),
                        "state": scored[arm]["fields"][field_id]["state"],
                        "conflict": "conflicting_field_candidates" in (
                            observations_by_id[sample_id][arm].get("fields", {}).get(field_id, {}).get("issues") or []
                        ),
                    }
                    for field_id in scored[arm]["fields"]
                },
            } for arm in ("incumbent", "vl")},
        })
    overall: dict[str, Any] = {}
    for arm in ("incumbent", "vl"):
        arm_rows = [sample["arms"][arm] for sample in result_samples]
        overall[arm] = {
            "required_fields": sum(row["required_fields"] for row in arm_rows),
            "required_exact_selected": sum(row["required_exact_selected"] for row in arm_rows),
            "required_wrong_suggestion": sum(row["required_wrong_suggestion"] for row in arm_rows),
            "required_abstain": sum(row["required_abstain"] for row in arm_rows),
            "required_false_recognized": sum(row["required_false_recognized"] for row in arm_rows),
            "optional_patronymic_exact_selected": sum(row["optional_patronymic_exact_selected"] for row in arm_rows),
        }
    output = {
        "schema_version": SCHEMA, "status": "PASS",
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "scope": "two development images; selected-field Go extraction on cached OCR versus cached PaddleOCR-VL blocks; not held-out accuracy",
        "go_options": {"passes": "all", "normalize_names": True, "mrz_policy": "augment"},
        "gold_loaded_after_all_four_go_runs": True,
        "baseline_replay": "cached incumbent observations rerun through same Go binary/options and matched extraction semantics",
        "historical_cached_extraction_parity": historical_cache_parity,
        "native_inference": "cached PaddleOCR-VL outputs only; no model/runtime invocation in this comparison",
        "native_timing_boundary": "historical PaddleOCR-VL predict_seconds from original worker manifests; not Go or adapter elapsed time",
        "adapter_contract": {
            "geometry": "one full native text block per OCR line; native whole-block polygon preserved; no word or line splitting",
            "document_bounds": "envelope of accepted textual block polygons; provisional OCR bbox envelope, not a physical contour; no seam",
            "score": "zero is required-schema unavailable sentinel; not confidence; Go extractor does not use it",
            "mrz": "no independent MRZ pass supplied; Go augment policy only examines native original-pass blocks under existing rules",
        },
        "evaluation": {
            "required_fields": list(REQUIRED_FIELDS), "optional_field": "patronymic",
            "normalization": "reused existing run_algorithm_comparison canonicalization; exact selected value only; no oracle candidate selection or fuzzy matching",
            "public_values": False,
        },
        "samples": result_samples,
        "overall": overall,
        "limits": [
            "Whole-block text and geometry differ in granularity from RapidOCR word/line observations.",
            "Only two development images; no production or held-out accuracy claim.",
            "VL inference was measured in a separate completed run and is not included in the offline Go comparison timing.",
            "Go pass-set all has only one native original-image pass in the VL arm.",
        ],
        "timing": {"comparison_wall_seconds": round(time.monotonic() - started, 3)},
    }
    _private_json(work_root / "comparison-private.json", output)
    _private_json(work_root / "source-manifest-private.json", {
        "inputs": {sample_id: {
            "sha256_pre": input_manifest_items[sample_id].get("sha256_pre"),
            "sha256_post": input_manifest_items[sample_id].get("sha256_post"),
            "current_sha256": source_hashes[sample_id],
        } for sample_id in SAMPLE_IDS},
        "native_outputs": {sample_id: _sha256(trial_root / sample_id / f"{sample_id}_res.json") for sample_id in SAMPLE_IDS},
        "incumbent_baseline": {sample_id: {
            "observations_sha256": _sha256(cache_root / sample_id / "baseline/observations.json"),
            "historical_extraction_sha256": _sha256(cache_root / sample_id / "baseline/extraction.json"),
            "historical_extraction_semantic_parity": historical_cache_parity[sample_id],
        } for sample_id in SAMPLE_IDS},
        "go_binary_sha256": _sha256(go_path),
        "adapter_script_sha256": _sha256(Path(__file__).resolve()),
        "go_source_sha256": {
            relative: _sha256(ROOT / relative) for relative in (
                "cmd/ocr-eval/main.go", "internal/passport/extract.go",
                "internal/passport/observations.go", "internal/passport/mrz_integration.go",
                "ocr/schema/observations-v1.schema.json",
            )
        },
        "gold_sha256": _sha256(gold_path),
    })
    _private_json(work_root / "go-run-ledger-private.json", {
        "flags": list(GO_FLAGS),
        "runs": go_run_records,
        "rejected_attempts": [
            {"reason": "preexisting cached binary lacked mrz-policy flag; discarded", "argv_flags": ["--normalize-names", "true", "--mrz-policy", "augment"]},
            {"reason": "first current-binary bool flag form stopped flag parsing before mrz-policy; semantic parity check rejected; rerun corrected", "argv_flags": ["--normalize-names", "true", "--mrz-policy", "augment"]},
        ],
        "go_binary_sha256": _sha256(go_path),
    })
    # No image hash, paths, OCR text, candidate values, or gold literals go public.
    public = {key: output[key] for key in (
        "schema_version", "status", "generated_at_utc", "scope", "go_options", "gold_loaded_after_all_four_go_runs",
        "baseline_replay", "native_inference", "native_timing_boundary", "adapter_contract",
        "historical_cached_extraction_parity", "evaluation", "samples", "overall", "limits", "timing",
    )}
    _private_json(output_dir / "paddleocr-vl-go-comparison.json", public)
    md = [
        "# PaddleOCR-VL blocks through the Go extractor", "",
        f"Generated: `{output['generated_at_utc']}`.", "",
        "Two development images; cached native PaddleOCR-VL outputs only. The Go extractor reran with the same flags on both the incumbent cached observations and VL-adapted blocks.",
        "Gold was read after all four Go runs. OCR text, extracted values, and gold literals remain private.", "",
        "| Sample | Input | Required exact selected | Wrong suggestion | Abstain | Optional patronymic | Historical VL predict (s) |",
        "|---|---|---:|---:|---:|---|---:|",
    ]
    for row in result_samples:
        vl, inc = row["arms"]["vl"], row["arms"]["incumbent"]
        md.append(
            f"| {row['sample_id']} | VL {row['native_textual_block_count']} blocks / incumbent | "
            f"VL {vl['required_exact_selected']}/10 / incumbent {inc['required_exact_selected']}/10 | "
            f"VL {vl['required_wrong_suggestion']} / incumbent {inc['required_wrong_suggestion']} | "
            f"VL {vl['required_abstain']} / incumbent {inc['required_abstain']} | "
            f"VL {vl['optional_patronymic_state']} ({str(vl['optional_patronymic_exact_selected']).lower()}) / "
            f"incumbent {inc['optional_patronymic_state']} ({str(inc['optional_patronymic_exact_selected']).lower()}) | "
            f"{row['native_predict_seconds_historical']:.3f} |"
        )
    md.extend(["", "## Per-field results", "", "| Sample | Arm | Field | Outcome | Go state | Conflict |", "|---|---|---|---|---|---|"])
    for row in result_samples:
        for arm in ("incumbent", "vl"):
            label = "incumbent" if arm == "incumbent" else "PaddleOCR-VL"
            for field_id, field_result in row["arms"][arm]["fields"].items():
                md.append(f"| {row['sample_id']} | {label} | `{field_id}` | {field_result['status']} | `{field_result['state']}` | {str(field_result['conflict']).lower()} |")
    md.extend(["", "This is a selected-field extractor comparison, not a comparison of raw OCR readability or general accuracy.",
               "VL supplied one original-image pass and no independent MRZ pass. Its whole-block geometry is coarser than the incumbent OCR lines.", ""])
    path = output_dir / "paddleocr-vl-go-comparison.md"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write("\n".join(md))
    return public


def _self_test() -> None:
    """Pure, model-free adapter invariants, using synthetic data only."""
    import copy
    import tempfile
    from types import SimpleNamespace
    from unittest.mock import patch

    with tempfile.TemporaryDirectory() as temporary:
        source = Path(temporary) / "input.jpg"
        source.write_bytes(b"not-decoded-in-this-test")
        text = "first line\nsecond line"
        result = {"width": 100, "height": 200, "parsing_res_list": [{
            "block_id": 3, "block_label": "text", "block_content": text,
            "block_bbox": [10, 20, 60, 80],
            "block_polygon_points": [[10, 20], [60, 20], [60, 80], [10, 80]],
        }]}
        meta = {"sample_id": "sample-01", "sha256_pre": "a" * 64, "sha256_post": "a" * 64}
        worker = {"returncode": 0, "predict_seconds": 2.5, "configured": {
            "use_doc_orientation_classify": False, "use_doc_unwarping": False, "pipeline_version": "v1.6",
        }, "versions": {}}
        original_result = copy.deepcopy(result)
        with patch(__name__ + "._sha256", return_value="a" * 64), patch(__name__ + "._normalized_image", return_value=(100, 200, 1)):
            obs, mapping = build_adapter(sample_id="sample-01", source_path=source, source_hash="a" * 64,
                                         image_meta=meta, result=result, worker_meta=worker, result_sha256="b" * 64)
        assert result == original_result
        assert len(obs["lines"]) == 1 and obs["lines"][0]["text_raw"] == text
        assert obs["lines"][0]["quad"] == [[10.0, 20.0], [60.0, 20.0], [60.0, 80.0], [10.0, 80.0]]
        assert obs["lines"][0]["score"] == 0 and obs["runtime"]["score_policy"].startswith("unavailable_")
        assert not any(item["kind"] == "mrz" for item in obs["passes"])
        assert obs["document"]["bounds"] == [10.0, 20.0, 60.0, 80.0] and obs["document"]["seam_y"] is None
        assert mapping["line_to_native_block"][0]["native_block_id"] == "3"
        assert _go_command(Path("go"), Path("in.json"), Path("out.json")) == [
            "go", "--input", "in.json", "--output", "out.json", "--passes=all",
            "--normalize-names=true", "--mrz-policy", "augment",
        ]
        assert _canonical({"line_ids": ["b", "a"], "pass_ids": ["p2", "p1"]}) == _canonical({"line_ids": ["a", "b"], "pass_ids": ["p1", "p2"]})
        assert _canonical({"candidates": [{"value": "first"}, {"value": "second"}]}) != _canonical({"candidates": [{"value": "second"}, {"value": "first"}]})
        assert _canonical({"raw_variants": [["first"], ["second"]]}) != _canonical({"raw_variants": [["second"], ["first"]]})
        for bad_result, digest, orientation, expected_code in (
            (dict(result, width=101), "a" * 64, 1, "native_result_dimensions_mismatch"),
            (result, "c" * 64, 1, "input_image_sha_mismatch"),
            (result, "a" * 64, 6, "source_exif_orientation_not_identity"),
            ({**result, "parsing_res_list": [{**result["parsing_res_list"][0], "block_polygon_points": [[-1, 20], [60, 20], [60, 80], [10, 80]]}]}, "a" * 64, 1, "native_block_polygon_out_of_bounds"),
        ):
            try:
                with patch(__name__ + "._sha256", return_value="a" * 64), patch(__name__ + "._normalized_image", return_value=(100, 200, orientation)):
                    build_adapter(sample_id="sample-01", source_path=source, source_hash=digest,
                                  image_meta=meta, result=bad_result, worker_meta=worker, result_sha256="b" * 64)
            except ComparisonError as error:
                assert error.code == expected_code, (error.code, expected_code)
            else:
                raise AssertionError(f"{expected_code} did not fail closed")

    # Exercise the orchestration boundary with fake Go and synthetic gold: all
    # four Go calls must finish before the gold path is read, and reports must
    # not expose synthetic OCR/gold strings.
    from PIL import Image
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        trial, cache, images, gold_path = (root / name for name in ("trial", "cache", "images", "gold.json"))
        work, output, fake_go = (root / name for name in ("private", "public", "fake-go"))
        for folder in (trial, cache, images):
            folder.mkdir(mode=0o700)
        fake_go.write_text("fake", encoding="utf-8")
        sample_rows = []
        gold_images = {}
        expected_extraction = {"fields": {
            name: {"state": "missing", "value": None, "rules": []}
            for name in ("surname", "given_name", "patronymic", "gender", "birth_date", "birth_place", "series", "number", "issue_date", "issuer", "department_code")
        }}
        go_events: list[str] = []
        for index, sample_id in enumerate(SAMPLE_IDS):
            image_path = images / ("image-01.jpg" if index == 0 else "image-02.jpg")
            Image.new("RGB", (100, 200), (index * 80, 10, 20)).save(image_path)
            digest = _sha256(image_path)
            sample_rows.append({"sample_id": sample_id, "source_path": str(image_path), "sha256_pre": digest, "sha256_post": digest})
            sample_trial = trial / sample_id
            sample_trial.mkdir(mode=0o700)
            (sample_trial / f"{sample_id}_res.json").write_text(json.dumps({"width": 100, "height": 200, "parsing_res_list": [{
                "block_id": 1, "block_label": "text", "block_content": "SYNTHETIC_PRIVATE_OCR_TEXT",
                "block_bbox": [10, 20, 60, 80], "block_polygon_points": [[10, 20], [60, 20], [60, 80], [10, 80]],
            }]}), encoding="utf-8")
            (sample_trial / "worker-metadata.json").write_text(json.dumps({"returncode": 0, "predict_seconds": 1.25,
                "configured": {"use_doc_orientation_classify": False, "use_doc_unwarping": False, "pipeline_version": "v1.6"}, "versions": {}}), encoding="utf-8")
            cache_sample = cache / sample_id / "baseline"
            cache_sample.mkdir(parents=True, mode=0o700)
            (cache_sample / "observations.json").write_text(json.dumps({"image": {"id": sample_id, "sha256": digest}}), encoding="utf-8")
            (cache_sample / "extraction.json").write_text(json.dumps(expected_extraction), encoding="utf-8")
            fields = {name: {"literal": f"PRIVATE_GOLD_{sample_id}_{name}"} for name in expected_extraction["fields"]}
            gold_images[sample_id] = {"source_image_path": str(image_path), "fields": fields}
        (trial / "input-manifest.json").write_text(json.dumps({"samples": sample_rows}), encoding="utf-8")
        (trial / "run-manifest.json").write_text(json.dumps({"pins": {"paddleocr": "3.7.0", "paddlepaddle": "3.2.1", "pipeline_version": "v1.6"},
            "input_hashes_private": {row["sample_id"]: row["sha256_post"] for row in sample_rows}}), encoding="utf-8")
        gold_path.write_text(json.dumps({"images": gold_images}), encoding="utf-8")
        initial_hashes = {row["sample_id"]: _sha256(Path(row["source_path"])) for row in sample_rows}
        original_read_json = _json
        def tracked_json(path: Path, code: str) -> Any:
            if path == gold_path:
                go_events.append("gold")
            return original_read_json(path, code)
        def fake_runner(command: list[str], **_: Any) -> Any:
            go_events.append("go")
            out = Path(command[command.index("--output") + 1])
            _private_json(out, expected_extraction)
            return SimpleNamespace(returncode=0, stdout="completed", stderr="")
        with patch(__name__ + "._json", side_effect=tracked_json):
            public = _evaluate_runs(trial_root=trial, cache_root=cache, image_root=images, gold_path=gold_path,
                                    go_path=fake_go, work_root=work, output_dir=output, runner=fake_runner)
        assert go_events == ["go", "go", "go", "go", "gold"]
        assert all(_sha256(Path(row["source_path"])) == initial_hashes[row["sample_id"]] for row in sample_rows)
        public_blob = (output / "paddleocr-vl-go-comparison.json").read_text(encoding="utf-8") + (output / "paddleocr-vl-go-comparison.md").read_text(encoding="utf-8")
        assert "SYNTHETIC_PRIVATE_OCR_TEXT" not in public_blob
        assert "PRIVATE_GOLD_" not in public_blob
        assert public["overall"]["vl"]["required_fields"] == 20


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        _self_test()
        verification = DEFAULT_WORK / "verification"
        _private_dir(verification)
        for name, content in (
            ("adapter-self-test.log", "adapter self-test PASS\n"),
            ("adapter-self-test.exit-code.txt", "0\n"),
            ("adapter-self-test.command.txt", "python ocr/experiments/run_paddleocr_vl_go_comparison.py --self-test\n"),
        ):
            path = verification / name
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(content)
        print("adapter self-test PASS")
    else:
        _evaluate_runs(trial_root=DEFAULT_TRIAL_ROOT, cache_root=DEFAULT_CACHE_ROOT,
                       image_root=DEFAULT_IMAGE_ROOT, gold_path=DEFAULT_GOLD, go_path=DEFAULT_GO,
                       work_root=DEFAULT_WORK, output_dir=DEFAULT_OUTPUT)
