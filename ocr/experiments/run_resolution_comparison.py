#!/usr/bin/env python3
"""Compare configured primary detector resolution on two private images.

Raw OCR and extraction stay in a private work directory. The larger mode uses
the selected primary profile for original/document/page passes and the native
736/min profile for right-margin and MRZ passes to bound narrow-crop growth.
"""

from __future__ import annotations

import argparse
import importlib
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import ARCHIVE_OUTPUT_ROOT, ARCHIVE_WORK_ROOT  # noqa: E402
from ocr.experiments import run_algorithm_comparison as base

DEFAULT_WORK = ARCHIVE_WORK_ROOT / "ocr-resolution-work"
DEFAULT_OUTPUT = ARCHIVE_OUTPUT_ROOT / "ocr-resolution-comparison"
LARGER_LIMIT_SIDE_LEN = 1280
SCHEMA_V2 = "passport-ocr-observations/v2"
REQUIRED_FIELDS = tuple(base.REQUIRED_FIELDS)
VARIANT_NATIVE = "native_config"
VARIANT_LARGER = "larger_primary_detector_min_side_1280"
TIMING_BOUNDARY = (
    "OCR seconds measure per-image observation processing and all configured passes, including image reads; engine "
    "construction/model setup happens before timing and the engines are reused sequentially across both images. "
    "Go extraction seconds measure post-processing only. Det.limit_type remains min; the larger profile applies only "
    "to original/document/page passes. Right-margin and RapidOCR MRZ passes use the native detector profile. "
    "Peak RSS was not measured."
)


class ResolutionComparisonError(base.ComparisonError):
    """Sanitized resolution comparison failure."""


def larger_limit(default_limit: int) -> int:
    if isinstance(default_limit, bool) or not isinstance(default_limit, int):
        raise ResolutionComparisonError("installed_detector_limit_invalid")
    if default_limit >= LARGER_LIMIT_SIDE_LEN:
        raise ResolutionComparisonError("installed_detector_limit_not_below_target")
    return LARGER_LIMIT_SIDE_LEN


def _assert_provider_report(report: Any) -> None:
    sessions = report.get("sessions") if isinstance(report, dict) else None
    if not isinstance(sessions, dict) or any(sessions.get(role) != ["CPUExecutionProvider"] for role in ("det", "rec")):
        raise ResolutionComparisonError("actual_ocr_provider_not_cpu")


def public_required_counts(scored: dict[str, Any]) -> dict[str, int]:
    fields = scored.get("fields")
    if not isinstance(fields, dict):
        raise ResolutionComparisonError("scorer_fields_invalid")
    counts = {"correct": 0, "wrong": 0, "abstain": 0}
    for name in REQUIRED_FIELDS:
        field = fields.get(name)
        if not isinstance(field, dict):
            raise ResolutionComparisonError("scorer_required_field_missing")
        if field.get("suggested_correct") is True:
            counts["correct"] += 1
        elif field.get("value_present") is True:
            counts["wrong"] += 1
        else:
            counts["abstain"] += 1
    if sum(counts.values()) != 10:
        raise ResolutionComparisonError("required_count_total_mismatch")
    return counts


def _round32(value: float) -> int:
    return int(round(value / 32) * 32)


def _resize_dimensions(width: int, height: int, scale: float) -> tuple[int, int]:
    """Match RapidOCR's int truncation followed by nearest-multiple-of-32 rounding."""
    resize_h = int(height * scale)
    resize_w = int(width * scale)
    return _round32(resize_w), _round32(resize_h)


def estimate_detector_pixels(width: int, height: int, config: dict[str, Any], rotation: int = 0) -> int | None:
    """Estimate installed RapidOCR pre-detector shape, including configured vertical padding."""
    if rotation in (90, 270):
        width, height = height, width
    required = (
        "global_use_preprocess_img", "global_min_side_len", "global_max_side_len",
        "global_use_vertical_padding", "global_min_height", "global_width_height_ratio",
        "limit_side_len", "limit_type",
    )
    if any(config.get(key) is None for key in required) or width <= 0 or height <= 0:
        return None
    if config["limit_type"] != "min":
        raise ResolutionComparisonError("unsupported_detector_limit_type")
    if config["global_use_preprocess_img"]:
        max_side = float(config["global_max_side_len"])
        if max(width, height) > max_side:
            scale = max_side / max(width, height)
            width, height = _resize_dimensions(width, height, scale)
        min_side = float(config["global_min_side_len"])
        if min(width, height) < min_side:
            scale = min_side / min(width, height)
            width, height = _resize_dimensions(width, height, scale)
    if config["global_use_vertical_padding"]:
        ratio = float(config["global_width_height_ratio"])
        if height <= float(config["global_min_height"]) or width / height > ratio:
            target_height = max(int(width / ratio), int(config["global_min_height"])) * 2
            padding = int(abs(target_height - height) / 2)
            height += 2 * padding
    if min(width, height) < int(config["limit_side_len"]):
        scale = float(config["limit_side_len"]) / min(width, height)
        width, height = _resize_dimensions(width, height, scale)
    return width * height


