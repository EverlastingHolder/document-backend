#!/usr/bin/env python3
"""Run a bounded, local, review-only OCR comparison on field hypotheses.

The runner consumes two explicitly supplied images and matching saved
observations/extractions. It never reruns Go, trains weights, downloads files,
or changes the baseline field result. Raw output and paths stay private.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import time
import unicodedata
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[2]

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import ARCHIVE_OUTPUT_ROOT, ARCHIVE_WORK_ROOT, RAPIDOCR_MODEL_DIR  # noqa: E402
from ocr.experiments import field_roi_pipeline as roi


DEFAULT_BASELINE = ARCHIVE_WORK_ROOT / "ocr-resolution-work"
DEFAULT_WORK = ARCHIVE_WORK_ROOT / "ocr-field-roi-work"
DEFAULT_OUTPUT = ARCHIVE_OUTPUT_ROOT / "ocr-field-roi-comparison"
DEFAULT_MODEL_DIR = RAPIDOCR_MODEL_DIR
DEFAULT_TESSDATA_DIR = DEFAULT_MODEL_DIR / "tessdata_fast-4.1.0"
DEFAULT_TESSERACT = Path("/opt/homebrew/bin/tesseract")
REPORT_SCHEMA = "passport-field-roi-comparison/v1"
NORMALIZATION_NOTE = "Literal NFC/case/whitespace comparison only; Go given_name visual-Latin-to-Cyrillic proposal is not applied."
BASELINE_REFERENCE = {"correct": 15, "wrong": 3, "abstain": 2, "required_fields": 20}


class ComparisonError(RuntimeError):
    """A safe error containing a code, not user or OCR data."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
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


def _public_json(path: Path, payload: Any) -> None:
    _private_json(path, payload)


