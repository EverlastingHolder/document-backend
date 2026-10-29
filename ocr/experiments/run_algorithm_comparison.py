#!/usr/bin/env python3
"""Run a bounded private A/B comparison for OCR observations and Go extraction.

The script reuses the already-installed Python environment and model files. It
never installs packages, downloads weights, sends images elsewhere, or prints
OCR/gold values. Private raw observations and extraction JSON stay under the
0700 work directory; the output directory receives aggregate-only reports.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import unicodedata
from datetime import datetime
from importlib.metadata import version
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
EXPECTED_PACKAGES = {"rapidocr": "3.10.0", "onnxruntime": "1.22.0", "Pillow": "11.3.0"}
MODEL_HASHES = {
    "det": ("ch_PP-OCRv5_det_mobile.onnx", "4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae"),
    "rec": ("eslav_PP-OCRv5_rec_mobile.onnx", "08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab"),
}
FIELDS = (
    "surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
    "series", "number", "issue_date", "issuer", "department_code",
)
REQUIRED_FIELDS = tuple(field for field in FIELDS if field != "patronymic")
MODES = (
    ("A_original_raw", "original", False),
    ("B_original_name_proposal", "original", True),
    ("C_all_passes_name_proposal", "all", True),
)


class ComparisonError(RuntimeError):
    """A sanitized, user-safe comparison failure."""


def private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def private_json(path: Path, payload: Any) -> None:
    private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def write_public(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def safe_error_text(value: str) -> str:
    """Redact paths, quoted text, long digit runs and Cyrillic OCR fragments."""
    value = re.sub(r"(['\"]).*?\1", "<redacted>", value)
    value = re.sub(r"(?<!\w)\d{4,}(?!\w)", "<redacted-number>", value)
    value = re.sub(r"[А-Яа-яЁё]{2,}", "<redacted-text>", value)
    value = re.sub(r"(?<!\w)/(?:[^\s:,]+/?)+", "<path>", value)
    value = re.sub(r"[\r\n\t]+", " ", value)
    return value[:500]


def write_failure(output_dir: Path, phase: str, reason: str, stderr: str = "") -> None:
    public = {
        "schema_version": "passport-ocr-algorithm-comparison/v1",
        "status": "FAIL",
        "phase": phase,
        "reason": reason,
        "diagnostic": safe_error_text(stderr) if stderr else None,
    }
    private_dir(output_dir)
    write_public(output_dir / "ocr-algorithm-comparison.json", json.dumps(public, indent=2) + "\n")
    write_public(
        output_dir / "ocr-algorithm-comparison.md",
        "# OCR algorithm comparison\n\n"
        f"- Status: **FAIL**\n- Phase: `{phase}`\n- Reason: {reason}\n"
        + (f"- Sanitized diagnostic: `{public['diagnostic']}`\n" if public["diagnostic"] else ""),
    )


def check_toolchain(python: Path, go: Path, model_dir: Path) -> tuple[str, dict[str, str]]:
    if Path(sys.executable).resolve() != python:
        raise ComparisonError("runner_interpreter_does_not_match_requested_python")
    if not python.is_file() or not go.is_file():
        raise ComparisonError("requested_python_or_go_executable_missing")
    if not model_dir.is_dir():
        raise ComparisonError("cached_model_directory_missing")
    versions: dict[str, str] = {}
    for package, expected in EXPECTED_PACKAGES.items():
        try:
            actual = version(package)
        except Exception as error:
            raise ComparisonError(f"python_package_unavailable_{package}") from error
        if actual != expected:
            raise ComparisonError(f"python_package_version_mismatch_{package}")
        versions[package] = actual
    for role, (filename, expected_hash) in MODEL_HASHES.items():
        path = model_dir / filename
        if not path.is_file() or sha256_file(path) != expected_hash:
            raise ComparisonError(f"cached_model_hash_mismatch_{role}")
    try:
        result = subprocess.run([str(go), "version"], cwd=ROOT, text=True, capture_output=True, timeout=10, check=True)
    except (OSError, subprocess.SubprocessError) as error:
        raise ComparisonError("go_version_preflight_failed") from error
    if "go1.27.1" not in result.stdout:
        raise ComparisonError("unexpected_go_toolchain_version")
    return result.stdout.strip(), versions


def run_go(go: Path, args: list[str], *, env: dict[str, str], timeout: int, phase: str) -> str:
    return run_executable(go, args, env=env, timeout=timeout, phase=phase)


def run_executable(executable: Path, args: list[str], *, env: dict[str, str], timeout: int, phase: str) -> str:
    try:
        completed = subprocess.run(
            [str(executable), *args], cwd=ROOT, env=env, text=True, capture_output=True,
            timeout=timeout, check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise ComparisonError(f"{phase}_timed_out") from error
    except OSError as error:
        raise ComparisonError(f"{phase}_could_not_start") from error
    if completed.returncode:
        raise ComparisonError(f"{phase}_failed_exit_{completed.returncode}\n{safe_error_text(completed.stderr[-3000:])}")
    return completed.stdout


def evaluator_command(
    evaluator: Path,
    observation_path: Path,
    extraction_path: Path,
    pass_set: str,
    normalize_names: bool,
) -> list[str]:
    """Build argv for direct execution of the compiled evaluator binary."""
    return [
        str(evaluator), "--input", str(observation_path), "--output", str(extraction_path),
        f"--passes={pass_set}", f"--normalize-names={'true' if normalize_names else 'false'}",
    ]


def load_reusable_observations(
    work_root: Path,
    matched: list[tuple[str, Path, str]],
    gold_images: dict[str, Any],
) -> dict[str, tuple[Path, dict[str, Any], float | None]]:
    """Load only observations bound to the exact inputs and pinned OCR models."""
    observations: dict[str, tuple[Path, dict[str, Any], float | None]] = {}
    expected_models = {
        "detector": MODEL_HASHES["det"][1],
        "recognizer": MODEL_HASHES["rec"][1],
    }
    for image_id, image_path, gold_key in matched:
        observation_path = work_root / image_id / "observations.json"
        try:
            observation = json.loads(observation_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise ComparisonError(f"reusable_observations_missing_or_invalid_{image_id}") from error
        if not isinstance(observation, dict):
            raise ComparisonError(f"reusable_observations_mismatch_{image_id}")
        image = observation.get("image")
        runtime = observation.get("runtime") if isinstance(observation, dict) else None
        if (
            observation.get("schema_version") != "passport-ocr-observations/v1"
            or not isinstance(image, dict)
            or image.get("id") != image_id
            or image.get("sha256") != sha256_file(image_path)
            or image.get("coordinate_frame") != "exif_normalized_pixels"
            or not isinstance(runtime, dict)
            or runtime.get("model_sha256") != expected_models
        ):
            raise ComparisonError(f"reusable_observations_mismatch_{image_id}")
        observations[image_id] = (observation_path, gold_images[gold_key], None)
    return observations


def image_key(image_id: str, images: dict[str, Any]) -> str:
    normalized = re.sub(r"[^a-z0-9]", "", image_id.casefold())
    matches = []
    for key, entry in images.items():
        candidates = (key, str(entry.get("source_image_id", "")))
        if any(re.sub(r"[^a-z0-9]", "", candidate.casefold()) == normalized for candidate in candidates):
            matches.append(key)
    if len(matches) != 1:
        raise ComparisonError("ground_truth_image_mapping_not_unique")
    return matches[0]


def canonical(value: str, field: str) -> str:
    """Conservative comparison only; never repair OCR glyphs or fuzzy-match."""
    value = unicodedata.normalize("NFC", value).strip()
    value = " ".join(value.split()).casefold()
    if field == "gender":
        # Compare only known canonical and printed passport forms. Keep every
        # other token literal so ambiguous OCR is not guessed into M or F.
        return {
            "m": "m", "m.": "m", "муж": "m", "муж.": "m",
            "f": "f", "f.": "f", "жен": "f", "жен.": "f",
        }.get(value, value)
    if field in {"series", "number", "department_code"}:
        digits = "".join(character for character in value if character.isdigit())
        allowed = set("0123456789 -")
        if digits and all(character in allowed for character in value):
            return digits
    if field in {"birth_date", "issue_date"}:
        for pattern in ("%Y-%m-%d", "%d.%m.%Y", "%d/%m/%Y", "%d-%m-%Y"):
            try:
                return datetime.strptime(value, pattern).date().isoformat()
            except ValueError:
                pass
    return value


def field_result(extraction: dict[str, Any], name: str) -> dict[str, Any]:
    fields = extraction.get("fields")
    if not isinstance(fields, dict) or not isinstance(fields.get(name), dict):
        raise ComparisonError("go_extraction_missing_expected_field")
    return fields[name]


def score_extraction(extraction: dict[str, Any], gold_fields: dict[str, Any]) -> dict[str, Any]:
    field_rows: dict[str, Any] = {}
    totals = {
        "expected_present": 0, "recognized_correct": 0, "suggested_correct": 0,
        "required_expected_present": 0, "optional_expected_present": 0,
        "required_recognized_correct": 0, "required_suggested_correct": 0,
        "wrong_suggestion": 0, "false_recognized": 0, "missing_or_abstained": 0,
        "required_wrong_suggestion": 0, "required_false_recognized": 0,
        "required_missing_or_abstained": 0,
        "expected_absent": 0, "false_fill": 0,
        "required_false_fill": 0,
    }
    required_recognized_exact = True
    required_suggested_exact = True
    rules_applied = 0
    for name in FIELDS:
        expected_obj = gold_fields.get(name)
        if not isinstance(expected_obj, dict) or not isinstance(expected_obj.get("literal"), str):
            raise ComparisonError("ground_truth_field_shape_invalid")
        expected = expected_obj["literal"]
        result = field_result(extraction, name)
        value = result.get("value")
        if value is not None and not isinstance(value, str):
            raise ComparisonError("go_extraction_value_shape_invalid")
        state = result.get("state")
        if state not in {"recognized", "uncertain", "missing"}:
            raise ComparisonError("go_extraction_state_invalid")
        candidate = value or ""
        is_expected = bool(expected.strip())
        is_present = bool(candidate.strip())
        same = is_expected and is_present and canonical(candidate, name) == canonical(expected, name)
        if isinstance(result.get("rules"), list):
            rules_applied += len(result["rules"])
        if is_expected:
            totals["expected_present"] += 1
            if name in REQUIRED_FIELDS:
                totals["required_expected_present"] += 1
            else:
                totals["optional_expected_present"] += 1
            if same:
                totals["suggested_correct"] += 1
                if name in REQUIRED_FIELDS:
                    totals["required_suggested_correct"] += 1
                if state == "recognized":
                    totals["recognized_correct"] += 1
                    if name in REQUIRED_FIELDS:
                        totals["required_recognized_correct"] += 1
            elif not is_present or state == "missing":
                totals["missing_or_abstained"] += 1
                if name in REQUIRED_FIELDS:
                    totals["required_missing_or_abstained"] += 1
            else:
                totals["wrong_suggestion"] += 1
                if name in REQUIRED_FIELDS:
                    totals["required_wrong_suggestion"] += 1
                if state == "recognized":
                    totals["false_recognized"] += 1
                    if name in REQUIRED_FIELDS:
                        totals["required_false_recognized"] += 1
            if name in REQUIRED_FIELDS:
                required_recognized_exact &= same and state == "recognized"
                required_suggested_exact &= same
        else:
            totals["expected_absent"] += 1
            if is_present:
                totals["false_fill"] += 1
                if name in REQUIRED_FIELDS:
                    totals["required_false_fill"] += 1
        field_rows[name] = {
            "state": state,
            "expected_present": is_expected,
            "value_present": is_present,
            "suggested_correct": bool(same),
            "recognized_correct": bool(same and state == "recognized"),
            "rules_applied": len(result.get("rules", [])) if isinstance(result.get("rules"), list) else 0,
        }
    return {
        "fields": field_rows,
        "totals": totals,
        "required_exact_document_recognized": required_recognized_exact,
        "required_exact_document_suggested": required_suggested_exact,
        "rules_applied": rules_applied,
    }


def patronymic_status(field: dict[str, Any]) -> str:
    """Render the optional field status from selected-value presence first."""
    if not field["expected_present"]:
        return "expected-absent/false-fill" if field["value_present"] else "expected-absent/no-fill"
    if not field["value_present"]:
        return "missing"
    if field["recognized_correct"]:
        return "exact-recognized"
    if field["suggested_correct"]:
        return "exact-uncertain"
    return f"{field['state']}-wrong"


def load_observation_builder():
    try:
        from ocr.experiments.region_pipeline import build_observations, create_cpu_engine
    except Exception as error:
        raise ComparisonError("region_pipeline_unavailable") from error
    return build_observations, create_cpu_engine


def assert_cpu_engine(engine: Any) -> dict[str, list[str]]:
    providers: dict[str, list[str]] = {}
    for role, attribute in (("detector", "text_det"), ("recognizer", "text_rec")):
        stage = getattr(engine, attribute, None)
        session = getattr(getattr(stage, "session", None), "session", None)
        getter = getattr(session, "get_providers", None)
        active = getter() if callable(getter) else None
        if not active or active != ["CPUExecutionProvider"]:
            raise ComparisonError(f"rapidocr_{role}_not_cpu_only")
        providers[role] = list(active)
    return providers


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, type=Path, help="Exact existing Python interpreter for this run")
    parser.add_argument("--go", required=True, type=Path, help="Exact Go executable")
    parser.add_argument("--work-root", required=True, type=Path, help="Private workspace for raw OCR and field outputs")
    parser.add_argument("--output-dir", required=True, type=Path, help="Aggregate-only report destination")
    parser.add_argument("--image", action="append", required=True, type=Path, help="One of exactly two explicit local image paths")
    parser.add_argument("--gold", required=True, type=Path, help="Private ground-truth JSON; never copied or printed")
    parser.add_argument("--model-dir", required=True, type=Path, help="Directory containing the already-cached pinned ONNX models")
    parser.add_argument(
        "--reuse-observations", action="store_true",
        help="Reuse matching private observations from --work-root; fail if either cache is absent or mismatched",
    )
    args = parser.parse_args()

    output_dir = args.output_dir.expanduser().resolve()
    phase = "arguments"
    try:
        python = args.python.expanduser().resolve()
        go = args.go.expanduser().resolve()
        work_root = args.work_root.expanduser().resolve()
        gold_path = args.gold.expanduser().resolve()
        model_dir = args.model_dir.expanduser().resolve()
        images = [path.expanduser().resolve() for path in args.image]
        if len(images) != 2 or len(set(images)) != 2:
            raise ComparisonError("exactly_two_distinct_images_required")
        if any(not path.is_file() for path in images) or not gold_path.is_file():
            raise ComparisonError("explicit_input_file_missing")
        private_dir(work_root)
        private_dir(output_dir)

        phase = "preflight"
        go_version, python_versions = check_toolchain(python, go, model_dir)
        gold = json.loads(gold_path.read_text(encoding="utf-8"))
        gold_images = gold.get("images") if isinstance(gold, dict) else None
        if not isinstance(gold_images, dict):
            raise ComparisonError("ground_truth_schema_invalid")
        # Public identifiers are assigned by explicit argument order, never
        # derived from the source filename. Keep source path/gold key mapping
        # only in the private work directory.
        matched = [
            (f"sample-{index:02d}", path, image_key(path.stem, gold_images))
            for index, path in enumerate(images, start=1)
        ]
        if len({item[2] for item in matched}) != 2:
            raise ComparisonError("images_map_to_duplicate_ground_truth_records")
        observations_by_id = None
        if args.reuse_observations:
            phase = "reuse_observations"
            observations_by_id = load_reusable_observations(work_root, matched, gold_images)
            phase = "preflight"
        private_json(
            work_root / "private-input-manifest.json",
            {
                "schema_version": "ocr-private-input-manifest/v1",
                "images": [
                    {"sample_id": image_id, "source_path": str(image_path), "ground_truth_key": gold_key}
                    for image_id, image_path, gold_key in matched
                ],
            },
        )
        env = os.environ.copy()
        env.update({
            "GOCACHE": str(work_root / "go-cache"),
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
        })
        private_dir(work_root / "go-cache")
        phase = "go_test"
        run_go(go, ["test", "./..."], env=env, timeout=300, phase=phase)
        phase = "go_build"
        evaluator = work_root / "ocr-eval"
        run_go(go, ["build", "-o", str(evaluator), "./cmd/ocr-eval"], env=env, timeout=300, phase=phase)
        if not evaluator.is_file():
            raise ComparisonError("go_evaluator_binary_not_created")
        evaluator.chmod(0o700)

        providers: dict[str, list[str]] | None = None
        if args.reuse_observations:
            assert observations_by_id is not None
            first_observation = json.loads(next(iter(observations_by_id.values()))[0].read_text(encoding="utf-8"))
            providers = first_observation["runtime"].get("providers")
        else:
            build_observations, create_cpu_engine = load_observation_builder()
            phase = "ocr_observations"
            det_model = model_dir / MODEL_HASHES["det"][0]
            rec_model = model_dir / MODEL_HASHES["rec"][0]
            engine = create_cpu_engine(det_model, rec_model)
            observations_by_id = {}
            for image_id, image_path, gold_key in matched:
                started = time.perf_counter()
                observation = build_observations(
                    image_path=image_path,
                    image_id=image_id,
                    det_model=det_model,
                    rec_model=rec_model,
                    engine=engine,
                    enhanced=True,
                )
                elapsed = time.perf_counter() - started
                if providers is None:
                    # RapidOCR may instantiate ONNX sessions lazily, so inspect the
                    # actual sessions only after the first inference call.
                    providers = assert_cpu_engine(engine)
                if observation.get("schema_version") != "passport-ocr-observations/v1":
                    raise ComparisonError("observation_schema_version_mismatch")
                if observation.get("image", {}).get("coordinate_frame") != "exif_normalized_pixels":
                    raise ComparisonError("observation_coordinate_frame_mismatch")
                if observation.get("image", {}).get("id") != image_id:
                    raise ComparisonError("observation_image_id_mismatch")
                private_json(work_root / image_id / "observations.json", observation)
                observations_by_id[image_id] = (work_root / image_id / "observations.json", gold_images[gold_key], elapsed)

        phase = "field_extraction_and_scoring"
        run_results: list[dict[str, Any]] = []
        for mode_id, pass_set, normalize_names in MODES:
            for image_id, (observation_path, gold_entry, ocr_seconds) in observations_by_id.items():
                extraction_path = work_root / image_id / f"{mode_id}.json"
                started = time.perf_counter()
                command = evaluator_command(evaluator, observation_path, extraction_path, pass_set, normalize_names)
                run_executable(
                    Path(command[0]), command[1:],
                    env=env, timeout=60, phase="go_extract",
                )
                extraction_seconds = time.perf_counter() - started
                extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
                if extraction.get("schema_version") != "passport-extraction/v1" or extraction.get("image_id") != image_id:
                    raise ComparisonError("go_extraction_schema_or_image_id_mismatch")
                scored = score_extraction(extraction, gold_entry.get("fields", {}))
                run_results.append({
                    "mode": mode_id,
                    "image_id": image_id,
                    "pass_set": pass_set,
                    "normalize_names": normalize_names,
                    "ocr_seconds": round(ocr_seconds, 6) if ocr_seconds is not None else None,
                    "ocr_timing": "current_inference" if ocr_seconds is not None else "reused_historical_not_measured",
                    "extraction_seconds": round(extraction_seconds, 6),
                    **scored,
                })

        phase = "report"
        report = {
            "schema_version": "passport-ocr-algorithm-comparison/v1",
            "status": "PASS",
            "scope": "two development images; not a held-out accuracy estimate",
            "observations_reused": args.reuse_observations,
            "ocr_timing": "historical_not_measured_this_run" if args.reuse_observations else "measured_this_run",
            "toolchain": {"go": go_version, "python_packages": python_versions, "providers": providers},
            "model_sha256": {role: values[1] for role, values in MODEL_HASHES.items()},
            "results": run_results,
        }
        private_json(work_root / "private-comparison-details.json", report)
        public_report = {
            key: report[key]
            for key in ("schema_version", "status", "scope", "observations_reused", "ocr_timing", "toolchain")
        }
        public_report["model_sha256"] = "pinned local models verified"
        public_report["results"] = run_results
        private_json(output_dir / "ocr-algorithm-comparison.json", public_report)
        rows = [
            "# OCR algorithm comparison", "",
            "Two local development images; these results are not a held-out accuracy estimate.",
            "Raw OCR and extraction values are stored only in the private work directory.", "",
            "| Variant | Image | Gold present (required/optional) | Required exact recognized / 10 | Required exact suggested / 10 | Patronymic | Wrong required suggestions | False recognized required | False fills required | Missing/abstained required | OCR s | Go s |",
            "|---|---|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|",
        ]
        for result in run_results:
            totals = result["totals"]
            required = len(REQUIRED_FIELDS)
            patronymic = result["fields"]["patronymic"]
            patronymic_label = patronymic_status(patronymic)
            ocr_duration = (
                f"{result['ocr_seconds']:.3f}"
                if result["ocr_seconds"] is not None
                else "historical (not measured)"
            )
            rows.append(
                f"| {result['mode']} | {result['image_id']} | {totals['required_expected_present']}/{required} + {totals['optional_expected_present']}/1 "
                f"| {totals['required_recognized_correct']}/{required} "
                f"| {totals['required_suggested_correct']}/{required} | {patronymic_label} "
                f"| {totals['required_wrong_suggestion']} | {totals['required_false_recognized']} "
                f"| {totals['required_false_fill']} | {totals['required_missing_or_abstained']} "
                f"| {ocr_duration} | {result['extraction_seconds']:.3f} |"
            )
        rows.extend([
            "", "Per-field rows contain only states and match indicators; no recognized or gold text is included.",
            "The gold literal values were compared without character-confusable substitutions or fuzzy matching.",
            "A/B/C deltas apply only to these two development images and do not establish production quality.", "",
        ])
        write_public(output_dir / "ocr-algorithm-comparison.md", "\n".join(rows))
        print("OCR algorithm comparison PASS; aggregate reports written.")
        return 0
    except (ComparisonError, OSError, ValueError, json.JSONDecodeError, subprocess.SubprocessError) as error:
        error_lines = str(error).splitlines()
        if isinstance(error, ComparisonError):
            message = error_lines[0]
            diagnostic = " ".join(error_lines[1:])
        else:
            # Filesystem/decoder exceptions can embed the private input path or
            # a fragment of malformed input. Expose only the exception class.
            message = f"{type(error).__name__}_failure"
            diagnostic = ""
        write_failure(output_dir, phase, message, diagnostic)
        print(f"OCR algorithm comparison FAIL during {phase}: {message}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