def estimate_pass_peaks(observation: dict[str, Any]) -> dict[str, Any]:
    runtime = observation.get("runtime", {})
    profiles = runtime.get("detector_profiles", {})
    assignments = runtime.get("pass_detector_profiles", {})
    peaks: dict[str, int] = {}
    for item in observation.get("passes", []):
        if not isinstance(item, dict) or not isinstance(item.get("crop"), list) or len(item["crop"]) != 4:
            continue
        x0, y0, x1, y1 = item["crop"]
        profile_name = assignments.get(item.get("id"))
        profile_config = profiles.get(profile_name)
        if not isinstance(profile_config, dict):
            continue
        pixels = estimate_detector_pixels(x1 - x0, y1 - y0, profile_config, item.get("rotation_degrees", 0))
        if pixels is not None:
            peaks[profile_name] = max(peaks.get(profile_name, 0), pixels)
    return {"max_pixels_by_profile_estimate": peaks, "max_pixels_estimate": max(peaks.values(), default=None)}


def source_hashes() -> dict[str, str]:
    hashes = {
        "region_pipeline": base.sha256_file(ROOT / "ocr/experiments/region_pipeline.py"),
        "resolution_runner": base.sha256_file(Path(__file__).resolve()),
        "scorer": base.sha256_file(ROOT / "ocr/experiments/run_algorithm_comparison.py"),
    }
    module_names = (
        "rapidocr.main", "rapidocr.ch_ppocr_det.utils", "rapidocr.utils.process_img",
    )
    for module_name in module_names:
        module = importlib.import_module(module_name)
        module_path = Path(module.__file__).resolve()
        hashes[f"installed_{module_name.replace('.', '_')}"] = base.sha256_file(module_path)
    rapidocr_package = importlib.import_module("rapidocr")
    hashes["installed_rapidocr_config"] = base.sha256_file(Path(rapidocr_package.__file__).resolve().parent / "config.yaml")
    return hashes