def _read_json(path: Path, code: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ComparisonError(code) from error
    if not isinstance(value, dict):
        raise ComparisonError(code)
    return value


def _map_inputs_to_baseline(
    images: list[Path], baseline_root: Path, profile: str,
) -> list[dict[str, Any]]:
    if profile != "native_config":
        raise ComparisonError("only_native_736_baseline_is_supported")
    records: dict[str, dict[str, Any]] = {}
    for sample_id in ("sample-01", "sample-02"):
        sample_root = baseline_root / profile / sample_id
        obs_path = sample_root / "observations.json"
        extraction_path = sample_root / "extraction.json"
        observations = _read_json(obs_path, "baseline_observations_missing_or_invalid")
        extraction = _read_json(extraction_path, "baseline_extraction_missing_or_invalid")
        image = observations.get("image")
        if (
            observations.get("schema_version") != "passport-ocr-observations/v2"
            or not isinstance(image, dict)
            or image.get("id") != sample_id
            or image.get("coordinate_frame") != roi.COORDINATE_FRAME
            or extraction.get("schema_version") != "passport-extraction/v1"
            or extraction.get("image_id") != sample_id
        ):
            raise ComparisonError("baseline_schema_or_identity_mismatch")
        runtime = observations.get("runtime")
        models = runtime.get("model_sha256") if isinstance(runtime, dict) else None
        if not isinstance(models, dict) or models.get("detector") != roi.DETECTOR_SHA256 or models.get("recognizer") != roi.RECOGNIZER_SHA256:
            raise ComparisonError("baseline_model_pin_mismatch")
        profiles = runtime.get("detector_profiles") if isinstance(runtime, dict) else None
        primary = profiles.get("primary") if isinstance(profiles, dict) else None
        if not isinstance(primary, dict) or primary.get("limit_side_len") != 736 or primary.get("limit_type") != "min":
            raise ComparisonError("baseline_not_native_736_min")
        records[sample_id] = {"sample_id": sample_id, "observations": observations, "extraction": extraction, "obs_path": obs_path, "extraction_path": extraction_path}
    matched: list[dict[str, Any]] = []
    for path in images:
        digest = _sha256(path)
        matches = [record for record in records.values() if record["observations"]["image"].get("sha256") == digest]
        if len(matches) != 1:
            raise ComparisonError("image_to_baseline_mapping_not_unique")
        record = matches[0]
        if record["sample_id"] in {item["sample_id"] for item in matched}:
            raise ComparisonError("duplicate_baseline_sample_mapping")
        image = record["observations"]["image"]
        if not isinstance(image.get("width"), int) or not isinstance(image.get("height"), int):
            raise ComparisonError("baseline_image_dimensions_invalid")
        matched.append({**record, "image_path": path, "image_sha256": digest})
    if len(matched) != 2 or {item["sample_id"] for item in matched} != {"sample-01", "sample-02"}:
        raise ComparisonError("exactly_two_distinct_baseline_images_required")
    return sorted(matched, key=lambda item: item["sample_id"])


def _validate_pins(model_dir: Path, tessdata_dir: Path) -> dict[str, str]:
    detector = model_dir / roi.MODEL_FILENAMES["detector"]
    recognizer = model_dir / roi.MODEL_FILENAMES["recognizer"]
    tessdata = tessdata_dir / roi.TESSDATA_FILENAME
    pins = ((detector, roi.DETECTOR_SHA256), (recognizer, roi.RECOGNIZER_SHA256), (tessdata, roi.TESSDATA_SHA256))
    hashes = {}
    for path, expected in pins:
        if not path.is_file() or _sha256(path) != expected:
            raise ComparisonError("local_model_pin_mismatch")
        hashes[path.name] = expected
    return hashes


def _private_baseline_copy(record: dict[str, Any], destination: Path) -> None:
    _private_json(destination / "observations.json", record["observations"])
    _private_json(destination / "extraction.json", record["extraction"])


def normalize_candidate(value: str) -> str:
    return " ".join(unicodedata.normalize("NFC", value).strip().split()).casefold()


def _tokens(value: str) -> list[str]:
    return re.findall(r"[^\W_]+", unicodedata.normalize("NFC", value).casefold(), flags=re.UNICODE)


def _whole_token_sequence_present(expected: str, candidate: str) -> bool:
    expected_tokens = _tokens(expected)
    candidate_tokens = _tokens(candidate)
    if not expected_tokens or len(expected_tokens) > len(candidate_tokens):
        return False
    n = len(expected_tokens)
    return any(candidate_tokens[index:index + n] == expected_tokens for index in range(len(candidate_tokens) - n + 1))


def _box_union(boxes: list[list[float]]) -> list[float] | None:
    if not boxes:
        return None
    return [min(box[0] for box in boxes), min(box[1] for box in boxes), max(box[2] for box in boxes), max(box[3] for box in boxes)]


def _boxes_adjacent(previous: list[float], current: list[float]) -> bool:
    ph, ch = previous[3] - previous[1], current[3] - current[1]
    if ph <= 0 or ch <= 0:
        return False
    vertical_gap = current[1] - previous[3]
    max_height = max(ph, ch)
    if vertical_gap < -max_height or vertical_gap > max(8.0, 2.5 * max_height):
        return False
    x_overlap = min(previous[2], current[2]) - max(previous[0], current[0])
    left_tolerance = max(2.0 * max_height, 0.10 * max(previous[2] - previous[0], current[2] - current[0]))
    return x_overlap > 0 or abs(previous[0] - current[0]) <= left_tolerance


def _horizontal_gap(previous: list[float], current: list[float]) -> float:
    return max(0.0, current[0] - previous[2], previous[0] - current[2])


def _horizontally_connected(previous: list[float], current: list[float]) -> bool:
    previous_height = max(0.0, previous[3] - previous[1])
    current_height = max(0.0, current[3] - current[1])
    return _horizontal_gap(previous, current) <= max(8.0, 2.0 * max(previous_height, current_height))


def _tesseract_rows(words: list[dict[str, Any]]) -> list[dict[str, Any]]:
    grouped: dict[tuple[int, int, int, int], list[dict[str, Any]]] = {}
    for word in words:
        ids = tuple(word.get(key) for key in ("page_num", "block_num", "par_num", "line_num"))
        if not all(isinstance(value, int) for value in ids) or not isinstance(word.get("box_source"), list):
            continue
        grouped.setdefault(ids, []).append(word)
    rows: list[dict[str, Any]] = []
    for (page_num, block_num, par_num, line_num), line_words in sorted(grouped.items()):
        ordered = sorted(line_words, key=lambda word: word.get("word_num", 0))
        run: list[dict[str, Any]] = []
        for word in ordered:
            disconnected = bool(run) and not _horizontally_connected(run[-1]["box_source"], word["box_source"])
            if run and (word.get("word_num") != run[-1].get("word_num", 0) + 1 or disconnected):
                rows.append(_make_tesseract_row(run, page_num, block_num, par_num, line_num))
                run = []
            run.append(word)
        if run:
            rows.append(_make_tesseract_row(run, page_num, block_num, par_num, line_num))
    return rows


def _make_tesseract_row(words: list[dict[str, Any]], page: int, block: int, par: int, line: int) -> dict[str, Any]:
    ordered = sorted(words, key=lambda word: word.get("word_num", 0))
    boxes = [word["box_source"] for word in ordered]
    return {
        "candidate_text": " ".join(str(word.get("text_raw", "")) for word in ordered).strip(),
        "box_source": _box_union(boxes),
        "geometry_kind": "tesseract_tsv_line",
        "group_provenance": {
            "reader": "tesseract_rus", "page_num": page, "block_num": block,
            "par_num": par, "line_num": line,
            "word_nums": [word.get("word_num") for word in ordered],
        },
    }


def _rapid_rows(outputs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    rows: list[list[dict[str, Any]]] = []
    output_indices = {id(item): index for index, item in enumerate(outputs)}
    ordered = sorted(
        [item for item in outputs if isinstance(item, dict) and isinstance(item.get("text_raw"), str)],
        key=lambda item: ((item.get("box_source") or [0.0, 0.0, 0.0, 0.0])[1], (item.get("box_source") or [0.0, 0.0, 0.0, 0.0])[0]),
    )
    for output in ordered:
        box = output.get("box_source")
        if not isinstance(box, list) or len(box) != 4 or output.get("geometry_kind") == "entire_crop":
            rows.append([output])
            continue
        target = None
        for row in rows:
            row_boxes = [item.get("box_source") for item in row if isinstance(item.get("box_source"), list)]
            row_box = _box_union(row_boxes)
            if row_box is None:
                continue
            row_center = (row_box[1] + row_box[3]) / 2
            box_center = (box[1] + box[3]) / 2
            row_height = row_box[3] - row_box[1]
            box_height = box[3] - box[1]
            overlap = min(row_box[3], box[3]) - max(row_box[1], box[1])
            if (
                overlap >= 0.5 * min(row_height, box_height)
                and abs(row_center - box_center) <= max(row_height, box_height)
                and _horizontally_connected(row_box, box)
            ):
                target = row
                break
        if target is None:
            rows.append([output])
        else:
            target.append(output)
    normalized_rows = []
    for row_index, row in enumerate(rows, start=1):
        row = sorted(row, key=lambda item: (item.get("box_source") or [0.0, 0.0])[0])
        boxes = [item["box_source"] for item in row if isinstance(item.get("box_source"), list)]
        normalized_rows.append({
            "candidate_text": " ".join(item["text_raw"] for item in row).strip(),
            "box_source": _box_union(boxes),
            "geometry_kind": "rapid_detector_row" if boxes else "rapid_entire_crop_line",
            "group_provenance": {
                "reader": "rapidocr", "grouping": "same_row_vertical_overlap_and_horizontal_connectedness",
                "row_index": row_index, "output_indices": [output_indices[id(item)] for item in row if id(item) in output_indices],
            },
        })
    return normalized_rows


def build_candidate_groups(reader_result: dict[str, Any], field_id: str) -> list[dict[str, Any]]:
    """Build only physical line or connected block candidates from reader provenance."""
    cached = reader_result.get("candidate_groups")
    if isinstance(cached, list):
        return cached
    if reader_result.get("reader") == "tesseract_rus" or isinstance(reader_result.get("words"), list):
        rows = _tesseract_rows(reader_result.get("words", []))
        block_field = field_id in {"birth_place", "issuer"}
        if not block_field:
            return rows
        groups: list[list[dict[str, Any]]] = []
        for row in rows:
            provenance = row["group_provenance"]
            if not groups:
                groups.append([row])
                continue
            prior = groups[-1][-1]
            previous = prior["group_provenance"]
            same_block = all(previous[key] == provenance[key] for key in ("page_num", "block_num", "par_num"))
            consecutive = provenance["line_num"] == previous["line_num"] + 1
            if same_block and consecutive and prior.get("box_source") and row.get("box_source") and _boxes_adjacent(prior["box_source"], row["box_source"]):
                groups[-1].append(row)
            else:
                groups.append([row])
        return [
            {
                "candidate_text": " ".join(row["candidate_text"] for row in group).strip(),
                "box_source": _box_union([row["box_source"] for row in group if row.get("box_source")]),
                "geometry_kind": "tesseract_connected_block" if len(group) > 1 else "tesseract_tsv_line",
                "group_provenance": {"reader": "tesseract_rus", "rows": [row["group_provenance"] for row in group], "row_count": len(group)},
            }
            for group in groups
        ]

    outputs = reader_result.get("outputs", [])
    if not isinstance(outputs, list):
        return []
    rows = _rapid_rows(outputs)
    if field_id not in {"birth_place", "issuer"}:
        return rows
    groups: list[list[dict[str, Any]]] = []
    for row in sorted(rows, key=lambda item: (item["box_source"][1] if item.get("box_source") else 0, item["box_source"][0] if item.get("box_source") else 0)):
        if not groups:
            groups.append([row])
            continue
        previous = groups[-1][-1]
        if previous.get("box_source") and row.get("box_source") and _boxes_adjacent(previous["box_source"], row["box_source"]):
            groups[-1].append(row)
        else:
            groups.append([row])
    return [
        {
            "candidate_text": " ".join(row["candidate_text"] for row in group).strip(),
            "box_source": _box_union([row["box_source"] for row in group if row.get("box_source")]),
            "geometry_kind": "rapid_connected_block" if len(group) > 1 else group[0]["geometry_kind"],
            "group_provenance": {"reader": "rapidocr", "grouping": "adjacent_aligned_rows", "rows": [row["group_provenance"] for row in group], "row_count": len(group)},
        }
        for group in groups
    ]


def score_reader_diagnostics(
    reader_result: dict[str, Any], expected_literal: str, field_id: str,
) -> dict[str, Any]:
    """Post-hoc diagnostic only; never feeds a proposal or selected field."""
    groups = build_candidate_groups(reader_result, field_id)
    expected = expected_literal.strip()
    return {
        "candidate_group_count": len(groups),
        "exact_normalized_whole_candidate_group_match": bool(expected) and any(
            normalize_candidate(group["candidate_text"]) == normalize_candidate(expected) for group in groups
        ),
        "contiguous_whole_token_sequence_in_allowed_group": bool(expected) and any(
            _whole_token_sequence_present(expected, group["candidate_text"]) for group in groups
        ),
    }


def _gold_entries_after_readers(
    gold_path: Path | None, records: list[dict[str, Any]], reader_records: list[dict[str, Any]],
) -> dict[str, dict[str, str]] | None:
    """Read optional ground truth only after all ROI and reader work is complete."""
    if gold_path is None:
        return None
    try:
        gold_root = json.loads(gold_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ComparisonError("ground_truth_unavailable_after_reader_completion") from error
    images = gold_root.get("images") if isinstance(gold_root, dict) else None
    if not isinstance(images, dict):
        raise ComparisonError("ground_truth_schema_invalid")
    from ocr.experiments.run_algorithm_comparison import image_key

    result: dict[str, dict[str, str]] = {}
    for record in records:
        try:
            key = image_key(record["image_path"].stem, images)
        except Exception as error:
            raise ComparisonError("ground_truth_image_mapping_not_unique") from error
        fields = images[key].get("fields") if isinstance(images[key], dict) else None
        if not isinstance(fields, dict):
            raise ComparisonError("ground_truth_fields_invalid")
        values: dict[str, str] = {}
        for field_id in roi.FIELD_IDS:
            field = fields.get(field_id)
            literal = field.get("literal") if isinstance(field, dict) else None
            if not isinstance(literal, str):
                raise ComparisonError("ground_truth_field_invalid")
            values[field_id] = literal
        result[record["sample_id"]] = values
    # Scoring happens only now and is separate by reader; never vote or alter a candidate.
    by_sample: dict[str, list[dict[str, Any]]] = {}
    for item in reader_records:
        by_sample.setdefault(item["sample_id"], []).append(item)
    for sample_id, items in by_sample.items():
        for item in items:
            literal = result[sample_id][item["field_id"]]
            item["posthoc_metrics"] = score_reader_diagnostics(item, literal, item["field_id"])
    return result


def _public_summary(records: list[dict[str, Any]], reader_records: list[dict[str, Any]], has_gold: bool) -> dict[str, Any]:
    by_sample = {record["sample_id"]: record for record in records}
    reader_by_roi: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for item in reader_records:
        reader_by_roi.setdefault((item["sample_id"], item["hypothesis_id"]), []).append(item)
    samples = []
    coverage: dict[str, int] = {field_id: 0 for field_id in roi.FIELD_IDS}
    total_reader_invocations = 0
    for sample_id in sorted(by_sample):
        plan = by_sample[sample_id]["plan"]
        fields = []
        for hypothesis in plan["hypotheses"]:
            field_id = hypothesis["field_id"]
            roi_readers = reader_by_roi.get((sample_id, hypothesis["id"]), [])
            total_reader_invocations += len(roi_readers)
            reader_statuses = [{"reader": item["reader"], "status": item.get("status", "unknown"), "reason_code": item.get("reason_code")} for item in roi_readers]
            candidate_count = sum(int(item.get("candidate_count", 0)) for item in roi_readers)
            if candidate_count > 0:
                coverage[field_id] += 1
            field_row: dict[str, Any] = {
                "field_id": field_id,
                "role_quality": hypothesis["role_quality"]["state"],
                "reason_codes": hypothesis["role_quality"]["reason_codes"],
                "reader_statuses": reader_statuses,
                "candidate_count": candidate_count,
                "elapsed_seconds": round(sum(float(item.get("elapsed_seconds", 0.0)) for item in roi_readers), 4),
                "autofill": False,
            }
            if has_gold:
                field_row["posthoc_diagnostics"] = {
                    item["reader"]: item.get("posthoc_metrics", {
                        "candidate_group_count": 0,
                        "exact_normalized_whole_candidate_group_match": False,
                        "contiguous_whole_token_sequence_in_allowed_group": False,
                    })
                    for item in roi_readers
                }
            fields.append(field_row)
        samples.append({
            "sample_id": sample_id,
            "hypothesis_count": len(plan["hypotheses"]),
            "rejections": plan["rejections"],
            "fields": fields,
        })
    return {
        "schema_version": REPORT_SCHEMA,
        "status": "COMPLETED",
        "baseline_profile": "native_config_736_min",
        "baseline_required_reference_only": BASELINE_REFERENCE,
        "sample_count": len(samples),
        "roi_limit_per_document": roi.MAX_ROIS_PER_DOCUMENT,
        "reader_call_limit_per_document": roi.MAX_READER_CALLS_PER_DOCUMENT,
        "posthoc_normalization_note": NORMALIZATION_NOTE,
        "reader_invocations": total_reader_invocations,
        "candidate_coverage_by_field": coverage,
        "candidate_coverage_semantics": "field_role_with_physical_candidate_group_output_not_accuracy",
        "candidate_grouping_semantics": "rapid_rows_and_tesseract_consecutive_words_require_horizontal_gap_le_max_8px_or_2x_row_height; given_name_gender_single_physical_line; birth_place_issuer_only_consecutive_connected_rows_within_block",
        "review_only": True,
        "models_voted_or_confidence_pooled": False,
        "samples": samples,
    }


def run_comparison(
    images: list[Path],
    *,
    baseline_root: Path,
    baseline_profile: str,
    model_dir: Path,
    tessdata_dir: Path,
    tesseract: Path,
    work_root: Path,
    output_dir: Path,
    gold_path: Path | None = None,
    engine: Any | None = None,
    reader_runner: Callable[..., dict[str, Any]] | None = None,
    test_adapter: bool = False,
) -> dict[str, Any]:
    """Execute full local comparison. Tests inject FakeReaders; CLI uses pins."""
    if len(images) != 2 or len({path.resolve() for path in images}) != 2:
        raise ComparisonError("exactly_two_distinct_images_required")
    records = _map_inputs_to_baseline([path.resolve() for path in images], baseline_root.resolve(), baseline_profile)
    hashes = _validate_pins(model_dir.resolve(), tessdata_dir.resolve())
    _private_dir(work_root)
    _private_dir(output_dir)
    # Planning is complete for both images before any OCR engine is created.
    for record in records:
        record["plan"] = roi.plan_field_rois(record["observations"], record["extraction"])
        sample_dir = work_root / record["sample_id"]
        _private_dir(sample_dir)
        _private_baseline_copy(record, sample_dir / "baseline")
        _private_json(sample_dir / "field-roi-plan.json", record["plan"])
    expected_count = sum(len(record["plan"]["hypotheses"]) for record in records)
    if expected_count > 2 * roi.MAX_ROIS_PER_DOCUMENT:
        raise ComparisonError("total_roi_limit_exceeded")

    if test_adapter:
        if reader_runner is None or engine is not None:
            raise ComparisonError("test_adapter_requires_reader_without_engine")
    elif reader_runner is not None:
        raise ComparisonError("custom_reader_runner_requires_test_adapter")
    elif expected_count and engine is not None:
        allowed_states = {
            "pending_lazy_first_call", "verified_cpu_detector_and_recognizer",
            "verified_cpu_rec_only_detector_not_used", "verified_cpu_det_only_existing",
            "verified_cpu_rec_only_existing",
        }
        if roi.pinned_cpu_engine_state(engine) not in allowed_states:
            raise ComparisonError("explicit_rapidocr_engine_provider_guard_missing")

    if reader_runner is None:
        reader_runner = roi.run_hypothesis_readers
    if expected_count and engine is None and not test_adapter:
        engine = roi.create_pinned_cpu_engine(model_dir)
    reader_records: list[dict[str, Any]] = []
    total_calls = 0
    for record in records:
        sample_results: list[dict[str, Any]] = []
        sample_calls = 0
        for hypothesis in record["plan"]["hypotheses"][:roi.MAX_ROIS_PER_DOCUMENT]:
            if len(hypothesis.get("reader_plan", [])) > roi.MAX_READERS_PER_ROI:
                raise ComparisonError("reader_limit_exceeded")
            if sample_calls + len(hypothesis.get("reader_plan", [])) > roi.MAX_READER_CALLS_PER_DOCUMENT:
                raise ComparisonError("reader_call_limit_per_document_exceeded")
            if total_calls + len(hypothesis.get("reader_plan", [])) > 2 * roi.MAX_READER_CALLS_PER_DOCUMENT:
                raise ComparisonError("reader_call_limit_exceeded")
            started = time.perf_counter()
            try:
                output = reader_runner(
                    hypothesis, record["image_path"], engine=engine,
                    tesseract=tesseract, tessdata_dir=tessdata_dir,
                )
            except Exception as error:
                if getattr(error, "code", None) == "rapidocr_cpu_provider_validation_failed":
                    # Do not proceed to Tesseract or another ROI when the
                    # actual lazy ONNX session provider violates CPU-only.
                    raise ComparisonError("rapidocr_cpu_provider_validation_failed") from error
                # Keep only class code; exception text might contain OCR or file paths.
                output = {"hypothesis_id": hypothesis["id"], "field_id": hypothesis["field_id"], "readers": [{"reader": "unknown", "status": "error", "reason_code": f"reader_runner_{type(error).__name__.lower()}", "outputs": []}]}
            elapsed = time.perf_counter() - started
            readers = output.get("readers", []) if isinstance(output, dict) else []
            seen = set()
            for reader in readers[:roi.MAX_READERS_PER_ROI]:
                if not isinstance(reader, dict):
                    continue
                name = reader.get("reader") if isinstance(reader.get("reader"), str) else "unknown"
                if name in seen:
                    continue
                seen.add(name)
                candidate_groups = build_candidate_groups({"reader": name, **reader}, hypothesis["field_id"])
                candidate_count = len(candidate_groups)
                item = {
                    "sample_id": record["sample_id"], "hypothesis_id": hypothesis["id"], "field_id": hypothesis["field_id"],
                    "reader": name, "status": reader.get("status", "unknown"),
                    "reason_code": reader.get("reason_code"), "candidate_count": candidate_count,
                    "elapsed_seconds": float(reader.get("elapsed_seconds", elapsed / max(1, len(readers)))),
                    "outputs": reader.get("outputs", []), "words": reader.get("words", []),
                    "candidate_groups": candidate_groups,
                    "confidence_semantics": reader.get("confidence_semantics"),
                    "private_stderr": reader.get("private_stderr", ""),
                }
                sample_results.append(item)
                reader_records.append(item)
                total_calls += 1
                sample_calls += 1
        _private_json(work_root / record["sample_id"] / "reader-results.json", sample_results)
        record["reader_results"] = sample_results
    if total_calls > 2 * roi.MAX_READER_CALLS_PER_DOCUMENT:
        raise ComparisonError("reader_call_limit_exceeded")
    # Optional scoring is strictly after every proposal and reader has finished.
    _gold_entries_after_readers(gold_path, records, reader_records)
    public = _public_summary(records, reader_records, gold_path is not None)
    provider_state = roi.pinned_cpu_engine_state(engine) or getattr(
        engine, "_field_roi_cpu_provider_state",
        "test_adapter_unverified" if test_adapter else ("not_initialized" if engine is None else "injected_engine_not_verified"),
    )
    public["rapidocr_provider_verification"] = provider_state
    public["reader_execution_mode"] = "test_adapter_unverified" if test_adapter else "pinned_cpu_factory_with_actual_provider_guard"
    source_files = [
        Path(__file__), Path(__file__).with_name("field_roi_pipeline.py"),
        ROOT / "ocr/experiments/region_pipeline.py", ROOT / "ocr/experiments/run_algorithm_comparison.py",
    ]
    private_run = {
        "schema_version": "passport-field-roi-private-run/v1",
        "baseline_profile": baseline_profile,
        "model_sha256": hashes,
        "source_sha256": {path.name: _sha256(path) for path in source_files},
        "reader_config": {
            "rapidocr": {
                "requested_providers": ["CPUExecutionProvider"],
                "actual_provider_verification": "lazy_stage_specific_after_each_invocation; unused_stages_not_initialized; both_stages_checked_with_region_pipeline_assert_cpu_only_when_present",
                "provider_verification_state": provider_state,
                "reader_execution_mode": "test_adapter_unverified" if test_adapter else "pinned_cpu_factory_with_actual_provider_guard",
                "models": {roi.MODEL_FILENAMES["detector"]: hashes[roi.MODEL_FILENAMES["detector"]], roi.MODEL_FILENAMES["recognizer"]: hashes[roi.MODEL_FILENAMES["recognizer"]]},
                "det_limit_side_len": 736, "det_limit_type": "min", "use_cls": False,
                "field_modes": {"given_name": "rec_only", "gender": "rec_only_or_det_rec_by_crop_geometry", "birth_place": "det_rec", "issuer": "det_rec"},
            },
            "tesseract": {
                "language": "rus", "oem": 1, "field_modes": {"given_name": "psm7", "gender": "psm7_or_psm6_by_crop_geometry", "birth_place": "psm6", "issuer": "psm6"},
                "timeout_seconds": roi.TESSERACT_TIMEOUT_SECONDS, "maximum_resized_side": roi.MAX_UPSCALE_SIDE,
                "confidence_semantics": "raw_word_confidence_not_calibrated",
            },
        },
        "samples": [
            {"sample_id": item["sample_id"], "image_sha256": item["image_sha256"], "hypothesis_count": len(item["plan"]["hypotheses"])}
            for item in records
        ],
        "timing_boundary": "image crops are processed sequentially; no Go or baseline extraction rerun; timing includes per-reader invocation only",
        "ground_truth_loaded_after_readers": gold_path is not None,
    }
    _private_json(work_root / "run.json", private_run)
    _public_json(output_dir / "field-roi-comparison.json", public)
    markdown = [
        "# Field-local OCR diagnostic comparison", "",
        "- Status: **COMPLETED**", "- Baseline: `native_config_736_min`", "- Images: 2", "- Review-only: yes",
        f"- ROI count: {sum(len(item['plan']['hypotheses']) for item in records)} / 8 maximum",
        f"- Reader invocations: {total_calls} / 16 maximum across two images",
        "- Candidate coverage counts field roles with diagnostic physical-line/block groups; it is not accuracy.",
        "- Baseline 15/20 is reference only; Go extraction was not rerun and no ROI output was selected.",
        "- Reader scores are raw, uncalibrated values. Readers were not voted or pooled.",
    ]
    for sample in public["samples"]:
        markdown.extend(["", f"## {sample['sample_id']}", "", f"Hypotheses: {sample['hypothesis_count']}", "", "| Field | Role quality | Readers | Candidate outputs |", "|---|---|---|---:|"])
        for field in sample["fields"]:
            readers = ", ".join(f"{item['reader']}:{item['status']}" for item in field["reader_statuses"])
            markdown.append(f"| `{field['field_id']}` | `{field['role_quality']}` | {readers} | {field['candidate_count']} |")
        if sample["rejections"]:
            markdown.extend(["", "Rejected hypotheses: " + ", ".join(f"`{row['field_id']}` ({row['reason_code']})" for row in sample["rejections"])])
    markdown.extend(["", "Optional post-hoc metrics are per-reader and do not select or alter candidates.", ""])
    markdown.append("- Post-hoc matching is literal NFC/case/whitespace normalization over each allowed candidate group; Go visual-Latin-to-Cyrillic name proposals are not applied.")
    # Markdown is also mode 0600 through the common writer.
    _write_text(output_dir / "field-roi-comparison.md", "\n".join(markdown))
    return public


def _write_text(path: Path, content: str) -> None:
    _private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def make_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image1", required=True, type=Path, help="First explicit source image")
    parser.add_argument("--image2", required=True, type=Path, help="Second explicit source image")
    parser.add_argument("--baseline-root", type=Path, default=DEFAULT_BASELINE)
    parser.add_argument("--baseline-profile", default="native_config")
    parser.add_argument("--model-dir", type=Path, default=DEFAULT_MODEL_DIR)
    parser.add_argument("--tesseract", type=Path, default=DEFAULT_TESSERACT)
    parser.add_argument("--tessdata-dir", type=Path, default=DEFAULT_TESSDATA_DIR)
    parser.add_argument("--gold", type=Path, help="Optional post-hoc private metrics; read only after ROI and reader completion")
    parser.add_argument("--work-root", type=Path, default=DEFAULT_WORK)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = make_parser().parse_args(argv)
    try:
        images = [args.image1.expanduser().resolve(), args.image2.expanduser().resolve()]
        if len(images) != 2 or any(not path.is_file() for path in images):
            raise ComparisonError("exactly_two_existing_images_required")
        if args.gold is not None and not args.gold.expanduser().resolve().is_file():
            raise ComparisonError("ground_truth_file_missing")
        run_comparison(
            images,
            baseline_root=args.baseline_root.expanduser().resolve(), baseline_profile=args.baseline_profile,
            model_dir=args.model_dir.expanduser().resolve(), tessdata_dir=args.tessdata_dir.expanduser().resolve(),
            tesseract=args.tesseract.expanduser().resolve(), work_root=args.work_root.expanduser().resolve(),
            output_dir=args.output_dir.expanduser().resolve(),
            gold_path=args.gold.expanduser().resolve() if args.gold else None,
        )
        return 0
    except ComparisonError as error:
        try:
            destination = args.output_dir.expanduser().resolve()
            _private_dir(destination)
            _public_json(destination / "field-roi-comparison.json", {
                "schema_version": REPORT_SCHEMA, "status": "FAIL", "reason_code": error.code,
            })
        except OSError:
            return 3
        return 2
    except Exception as error:
        try:
            destination = args.output_dir.expanduser().resolve()
            _private_dir(destination)
            _public_json(destination / "field-roi-comparison.json", {
                "schema_version": REPORT_SCHEMA, "status": "FAIL",
                "reason_code": f"unexpected_{type(error).__name__.lower()}",
            })
        except OSError:
            return 3
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
