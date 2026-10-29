#!/usr/bin/env python3
"""Compare VIZ-only and VIZ+national-MRZ Go extraction on two private images.

The OCR producer is the existing offline CPU pipeline with its MRZ option
enabled. Gold data is loaded only by this scorer after Go extraction. Raw OCR,
MRZ lines and extraction values remain in the private work directory.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import ARCHIVE_OUTPUT_ROOT, ARCHIVE_WORK_ROOT  # noqa: E402
from ocr.experiments import run_algorithm_comparison as base

DEFAULT_WORK = ARCHIVE_WORK_ROOT / "ocr-mrz-work"
DEFAULT_OUTPUT = ARCHIVE_OUTPUT_ROOT / "ocr-mrz-comparison"
SCHEMA_V2 = "passport-ocr-observations/v2"
MODES = (("viz_only", "off"), ("viz_plus_mrz", "augment"))
TIMING_BOUNDARY = (
    "Both policies use the same v2 observations, including the MRZ OCR passes. "
    "This compares field extraction; it is not a VIZ-only versus VIZ+MRZ end-to-end speed comparison. "
    "Fresh OCR seconds cover the full pipeline; extraction seconds cover Go post-processing only. "
    "When observations are reused, OCR time is historical and not measured in this run."
)


class MRZComparisonError(base.ComparisonError):
    """Sanitized comparison failure."""


def load_v2_observations(work_root: Path, matched: list[tuple[str, Path, str]]) -> dict[str, Path]:
    result: dict[str, Path] = {}
    expected_models = {
        "detector": base.MODEL_HASHES["det"][1],
        "recognizer": base.MODEL_HASHES["rec"][1],
    }
    for sample_id, image_path, _ in matched:
        path = work_root / sample_id / "observations.json"
        try:
            observation = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise MRZComparisonError("reusable_observations_missing_or_invalid") from error
        image = observation.get("image") if isinstance(observation, dict) else None
        runtime = observation.get("runtime") if isinstance(observation, dict) else None
        passes = observation.get("passes") if isinstance(observation, dict) else None
        if (
            observation.get("schema_version") != SCHEMA_V2
            or not isinstance(image, dict)
            or image.get("id") != sample_id
            or image.get("sha256") != base.sha256_file(image_path)
            or image.get("coordinate_frame") != "exif_normalized_pixels"
            or not isinstance(runtime, dict)
            or runtime.get("model_sha256") != expected_models
            or not isinstance(passes, list)
            or not any(isinstance(item, dict) and item.get("kind") == "mrz" for item in passes)
        ):
            raise MRZComparisonError("reusable_observations_mismatch")
        result[sample_id] = path
    return result


def public_mrz_summary(extraction: dict[str, Any]) -> dict[str, Any]:
    """Return counts and safe codes only; never expose raw lines or field values."""
    diagnostics = extraction.get("mrz")
    counts = {"valid": 0, "invalid": 0, "indeterminate": 0}
    checks: dict[str, dict[str, int]] = {}
    issues: set[str] = set()
    name_issues: set[str] = set()
    rules: dict[str, int] = {}
    field_issues: set[str] = set()
    if not isinstance(diagnostics, dict):
        return {"enabled": False, "status_counts": counts, "checks": checks, "issues": [], "name_issues": [], "field_issues": [], "rule_counts": {}}
    is_code = lambda value: isinstance(value, str) and re.fullmatch(r"[a-z][a-z0-9_]*", value) is not None
    for candidate in diagnostics.get("candidates", []):
        if not isinstance(candidate, dict):
            continue
        parsed = candidate.get("result")
        if not isinstance(parsed, dict):
            continue
        status = parsed.get("status")
        if status in counts:
            counts[status] += 1
        for check in parsed.get("checks", []):
            if not isinstance(check, dict):
                continue
            name, state = check.get("name"), check.get("status")
            if is_code(name) and is_code(state):
                row = checks.setdefault(name, {})
                row[state] = row.get(state, 0) + 1
        issues.update(code for code in parsed.get("issues", []) if is_code(code))
        name_issues.update(code for code in parsed.get("name_issues", []) if is_code(code))
    issues.update(code for code in diagnostics.get("issues", []) if is_code(code))
    fields = extraction.get("fields")
    if isinstance(fields, dict):
        for field in fields.values():
            if not isinstance(field, dict):
                continue
            field_issues.update(code for code in field.get("issues", []) if is_code(code))
            for code in field.get("rules", []):
                if is_code(code):
                    rules[code] = rules.get(code, 0) + 1
            for candidate in field.get("candidates", []):
                if isinstance(candidate, dict):
                    for code in candidate.get("rules", []):
                        if is_code(code):
                            rules[code] = rules.get(code, 0) + 1
    return {
        "enabled": True,
        "status_counts": counts,
        "checks": checks,
        "issues": sorted(issues),
        "name_issues": sorted(name_issues),
        "field_issues": sorted(field_issues),
        "rule_counts": rules,
    }


def _run(args: argparse.Namespace) -> dict[str, Any]:
    python = args.python.expanduser().resolve()
    go = args.go.expanduser().resolve()
    work_root = args.work_root.expanduser().resolve()
    output_dir = args.output_dir.expanduser().resolve()
    gold_path = args.gold.expanduser().resolve()
    model_dir = args.model_dir.expanduser().resolve()
    images = [path.expanduser().resolve() for path in args.image]
    if len(images) != 2 or len(set(images)) != 2:
        raise MRZComparisonError("exactly_two_distinct_images_required")
    if any(not path.is_file() for path in images) or not gold_path.is_file():
        raise MRZComparisonError("explicit_input_file_missing")
    if args.tesseract and not args.tesseract.expanduser().resolve().is_file():
        raise MRZComparisonError("explicit_tesseract_executable_missing")
    tesseract_path = args.tesseract.expanduser().resolve() if args.tesseract else None
    if not args.reuse_observations and tesseract_path is None:
        detected_tesseract = shutil.which("tesseract")
        if detected_tesseract is None:
            raise MRZComparisonError("tesseract_executable_unavailable")
        tesseract_path = Path(detected_tesseract).resolve()
    base.private_dir(work_root)
    base.private_dir(output_dir)

    go_version, python_versions = base.check_toolchain(python, go, model_dir)
    gold = json.loads(gold_path.read_text(encoding="utf-8"))
    gold_images = gold.get("images") if isinstance(gold, dict) else None
    if not isinstance(gold_images, dict):
        raise MRZComparisonError("ground_truth_schema_invalid")
    matched = [
        (f"sample-{index:02d}", image, base.image_key(image.stem, gold_images))
        for index, image in enumerate(images, start=1)
    ]
    if len({record[2] for record in matched}) != 2:
        raise MRZComparisonError("images_map_to_duplicate_ground_truth_records")
    base.private_json(work_root / "private-input-manifest.json", {
        "schema_version": "ocr-mrz-private-input-manifest/v1",
        "images": [
            {"sample_id": sample_id, "source_path": str(path), "sha256": base.sha256_file(path), "ground_truth_key": gold_key}
            for sample_id, path, gold_key in matched
        ],
    })

    env = os.environ.copy()
    env.update({"GOCACHE": str(work_root / "go-cache"), "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})
    base.private_dir(work_root / "go-cache")
    base.run_go(go, ["test", "./..."], env=env, timeout=300, phase="go_test")
    evaluator = work_root / "ocr-eval"
    base.run_go(go, ["build", "-o", str(evaluator), "./cmd/ocr-eval"], env=env, timeout=300, phase="go_build")
    if not evaluator.is_file():
        raise MRZComparisonError("go_evaluator_binary_not_created")
    evaluator.chmod(0o700)

    if args.reuse_observations:
        observation_paths = load_v2_observations(work_root, matched)
        observations_reused = True
        ocr_seconds: dict[str, float | None] = {sample_id: None for sample_id, _, _ in matched}
        first = json.loads(next(iter(observation_paths.values())).read_text(encoding="utf-8"))
        providers = first.get("runtime", {}).get("providers")
    else:
        build_observations, create_cpu_engine = base.load_observation_builder()
        det_model = model_dir / base.MODEL_HASHES["det"][0]
        rec_model = model_dir / base.MODEL_HASHES["rec"][0]
        engine = create_cpu_engine(det_model, rec_model)
        observation_paths = {}
        ocr_seconds = {}
        providers = None
        for sample_id, image_path, _ in matched:
            started = time.perf_counter()
            observation = build_observations(
                image_path=image_path,
                image_id=sample_id,
                det_model=det_model,
                rec_model=rec_model,
                engine=engine,
                enhanced=True,
                include_mrz=True,
                tesseract_path=tesseract_path,
            )
            ocr_seconds[sample_id] = time.perf_counter() - started
            if providers is None:
                providers = base.assert_cpu_engine(engine)
            image = observation.get("image", {})
            expected_models = {"detector": base.MODEL_HASHES["det"][1], "recognizer": base.MODEL_HASHES["rec"][1]}
            if (
                observation.get("schema_version") != SCHEMA_V2
                or image.get("id") != sample_id
                or image.get("sha256") != base.sha256_file(image_path)
                or image.get("coordinate_frame") != "exif_normalized_pixels"
                or observation.get("runtime", {}).get("model_sha256") != expected_models
                or not any(item.get("kind") == "mrz" for item in observation.get("passes", []))
            ):
                raise MRZComparisonError("observation_schema_or_input_mismatch")
            path = work_root / sample_id / "observations.json"
            base.private_json(path, observation)
            observation_paths[sample_id] = path
        observations_reused = False

    run_results: list[dict[str, Any]] = []
    for mode, policy in MODES:
        for sample_id, _, gold_key in matched:
            observation_path = observation_paths[sample_id]
            extraction_path = work_root / sample_id / f"{mode}.json"
            started = time.perf_counter()
            command = base.evaluator_command(evaluator, observation_path, extraction_path, "all", True)
            command.extend([f"--mrz-policy={policy}"])
            base.run_executable(Path(command[0]), command[1:], env=env, timeout=60, phase="go_extract")
            extraction_seconds = time.perf_counter() - started
            extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
            if extraction.get("schema_version") != "passport-extraction/v1" or extraction.get("image_id") != sample_id:
                raise MRZComparisonError("go_extraction_schema_or_image_id_mismatch")
            scored = base.score_extraction(extraction, gold_images[gold_key].get("fields", {}))
            run_results.append({
                "mode": mode,
                "image_id": sample_id,
                "pass_set": "all",
                "normalize_names": True,
                "mrz_policy": policy,
                "ocr_seconds": round(ocr_seconds[sample_id], 6) if ocr_seconds[sample_id] is not None else None,
                "extraction_seconds": round(extraction_seconds, 6),
                **scored,
                "mrz": public_mrz_summary(extraction),
            })

    report = {
        "schema_version": "passport-ocr-mrz-comparison/v1",
        "status": "PASS",
        "scope": "two development images; not a held-out accuracy estimate",
        "comparison_boundary": TIMING_BOUNDARY,
        "observations_reused": observations_reused,
        "ocr_timing": "historical_not_measured_this_run" if observations_reused else "measured_this_run",
        "toolchain": {"go": go_version, "python_packages": python_versions, "providers": providers},
        "model_sha256": {role: values[1] for role, values in base.MODEL_HASHES.items()},
        "results": run_results,
    }
    base.private_json(work_root / "private-mrz-comparison-details.json", report)
    public_report = {key: report[key] for key in ("schema_version", "status", "scope", "comparison_boundary", "observations_reused", "ocr_timing", "toolchain", "results")}
    public_report["model_sha256"] = "pinned local models verified"
    base.private_json(output_dir / "ocr-mrz-comparison.json", public_report)
    rows = [
        "# MRZ comparison", "",
        "Two local development images; results are not a held-out accuracy estimate.",
        TIMING_BOUNDARY,
        "Raw OCR, MRZ lines, decoded values and gold literals are stored only in the private work directory.", "",
        "| Variant | Image | Suggested exact required / 10 | Wrong suggestions | False fills | Missing or abstained | MRZ statuses | MRZ issue codes | OCR s | Go s |",
        "|---|---|---:|---:|---:|---:|---|---|---:|---:|",
    ]
    for result in run_results:
        totals = result["totals"]
        mrz = result["mrz"]
        counts = mrz["status_counts"]
        statuses = f"valid {counts['valid']}, invalid {counts['invalid']}, indeterminate {counts['indeterminate']}"
        issue_codes = ", ".join(mrz["issues"]) or "—"
        ocr_time = f"{result['ocr_seconds']:.3f}" if result["ocr_seconds"] is not None else "reused"
        rows.append(
            f"| {result['mode']} | {result['image_id']} | {totals['required_suggested_correct']}/10 "
            f"| {totals['required_wrong_suggestion']} | {totals['required_false_fill']} | {totals['required_missing_or_abstained']} "
            f"| {statuses} | {issue_codes} | {ocr_time} | {result['extraction_seconds']:.3f} |"
        )
    rows.extend(["", "Public rows contain counts, match indicators, timings and parser/check issue codes only.", "The valid status describes format/check-digit consistency and does not establish document authenticity or field truth.", "All fields remain subject to human review.", ""])
    base.write_public(output_dir / "ocr-mrz-comparison.md", "\n".join(rows))
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, type=Path, help="Existing project Python interpreter")
    parser.add_argument("--go", type=Path, default=Path("/opt/homebrew/bin/go"), help="Existing Go executable")
    parser.add_argument("--work-root", type=Path, default=DEFAULT_WORK, help="Private scratch directory")
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT, help="Aggregate-only report directory")
    parser.add_argument("--image", action="append", required=True, type=Path, help="Exactly two explicit local images")
    parser.add_argument("--gold", required=True, type=Path, help="Private ground-truth JSON")
    parser.add_argument("--model-dir", required=True, type=Path, help="Already-cached pinned OCR model directory")
    parser.add_argument("--tesseract", type=Path, help="Existing Tesseract executable; otherwise use PATH lookup")
    parser.add_argument("--reuse-observations", action="store_true", help="Reuse matching private v2 observations")
    args = parser.parse_args(argv)
    try:
        _run(args)
    except (MRZComparisonError, base.ComparisonError, OSError, ValueError, TypeError, KeyError, AttributeError, json.JSONDecodeError) as error:
        print(f"MRZ comparison failed: {base.safe_error_text(str(error))}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