def _run(args: argparse.Namespace) -> dict[str, Any]:
    python, go = args.python.expanduser().resolve(), args.go.expanduser().resolve()
    tesseract = args.tesseract.expanduser().resolve()
    work_root, output_dir = args.work_root.expanduser().resolve(), args.output_dir.expanduser().resolve()
    gold_path, model_dir = args.gold.expanduser().resolve(), args.model_dir.expanduser().resolve()
    images = [args.image1.expanduser().resolve(), args.image2.expanduser().resolve()]
    if len(set(images)) != 2:
        raise ResolutionComparisonError("exactly_two_distinct_images_required")
    if any(not path.is_file() for path in [*images, gold_path, tesseract]):
        raise ResolutionComparisonError("explicit_input_file_missing")
    base.private_dir(work_root)
    base.private_dir(output_dir)
    go_version, python_versions = base.check_toolchain(python, go, model_dir)
    gold = json.loads(gold_path.read_text(encoding="utf-8"))
    gold_images = gold.get("images") if isinstance(gold, dict) else None
    if not isinstance(gold_images, dict):
        raise ResolutionComparisonError("ground_truth_schema_invalid")
    matched = [(f"sample-{index:02d}", image, base.image_key(image.stem, gold_images)) for index, image in enumerate(images, 1)]
    if len({item[2] for item in matched}) != 2:
        raise ResolutionComparisonError("images_map_to_duplicate_ground_truth_records")
    base.private_json(work_root / "private-input-manifest.json", {
        "schema_version": "ocr-resolution-private-input-manifest/v1",
        "images": [
            {"sample_id": image_id, "source_path": str(path), "sha256": base.sha256_file(path), "gold_key": key}
            for image_id, path, key in matched
        ],
    })

    env = os.environ.copy()
    env.update({"GOCACHE": str(work_root / "go-cache"), "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})
    base.private_dir(work_root / "go-cache")
    evaluator = work_root / "ocr-eval"
    build_output = base.run_go(go, ["build", "-o", str(evaluator), "./cmd/ocr-eval"], env=env, timeout=300, phase="go_build")
    if not evaluator.is_file():
        raise ResolutionComparisonError("go_evaluator_binary_not_created")
    evaluator.chmod(0o700)
    base.private_json(work_root / "private-go-build.json", {"go_version": go_version, "output": build_output})

    try:
        from ocr.experiments import region_pipeline as pipeline
    except Exception as error:
        raise ResolutionComparisonError("region_pipeline_unavailable") from error
    det_model, rec_model = model_dir / base.MODEL_HASHES["det"][0], model_dir / base.MODEL_HASHES["rec"][0]
    native_engine = pipeline.create_cpu_engine(det_model, rec_model)
    native_config = pipeline._engine_detector_config(native_engine)
    default_limit = native_config.get("limit_side_len")
    target_limit = larger_limit(default_limit)
    if native_config.get("limit_type") != "min":
        raise ResolutionComparisonError("installed_detector_limit_type_unexpected")
    large_engine = pipeline.create_cpu_engine(det_model, rec_model, det_limit_side_len=target_limit)
    large_config = pipeline._engine_detector_config(large_engine)
    if (
        large_config.get("limit_side_len") != target_limit
        or large_config.get("limit_type") != native_config.get("limit_type")
        or any(large_config.get(key) != native_config.get(key) for key in (
            "global_min_side_len", "global_max_side_len", "global_use_preprocess_img",
            "global_use_vertical_padding", "global_min_height", "global_width_height_ratio",
        ))
    ):
        raise ResolutionComparisonError("larger_detector_config_mismatch")

    variants = (
        (VARIANT_NATIVE, native_engine, None, native_config),
        (VARIANT_LARGER, large_engine, native_engine, large_config),
    )
    results: list[dict[str, Any]] = []
    provider_reports: dict[str, Any] = {}
    for variant, primary_engine, auxiliary_engine, primary_config in variants:
        for sample_id, image_path, gold_key in matched:
            started = time.perf_counter()
            observation = pipeline.build_observations(
                image_path=image_path, image_id=sample_id, det_model=det_model, rec_model=rec_model,
                engine=primary_engine, auxiliary_engine=auxiliary_engine, enhanced=True, include_mrz=True,
                tesseract_path=tesseract,
                det_limit_side_len=(target_limit if variant == VARIANT_LARGER else None),
            )
            ocr_seconds = time.perf_counter() - started
            runtime = observation.get("runtime", {})
            expected_models = {"detector": base.MODEL_HASHES["det"][1], "recognizer": base.MODEL_HASHES["rec"][1]}
            expected_aux = native_config if variant == VARIANT_LARGER else None
            if (
                observation.get("schema_version") != SCHEMA_V2
                or observation.get("image", {}).get("id") != sample_id
                or observation.get("image", {}).get("sha256") != base.sha256_file(image_path)
                or runtime.get("model_sha256") != expected_models
                or runtime.get("detector_profiles") != {"primary": primary_config, "auxiliary": expected_aux}
                or not any(item.get("kind") == "mrz" for item in observation.get("passes", []) if isinstance(item, dict))
            ):
                raise ResolutionComparisonError("observation_source_or_config_mismatch")
            _assert_provider_report(runtime.get("providers"))
            base.assert_cpu_engine(primary_engine)
            if auxiliary_engine is not None:
                _assert_provider_report(runtime.get("auxiliary_providers"))
                base.assert_cpu_engine(auxiliary_engine)
            provider_reports[variant] = {"primary": runtime.get("providers"), "auxiliary": runtime.get("auxiliary_providers")}
            estimate = estimate_pass_peaks(observation)
            observation_path = work_root / variant / sample_id / "observations.json"
            base.private_json(observation_path, observation)
            extraction_path = work_root / variant / sample_id / "extraction.json"
            command = base.evaluator_command(evaluator, observation_path, extraction_path, "all", True)
            command.append("--mrz-policy=augment")
            extraction_started = time.perf_counter()
            base.run_executable(Path(command[0]), command[1:], env=env, timeout=60, phase="go_extract")
            extraction_seconds = time.perf_counter() - extraction_started
            extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
            if extraction.get("schema_version") != "passport-extraction/v1" or extraction.get("image_id") != sample_id:
                raise ResolutionComparisonError("go_extraction_schema_or_image_id_mismatch")
            scored = base.score_extraction(extraction, gold_images[gold_key].get("fields", {}))
            states = [field.get("state") for field in extraction.get("fields", {}).values() if isinstance(field, dict)]
            results.append({
                "variant": variant,
                "image_id": sample_id,
                "required_counts": public_required_counts(scored),
                "optional_patronymic": base.patronymic_status(scored["fields"]["patronymic"]),
                "field_state_counts": {state: states.count(state) for state in ("recognized", "uncertain", "missing")},
                "all_fields_uncertain": bool(states) and all(state == "uncertain" for state in states),
                "manual_review_required": True,
                "required_false_fill_count": scored["totals"]["required_false_fill"],
                "detector_pixel_estimate": estimate,
                "ocr_seconds": round(ocr_seconds, 6),
                "extraction_seconds": round(extraction_seconds, 6),
            })

    report = {
        "schema_version": "passport-ocr-resolution-comparison/v1",
        "status": "PASS",
        "scope": "two development images; no general accuracy claim",
        "comparison_boundary": TIMING_BOUNDARY,
        "observations_reused": False,
        "ocr_timing": "measured_fresh_per_image_excluding_engine_setup",
        "detector_profiles": {"native": native_config, "larger_primary": large_config},
        "toolchain": {"go": go_version, "python_packages": python_versions, "providers_by_variant": provider_reports},
        "model_sha256": {role: values[1] for role, values in base.MODEL_HASHES.items()},
        "source_sha256": source_hashes(),
        "results": results,
    }
    base.private_json(work_root / "private-resolution-comparison-details.json", report)
    base.private_json(output_dir / "ocr-resolution-comparison.json", report)
    rows = [
        "# Detector minimum-side resolution comparison", "",
        "Two development images only; this is not a held-out accuracy estimate or a general accuracy claim.",
        TIMING_BOUNDARY,
        "Raw OCR, extracted values, image paths and gold literals remain in the private work directory.", "",
        "| Variant | Image | Required correct / wrong / abstain (10) | Optional patronymic | States recognized / uncertain / missing | Max detector MP estimate | OCR s | Go s |",
        "|---|---|---:|---|---:|---:|---:|---:|",
    ]
    for item in results:
        counts, states = item["required_counts"], item["field_state_counts"]
        pixels = item["detector_pixel_estimate"]["max_pixels_estimate"]
        peak = f"{pixels / 1e6:.2f}" if pixels is not None else "unavailable"
        rows.append(
            f"| {item['variant']} | {item['image_id']} | {counts['correct']} / {counts['wrong']} / {counts['abstain']} "
            f"| {item['optional_patronymic']} | {states['recognized']} / {states['uncertain']} / {states['missing']} "
            f"| {peak} | {item['ocr_seconds']:.3f} | {item['extraction_seconds']:.3f} |"
        )
    rows.extend(["", "All fields require human review. Detector pixel counts are estimates from the installed resize settings; peak RAM was not measured.", ""])
    base.write_public(output_dir / "ocr-resolution-comparison.md", "\n".join(rows))
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, type=Path)
    parser.add_argument("--go", required=True, type=Path)
    parser.add_argument("--tesseract", required=True, type=Path)
    parser.add_argument("--image1", required=True, type=Path)
    parser.add_argument("--image2", required=True, type=Path)
    parser.add_argument("--gold", required=True, type=Path)
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--work-root", type=Path, default=DEFAULT_WORK)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    args = parser.parse_args(argv)
    try:
        if Path(sys.executable).resolve() != args.python.expanduser().resolve():
            raise ResolutionComparisonError("runner_interpreter_does_not_match_requested_python")
        _run(args)
    except (ResolutionComparisonError, base.ComparisonError, OSError, ValueError, TypeError, KeyError, AttributeError, json.JSONDecodeError) as error:
        print(f"resolution comparison failed: {base.safe_error_text(str(error))}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
