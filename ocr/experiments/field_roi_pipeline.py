"""Local, review-only OCR hypotheses derived from existing VIZ observations.

The geometry is deliberately conservative. A returned crop is a diagnostic
proposal, never a selected passport field and never a production autofill.
MRZ rows may bound a crop but are never used as OCR text or field evidence.
"""

from __future__ import annotations

import hashlib
import math
import os
import re
import shutil
import subprocess
import tempfile
import time
import weakref
from datetime import datetime
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PIL import Image, ImageOps


SCHEMA_VERSION = "passport-field-roi-diagnostics/v1"
COORDINATE_FRAME = "exif_normalized_pixels"
FIELD_IDS = ("given_name", "gender", "birth_place", "issuer")
MAX_ROIS_PER_DOCUMENT = 4
MAX_READERS_PER_ROI = 2
MAX_READER_CALLS_PER_DOCUMENT = 8
MAX_UPSCALE_SIDE = 1600
MAX_DETECTOR_PIXELS = 4_000_000
TESSERACT_TIMEOUT_SECONDS = 8.0
TESSERACT_SCALE = 2.0
TESSDATA_FILENAME = "rus.traineddata"
TESSDATA_SHA256 = "e16e5e036cce1d9ec2b00063cf8b54472625b9e14d893a169e2b0dedeb4df225"
DETECTOR_SHA256 = "4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae"
RECOGNIZER_SHA256 = "08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab"
MODEL_FILENAMES = {
    "detector": "ch_PP-OCRv5_det_mobile.onnx",
    "recognizer": "eslav_PP-OCRv5_rec_mobile.onnx",
}
_DATE = re.compile(r"(?<!\d)(\d{2})[.\-/](\d{2})[.\-/](\d{4})(?!\d)")
_DEPARTMENT = re.compile(r"^\s*(?:\d{3}\s*-\s*\d{3}|\d{6})\s*$")
_DIGIT = re.compile(r"\d")
_MRZ_CHARS = re.compile(r"^[A-Z0-9< ]{18,}$")


class FieldROIError(RuntimeError):
    """A safe, coded error that never embeds OCR text or paths."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


_PINNED_ENGINE_STATES: weakref.WeakKeyDictionary[Any, str] = weakref.WeakKeyDictionary()


def pinned_cpu_engine_state(engine: Any) -> str | None:
    """Return state only for the exact engine issued by the pinned factory."""
    try:
        return _PINNED_ENGINE_STATES.get(engine)
    except TypeError:
        return None


def _set_provider_state(engine: Any, state: str) -> None:
    try:
        if engine in _PINNED_ENGINE_STATES:
            _PINNED_ENGINE_STATES[engine] = state
    except TypeError:
        pass
    try:
        engine._field_roi_cpu_provider_state = state
    except Exception:
        pass


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _bounds(quad: Any) -> tuple[float, float, float, float] | None:
    if not isinstance(quad, list) or len(quad) != 4:
        return None
    points: list[tuple[float, float]] = []
    for point in quad:
        if not isinstance(point, list) or len(point) != 2 or not all(_finite_number(n) for n in point):
            return None
        points.append((float(point[0]), float(point[1])))
    return min(p[0] for p in points), min(p[1] for p in points), max(p[0] for p in points), max(p[1] for p in points)


def _center(box: tuple[float, float, float, float]) -> tuple[float, float]:
    return (box[0] + box[2]) / 2, (box[1] + box[3]) / 2


def _line_text(line: dict[str, Any]) -> str:
    value = line.get("text_raw")
    return value if isinstance(value, str) else ""


def _mrz_like(line: dict[str, Any], box: tuple[float, float, float, float], doc: tuple[float, float, float, float]) -> bool:
    text = _line_text(line).strip().upper()
    if "<" in text:
        return True
    width, height = doc[2] - doc[0], doc[3] - doc[1]
    _, cy = _center(box)
    if width <= 0 or height <= 0 or cy < doc[1] + 0.84 * height:
        return False
    return (box[2] - box[0]) > 0.5 * width and _MRZ_CHARS.fullmatch(text) is not None


def _valid_rect(rect: Any, width: int, height: int) -> tuple[int, int, int, int] | None:
    if not isinstance(rect, list) or len(rect) != 4 or not all(_finite_number(x) for x in rect):
        return None
    x0, y0, x1, y1 = (int(round(float(x))) for x in rect)
    if x0 < 0 or y0 < 0 or x1 <= x0 or y1 <= y0 or x1 > width or y1 > height:
        return None
    return x0, y0, x1, y1


def _clip_box(
    box: tuple[float, float, float, float],
    width: int,
    height: int,
    doc: tuple[float, float, float, float],
    *,
    pad_x: float,
    pad_y: float,
    x_limits: tuple[float, float] | None = None,
    y_limits: tuple[float, float] | None = None,
) -> list[int] | None:
    x0, y0, x1, y1 = box
    dx0, dy0, dx1, dy1 = doc
    limit_x0, limit_x1 = x_limits or (dx0, dx1)
    limit_y0, limit_y1 = y_limits or (dy0, dy1)
    crop = [
        max(0, int(math.floor(max(dx0, limit_x0, x0 - pad_x)))),
        max(0, int(math.floor(max(dy0, limit_y0, y0 - pad_y)))),
        min(width, int(math.ceil(min(dx1, limit_x1, x1 + pad_x)))),
        min(height, int(math.ceil(min(dy1, limit_y1, y1 + pad_y)))),
    ]
    if crop[2] <= crop[0] or crop[3] <= crop[1]:
        return None
    return crop


def _union(boxes: list[tuple[float, float, float, float]]) -> tuple[float, float, float, float] | None:
    if not boxes:
        return None
    return min(b[0] for b in boxes), min(b[1] for b in boxes), max(b[2] for b in boxes), max(b[3] for b in boxes)


def _group_rows(lines: list[dict[str, Any]], boxes: dict[str, tuple[float, float, float, float]], tolerance: float) -> list[list[dict[str, Any]]]:
    ordered = sorted(lines, key=lambda line: (_center(boxes[line["id"]])[1], boxes[line["id"]][0], line["id"]))
    rows: list[list[dict[str, Any]]] = []
    for line in ordered:
        y = _center(boxes[line["id"]])[1]
        if not rows or abs(y - _center(boxes[rows[-1][0]["id"]])[1]) > tolerance:
            rows.append([line])
        else:
            rows[-1].append(line)
    return rows


def _page_for(pass_record: dict[str, Any], box: tuple[float, float, float, float], seam: float) -> str:
    return "upper" if _center(box)[1] < seam else "lower"


def _candidate_ids(field: dict[str, Any]) -> set[str]:
    ids: set[str] = set()
    candidates = field.get("candidates")
    if isinstance(candidates, list):
        for candidate in candidates:
            if isinstance(candidate, dict) and isinstance(candidate.get("line_ids"), list):
                ids.update(item for item in candidate["line_ids"] if isinstance(item, str))
    evidence = field.get("evidence")
    if isinstance(evidence, dict) and isinstance(evidence.get("line_ids"), list):
        ids.update(item for item in evidence["line_ids"] if isinstance(item, str))
    return ids


def _anchor_lines(
    field_id: str,
    extraction: dict[str, Any],
    lines_by_id: dict[str, dict[str, Any]],
    pass_by_id: dict[str, dict[str, Any]],
    boxes: dict[str, tuple[float, float, float, float]],
    doc: tuple[float, float, float, float],
) -> list[dict[str, Any]]:
    fields = extraction.get("fields", {})
    field = fields.get(field_id, {}) if isinstance(fields, dict) else {}
    anchors: list[dict[str, Any]] = []
    for line_id in sorted(_candidate_ids(field)):
        line = lines_by_id.get(line_id)
        if not line or line_id not in boxes:
            continue
        pass_record = pass_by_id.get(line.get("pass_id", ""), {})
        if pass_record.get("status") != "ok" or pass_record.get("kind") not in {"original", "document", "upper_page", "lower_page"}:
            continue
        if pass_record.get("rotation_degrees", 0) != 0 or _mrz_like(line, boxes[line_id], doc):
            continue
        anchors.append(line)
    return anchors


def _validated_anchor_lines(
    field_id: str,
    predicate: Callable[[str], bool],
    extraction: dict[str, Any],
    lines_by_id: dict[str, dict[str, Any]],
    pass_by_id: dict[str, dict[str, Any]],
    boxes: dict[str, tuple[float, float, float, float]],
    doc: tuple[float, float, float, float],
) -> list[dict[str, Any]]:
    return [line for line in _anchor_lines(field_id, extraction, lines_by_id, pass_by_id, boxes, doc) if predicate(_line_text(line))]


def _date_text(text: str) -> bool:
    match = _DATE.search(text)
    if match is None:
        return False
    day, month, year = match.groups()
    for separator in (".", "-", "/"):
        if separator in match.group(0):
            try:
                datetime.strptime(f"{day}{separator}{month}{separator}{year}", f"%d{separator}%m{separator}%Y")
                return True
            except ValueError:
                return False
    return False


def _name_row_text(text: str) -> bool:
    if not text or _DIGIT.search(text) or any(char.isalpha() for char in text) is False:
        return False
    folded = text.upper().replace("Ё", "Е")
    if any(label in folded for label in ("ФАМИЛИЯ", "ИМЯ", "ОТЧЕСТВО", "ПАСПОРТ", "РОССИЙСКОЙ", "ФЕДЕРАЦИИ")):
        return False
    return True


def _anchor(field_id: str, line: dict[str, Any], relation: str) -> dict[str, str]:
    return {"role": field_id, "line_id": line["id"], "pass_id": line["pass_id"], "relation": relation}


def _proposal(
    field_id: str,
    quality: str,
    reasons: list[str],
    lines: list[dict[str, Any]],
    crop: list[int],
    adjacent: list[dict[str, str]],
    reader_mode: str,
) -> dict[str, Any]:
    pass_ids = sorted({line["pass_id"] for line in lines})
    line_ids = sorted({line["id"] for line in lines})
    return {
        "id": f"{field_id}-roi-01",
        "field_id": field_id,
        "role_quality": {"state": quality, "reason_codes": reasons},
        "source_pass_ids": pass_ids,
        "source_line_ids": line_ids,
        "crop_bounds": crop,
        "coordinate_frame": COORDINATE_FRAME,
        "adjacent_anchors": adjacent,
        "proposal_method": "viz_layout_v1",
        "reader_plan": [
            {"reader": "rapidocr", "mode": reader_mode},
            {"reader": "tesseract_rus", "mode": "psm7" if reader_mode == "rec_only" else "psm6"},
        ],
    }


def _rejection(field_id: str, reason: str) -> dict[str, str]:
    return {"field_id": field_id, "reason_code": reason}


def plan_field_rois(observations: dict[str, Any], extraction: dict[str, Any]) -> dict[str, Any]:
    """Create at most one privacy-safe crop hypothesis per requested field.

    The function is pure: it does not load OCR engines, read images, access
    gold, or mutate either input. Returned proposals are review-only.
    """
    rejected = [_rejection(field_id, "input_context_invalid") for field_id in FIELD_IDS]
    if not isinstance(observations, dict) or not isinstance(extraction, dict):
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    if observations.get("schema_version") not in {"passport-ocr-observations/v1", "passport-ocr-observations/v2"}:
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    image = observations.get("image")
    document = observations.get("document")
    if not isinstance(image, dict) or image.get("coordinate_frame") != COORDINATE_FRAME:
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    if not isinstance(image.get("id"), str) or not image.get("id") or not isinstance(image.get("sha256"), str) or re.fullmatch(r"[0-9a-fA-F]{64}", image["sha256"]) is None:
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    width, height = image.get("width"), image.get("height")
    if isinstance(width, bool) or not isinstance(width, int) or isinstance(height, bool) or not isinstance(height, int) or width < 1 or height < 1:
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    if extraction.get("schema_version") != "passport-extraction/v1" or extraction.get("image_id") != image.get("id"):
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    if not isinstance(document, dict):
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    doc_list = document.get("bounds")
    doc_rect = _valid_rect(doc_list, width, height)
    seam = document.get("seam_y")
    if (
        doc_rect is None or not _finite_number(seam) or not doc_rect[1] < float(seam) < doc_rect[3]
        or document.get("quality") != "candidate"
        or document.get("method") not in {"contour", "ocr_bbox_envelope"}
    ):
        return {
            "schema_version": SCHEMA_VERSION,
            "hypotheses": [],
            "rejections": [_rejection(field_id, "document_page_geometry_unverified") for field_id in FIELD_IDS],
        }
    doc = tuple(float(v) for v in doc_rect)
    doc_width, doc_height = doc[2] - doc[0], doc[3] - doc[1]

    passes_list = observations.get("passes")
    lines_list = observations.get("lines")
    if not isinstance(passes_list, list) or not isinstance(lines_list, list):
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}
    pass_by_id = {p.get("id"): p for p in passes_list if isinstance(p, dict) and isinstance(p.get("id"), str)}
    lines_by_id = {line.get("id"): line for line in lines_list if isinstance(line, dict) and isinstance(line.get("id"), str)}
    boxes: dict[str, tuple[float, float, float, float]] = {}
    viz_lines: list[dict[str, Any]] = []
    viz_mrz_boundary_lines: dict[str, list[dict[str, Any]]] = {}
    for line in lines_list:
        if not isinstance(line, dict) or not isinstance(line.get("id"), str):
            continue
        pass_record = pass_by_id.get(line.get("pass_id"), {})
        if (
            pass_record.get("status") != "ok"
            or pass_record.get("kind") not in {"original", "document", "upper_page", "lower_page"}
            or pass_record.get("rotation_degrees", 0) != 0
        ):
            continue
        box = _bounds(line.get("quad"))
        if box is None or box[0] < 0 or box[1] < 0 or box[2] > width or box[3] > height or box[2] <= box[0] or box[3] <= box[1]:
            continue
        if _mrz_like(line, box, doc):
            viz_mrz_boundary_lines.setdefault(line["pass_id"], []).append(line)
            continue
        boxes[line["id"]] = box
        viz_lines.append(line)

    fields = extraction.get("fields")
    if not isinstance(fields, dict):
        return {"schema_version": SCHEMA_VERSION, "hypotheses": [], "rejections": rejected}

    hypotheses: list[dict[str, Any]] = []
    rejections: list[dict[str, str]] = []
    dob_lines = _validated_anchor_lines("birth_date", _date_text, extraction, lines_by_id, pass_by_id, boxes, doc)
    issue_lines = _validated_anchor_lines("issue_date", _date_text, extraction, lines_by_id, pass_by_id, boxes, doc)
    dept_lines = _validated_anchor_lines("department_code", lambda text: _DEPARTMENT.fullmatch(text) is not None, extraction, lines_by_id, pass_by_id, boxes, doc)

    def ambiguous_anchor_passes(lines: list[dict[str, Any]]) -> set[str]:
        by_pass: dict[str, list[dict[str, Any]]] = {}
        for line in lines:
            by_pass.setdefault(line["pass_id"], []).append(line)
        ambiguous = set()
        for pass_id, candidates in by_pass.items():
            centers = [_center(boxes[line["id"]]) for line in candidates]
            if any(math.hypot(x - other_x, y - other_y) > 0.04 * doc_height for i, (x, y) in enumerate(centers) for other_x, other_y in centers[i + 1:]):
                ambiguous.add(pass_id)
        return ambiguous

    ambiguous_dob_passes = ambiguous_anchor_passes(dob_lines)
    ambiguous_issue_passes = ambiguous_anchor_passes(issue_lines)
    ambiguous_dept_passes = ambiguous_anchor_passes(dept_lines)
    dob_lines = [line for line in dob_lines if line["pass_id"] not in ambiguous_dob_passes]
    issue_lines = [line for line in issue_lines if line["pass_id"] not in ambiguous_issue_passes]
    dept_lines = [line for line in dept_lines if line["pass_id"] not in ambiguous_dept_passes]

    def same_pass_viz_mrz_boundary(pass_id: str) -> list[dict[str, Any]]:
        return [
            line for line in viz_mrz_boundary_lines.get(pass_id, [])
            if _page_for(pass_by_id[pass_id], _bounds(line["quad"]), float(seam)) == "lower"
            and (_bounds(line["quad"])[2] - _bounds(line["quad"])[0]) >= 0.30 * doc_width
        ]

    def same_pass(lines: list[dict[str, Any]], pass_id: str, page: str | None = None) -> list[dict[str, Any]]:
        selected = []
        for line in lines:
            if line.get("pass_id") != pass_id or line["id"] not in boxes:
                continue
            p = pass_by_id[pass_id]
            if page and _page_for(p, boxes[line["id"]], float(seam)) != page:
                continue
            selected.append(line)
        return selected

    # Name role is accepted as supported only when the complete three-row VIZ
    # group and a same-pass, validated DOB line identify the middle row.
    given_done = False
    supported_name_contexts: dict[str, dict[str, Any]] = {}
    name_line_ids: set[str] = set()
    for name_field in ("surname", "given_name", "patronymic"):
        field = fields.get(name_field, {})
        if isinstance(field, dict):
            name_line_ids.update(_candidate_ids(field))
    name_records = [line for line in viz_lines if line["id"] in name_line_ids and _name_row_text(_line_text(line))]
    for date_line in sorted(dob_lines, key=lambda line: (line.get("pass_id") != "original", line["id"])):
        pid = date_line["pass_id"]
        if _page_for(pass_by_id[pid], boxes[date_line["id"]], float(seam)) != "lower":
            continue
        same_names = same_pass(name_records, pid, "lower")
        same_dates = same_pass(dob_lines, pid, "lower")
        if not same_names or not same_dates:
            continue
        date_box = boxes[date_line["id"]]
        before = [line for line in same_names if _center(boxes[line["id"]])[1] < _center(date_box)[1]]
        rows = _group_rows(before, boxes, max(3.0, 0.018 * doc_height))
        # Require three candidate-backed rows in a stable text column.
        rows = [row for row in rows if row]
        if len(rows) != 3:
            continue
        ordered_name_fields = ("surname", "given_name", "patronymic")
        if any(
            not any(line["id"] in _candidate_ids(fields.get(field_id, {})) for line in row)
            for field_id, row in zip(ordered_name_fields, rows)
        ):
            continue
        left_edges = [min(boxes[line["id"]][0] for line in row) for row in rows]
        if max(left_edges) - min(left_edges) > 0.12 * doc_width:
            continue
        middle = rows[1]
        given_ids = _candidate_ids(fields.get("given_name", {})) if isinstance(fields.get("given_name"), dict) else set()
        if not any(line["id"] in given_ids for line in middle):
            continue
        row_box = _union([boxes[line["id"]] for line in middle])
        assert row_box is not None
        row_height = row_box[3] - row_box[1]
        # Preserve unread trailing row text while stopping at the detected page edge.
        name_column_left = max(doc[0], row_box[0] - 2.0 * row_height)
        name_column_right = min(doc[2], max(row_box[2], date_box[2]) + 2.0 * row_height)
        crop = _clip_box(
            row_box, width, height, doc,
            pad_x=max(row_height * 2.0, 3.0),
            pad_y=max(3.0, row_height * 0.65),
            x_limits=(name_column_left, name_column_right),
            y_limits=(float(seam), date_box[1]),
        )
        if crop is None:
            continue
        hypotheses.append(_proposal(
            "given_name", "supported_row", ["three_aligned_name_rows_before_validated_dob", "middle_full_row_selected"],
            middle, crop,
            [_anchor("birth_date", date_line, "upper_row_boundary")],
            "rec_only",
        ))
        supported_name_contexts[pid] = {
            "row": middle,
            "row_box": row_box,
            "date_line": date_line,
            "date_box": date_box,
        }
        given_done = True
        break
    if not given_done:
        rejections.append(_rejection("given_name", "three_aligned_name_rows_or_middle_row_unverified"))

    # Weak gender diagnostics use explicit VIZ evidence when available. A
    # missing-field probe is permitted only with an independent supported
    # three-name-row context and crops a band strictly left of the DOB column.
    gender_done = False
    gender_evidence = _anchor_lines("gender", extraction, lines_by_id, pass_by_id, boxes, doc)
    for date_line in sorted(dob_lines, key=lambda line: (line.get("pass_id") != "original", line["id"])):
        pid = date_line["pass_id"]
        if _page_for(pass_by_id[pid], boxes[date_line["id"]], float(seam)) != "lower":
            continue
        date_box = boxes[date_line["id"]]
        date_y = _center(date_box)[1]
        date_height = max(1.0, date_box[3] - date_box[1])
        date_ids = {line["id"] for line in dob_lines if line.get("pass_id") == pid}
        same_pass_gender = same_pass(gender_evidence, pid, "lower")
        near_gender_evidence = []
        for line in same_pass_gender:
            box = boxes[line["id"]]
            if (
                line["id"] not in date_ids
                and not _date_text(_line_text(line))
                and _page_for(pass_by_id[pid], box, float(seam)) == "lower"
                and abs(_center(box)[1] - date_y) <= 0.055 * doc_height
                and box[0] < date_box[0]
                and box[2] >= date_box[0] - 2.0 * date_height
                and box[3] - box[1] <= max(4.0 * date_height, 0.08 * doc_height)
            ):
                near_gender_evidence.append(line)
        rows = _group_rows(near_gender_evidence, boxes, max(3.0, 0.018 * doc_height))
        candidate_rows = min(rows, key=lambda row: abs(_center(boxes[row[0]["id"]])[1] - date_y)) if rows else []
        missing_probe = not candidate_rows
        name_context = supported_name_contexts.get(pid)
        if missing_probe:
            # A supplied but geometrically incompatible source is rejected;
            # do not reinterpret it as an empty-field probe.
            non_date_gender_sources = [
                line for line in same_pass_gender
                if line["id"] not in date_ids and not _date_text(_line_text(line))
            ]
            if non_date_gender_sources:
                continue
            if name_context is None or name_context["date_line"]["id"] != date_line["id"]:
                continue
            name_box = name_context["row_box"]
            # The empty-field probe looks into the left-of-date neighborhood;
            # keep the date column itself out of the crop and don't label the
            # name row as gender source evidence.
            date_width = max(1.0, date_box[2] - date_box[0])
            left_x0 = max(doc[0], min(name_box[0], date_box[0]) - date_width - 2.0 * date_height)
            left_x1 = min(date_box[0], max(name_box[2], date_box[0] - 2.0 * date_height))
            band_half_height = max(1.5 * date_height, 3.0)
            same_pass_mrz = same_pass_viz_mrz_boundary(pid)
            mrz_tops = [_bounds(line["quad"])[1] for line in same_pass_mrz]
            gender_lower_bound = min(mrz_tops) if mrz_tops else doc[3]
            crop = _clip_box(
                (left_x0, date_y - band_half_height, left_x1, date_y + band_half_height),
                width, height, doc,
                pad_x=0.0, pad_y=0.0,
                x_limits=(doc[0], date_box[0]),
                y_limits=(float(seam), gender_lower_bound),
            )
            if crop is None or crop[2] > date_box[0] or crop[2] <= crop[0]:
                continue
            candidate_rows = []
            crop_box = (float(crop[0]), float(crop[1]), float(crop[2]), float(crop[3]))
        else:
            # Explicit field evidence bounds a weak review-only crop. Never
            # harvest arbitrary nearby place/name lines as gender candidates.
            crop_box = _union([boxes[line["id"]] for line in candidate_rows] + [date_box])
            assert crop_box is not None
            pad_y = max(4.0, min((crop_box[3] - crop_box[1]) * 0.35, 2.0 * date_height))
            same_pass_mrz = same_pass_viz_mrz_boundary(pid)
            mrz_tops = [_bounds(line["quad"])[1] for line in same_pass_mrz]
            gender_lower_bound = min(mrz_tops) if mrz_tops else doc[3]
            crop = _clip_box(crop_box, width, height, doc, pad_x=max(8.0, 2.0 * date_height), pad_y=pad_y, y_limits=(float(seam), gender_lower_bound))
            if crop is None:
                continue
        gender_height = max(crop_box[3] - crop_box[1], 1.0)
        mrz_tops = [_bounds(line["quad"])[1] for line in same_pass_mrz]
        reasons = ["gender_role_layout_ambiguous_review_only"]
        if missing_probe:
            reasons.append("gender_neighborhood_from_name_date_anchors")
        adjacent = [_anchor("birth_date", date_line, "date_neighborhood")]
        if missing_probe and name_context is not None:
            adjacent.append(_anchor("given_name", name_context["row"][0], "independent_name_group_support_only"))
        if same_pass_mrz and mrz_tops:
            lower_line = min(
                same_pass_mrz,
                key=lambda line: _bounds(line["quad"])[1],
            )
            adjacent.append(_anchor("mrz_boundary", lower_line, "lower_crop_boundary_only"))
        reader_mode = "rec_only" if crop[3] - crop[1] <= 0.055 * doc_height else "det_rec"
        hypotheses.append(_proposal("gender", "weak", reasons, candidate_rows, crop, adjacent, reader_mode))
        gender_done = True
        break
    if not gender_done:
        rejections.append(_rejection("gender", "validated_dob_neighborhood_unavailable"))

    # Place is a VIZ block below DOB and above a source MRZ boundary. MRZ is
    # referenced only for crop bounds and excluded from source_line_ids.
    place_done = False
    place_evidence = _anchor_lines("birth_place", extraction, lines_by_id, pass_by_id, boxes, doc)
    for date_line in sorted(dob_lines, key=lambda line: (line.get("pass_id") != "original", line["id"])):
        pid = date_line["pass_id"]
        if _page_for(pass_by_id[pid], boxes[date_line["id"]], float(seam)) != "lower":
            continue
        mrz_for_pass = same_pass_viz_mrz_boundary(pid)
        mrz_boxes = [box for line in mrz_for_pass if (box := _bounds(line.get("quad"))) is not None and _center(box)[1] > float(seam)]
        if not mrz_boxes:
            # Do not pool independent MRZ reader passes: only same-pass VIZ
            # geometry may bound this field block.
            continue
        mrz_top = min(box[1] for box in mrz_boxes)
        date_box = boxes[date_line["id"]]
        date_bottom = date_box[3]
        candidates = same_pass(place_evidence, pid, "lower")
        column_left = date_box[0] - max(8.0, 2.0 * (date_box[3] - date_box[1]))
        if not candidates:
            candidates = [
                line for line in viz_lines if line.get("pass_id") == pid
                and _page_for(pass_by_id[pid], boxes[line["id"]], float(seam)) == "lower"
                and date_bottom < _center(boxes[line["id"]])[1] < mrz_top
                and boxes[line["id"]][2] > column_left
            ]
        candidates = [line for line in candidates if date_bottom < _center(boxes[line["id"]])[1] < mrz_top]
        if not candidates:
            continue
        block = _union([boxes[line["id"]] for line in candidates])
        assert block is not None
        column_left = max(doc[0], min(column_left, block[0] - 2.0 * max(1.0, block[3] - block[1])))
        # Include enough vertical context for wrapped/unread rows, stopping at
        # DOB above and actual VIZ/MRZ/page boundary below.
        crop = _clip_box(
            block, width, height, doc,
            pad_x=max(10.0, 2.0 * (block[3] - block[1])), pad_y=max(8.0, (block[3] - block[1]) * 0.5),
            x_limits=(column_left, doc[2]),
            y_limits=(date_bottom, mrz_top),
        )
        if crop is None:
            continue
        boundary = min(mrz_for_pass, key=lambda line: _bounds(line["quad"])[1])
        adjacent = [_anchor("birth_date", date_line, "upper_block_boundary"), _anchor("mrz_boundary", boundary, "lower_block_boundary_only")]
        reasons = ["birth_place_role_layout_ambiguous_review_only"]
        if not place_evidence:
            reasons.append("field_evidence_missing_bounded_block_only")
        hypotheses.append(_proposal("birth_place", "weak", reasons, candidates, crop, adjacent, "det_rec"))
        place_done = True
        break
    if not place_done:
        rejections.append(_rejection("birth_place", "dob_or_observed_mrz_boundary_unavailable"))

    # Issuer block requires same-pass issue date and department-code anchors.
    issuer_done = False
    issuer_evidence = _anchor_lines("issuer", extraction, lines_by_id, pass_by_id, boxes, doc)
    for date_line in sorted(issue_lines, key=lambda line: (line.get("pass_id") != "original", line["id"])):
        pid = date_line["pass_id"]
        if _page_for(pass_by_id[pid], boxes[date_line["id"]], float(seam)) != "upper":
            continue
        dept_anchor = next((line for line in same_pass(dept_lines, pid, "upper") if abs(_center(boxes[line["id"]])[1] - _center(boxes[date_line["id"]])[1]) <= 0.04 * doc_height), None)
        if dept_anchor is None:
            continue
        date_y = _center(boxes[date_line["id"]])[1]
        candidates = [line for line in same_pass(issuer_evidence, pid, "upper") if _center(boxes[line["id"]])[1] < date_y]
        if not candidates:
            continue
        block = _union([boxes[line["id"]] for line in candidates])
        assert block is not None
        crop = _clip_box(
            block, width, height, doc,
            pad_x=max(10.0, 0.06 * doc_width), pad_y=max(6.0, (block[3] - block[1]) * 0.35),
            x_limits=(doc[0], doc[2]), y_limits=(doc[1], boxes[date_line["id"]][1]),
        )
        if crop is None:
            continue
        hypotheses.append(_proposal(
            "issuer", "weak", ["issuer_block_role_layout_ambiguous_review_only"], candidates, crop,
            [_anchor("issue_date", date_line, "lower_block_boundary"), _anchor("department_code", dept_anchor, "adjacent_field_boundary")],
            "det_rec",
        ))
        issuer_done = True
        break
    if not issuer_done:
        rejections.append(_rejection("issuer", "same_pass_issue_date_and_department_anchors_unavailable"))

    hypotheses = hypotheses[:MAX_ROIS_PER_DOCUMENT]
    if len({item["field_id"] for item in hypotheses}) != len(hypotheses) or len(hypotheses) > MAX_ROIS_PER_DOCUMENT:
        raise FieldROIError("roi_limit_invariant_failed")
    return {"schema_version": SCHEMA_VERSION, "hypotheses": hypotheses, "rejections": rejections}


def resize_crop(crop_rgb: np.ndarray, *, max_side: int = MAX_UPSCALE_SIDE) -> tuple[np.ndarray, tuple[float, float]]:
    """Allow at most a two-times bicubic upscale, with actual x/y scale returned."""
    if not isinstance(crop_rgb, np.ndarray) or crop_rgb.ndim != 3 or crop_rgb.shape[2] != 3 or crop_rgb.size == 0:
        raise FieldROIError("crop_image_invalid")
    source = Image.fromarray(crop_rgb.astype(np.uint8, copy=False))
    width, height = source.size
    scale = min(TESSERACT_SCALE, max_side / max(width, height))
    if abs(scale - 1.0) < 1e-9:
        return np.asarray(source), (1.0, 1.0)
    target = (max(1, int(round(width * scale))), max(1, int(round(height * scale))))
    scaled = source.resize(target, Image.Resampling.BICUBIC)
    return np.asarray(scaled), (target[0] / width, target[1] / height)


def map_box_to_source(
    box: list[float] | tuple[float, float, float, float],
    crop_bounds: list[int] | tuple[int, int, int, int],
    scale_xy: tuple[float, float],
) -> list[float]:
    x0, y0, x1, y1 = crop_bounds
    sx, sy = scale_xy
    if not all(_finite_number(x) and float(x) > 0 for x in (sx, sy)):
        raise FieldROIError("crop_scale_invalid")
    bx0, by0, bx1, by1 = box
    return [x0 + bx0 / sx, y0 + by0 / sy, x0 + bx1 / sx, y0 + by1 / sy]


def parse_tesseract_tsv(stdout: str) -> list[dict[str, Any]]:
    """Parse Tesseract's standard 12-column TSV without CSV quote semantics."""
    import csv
    import io

    expected = ("level", "page_num", "block_num", "par_num", "line_num", "word_num", "left", "top", "width", "height", "conf", "text")
    reader = csv.reader(io.StringIO(stdout, newline=""), delimiter="\t", quoting=csv.QUOTE_NONE, strict=True)
    try:
        header = tuple(next(reader))
    except (StopIteration, csv.Error) as error:
        raise FieldROIError("tesseract_tsv_invalid") from error
    if header != expected:
        raise FieldROIError("tesseract_tsv_invalid")
    parsed: list[dict[str, Any]] = []
    try:
        for row in reader:
            if len(row) != len(expected):
                raise FieldROIError("tesseract_tsv_invalid")
            if row[0] != "5" or not row[11]:
                continue
            try:
                page_num, block_num, par_num, line_num, word_num = map(int, row[1:6])
                left, top, width, height = map(int, row[6:10])
                confidence = float(row[10])
            except ValueError as error:
                raise FieldROIError("tesseract_tsv_invalid") from error
            if width <= 0 or height <= 0 or not math.isfinite(confidence):
                continue
            parsed.append({
                "text_raw": row[11], "raw_confidence": confidence,
                "page_num": page_num, "block_num": block_num, "par_num": par_num,
                "line_num": line_num, "word_num": word_num,
                "geometry_kind": "tesseract_tsv_word",
                "box_crop": [left, top, left + width, top + height],
            })
    except (csv.Error, UnicodeError) as error:
        raise FieldROIError("tesseract_tsv_invalid") from error
    return parsed


def run_tesseract_reader(
    executable: str | Path,
    tessdata_dir: str | Path,
    crop_rgb: np.ndarray,
    *,
    mode: str,
    timeout_seconds: float = TESSERACT_TIMEOUT_SECONDS,
) -> dict[str, Any]:
    """Run one bounded private Tesseract invocation; errors expose codes only."""
    binary = Path(executable)
    tessdata = Path(tessdata_dir)
    if not binary.is_file():
        return {"status": "skipped", "reason_code": "tesseract_unavailable", "words": []}
    model = tessdata / TESSDATA_FILENAME
    if not model.is_file() or sha256_file(model) != TESSDATA_SHA256:
        return {"status": "skipped", "reason_code": "tessdata_pin_mismatch", "words": []}
    if mode not in {"psm6", "psm7"}:
        return {"status": "skipped", "reason_code": "tesseract_mode_invalid", "words": []}
    scaled, scale_xy = resize_crop(crop_rgb)
    started = time.perf_counter()
    private_dir = Path(tempfile.mkdtemp(prefix="passport-field-roi-"))
    private_dir.chmod(0o700)
    try:
        input_path = private_dir / "crop.png"
        Image.fromarray(scaled).save(input_path, format="PNG")
        input_path.chmod(0o600)
        command = [
            str(binary), str(input_path), "stdout", "-l", "rus", "--oem", "1", "--psm", "7" if mode == "psm7" else "6",
            "--tessdata-dir", str(tessdata), "-c", "tessedit_create_tsv=1",
        ]
        child_env = os.environ.copy()
        child_env.pop("TESSDATA_PREFIX", None)
        try:
            completed = subprocess.run(
                command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, encoding="utf-8", errors="replace", timeout=timeout_seconds, check=False,
                shell=False, env=child_env,
            )
        except subprocess.TimeoutExpired as error:
            stderr = error.stderr.decode("utf-8", "replace") if isinstance(error.stderr, bytes) else error.stderr
            return {"status": "error", "reason_code": "tesseract_timeout", "elapsed_seconds": time.perf_counter() - started, "private_stderr": stderr or "", "words": []}
        except OSError:
            return {"status": "error", "reason_code": "tesseract_failed", "elapsed_seconds": time.perf_counter() - started, "words": []}
        if completed.returncode != 0:
            return {"status": "error", "reason_code": "tesseract_failed", "elapsed_seconds": time.perf_counter() - started, "private_stderr": completed.stderr, "words": []}
        try:
            words = parse_tesseract_tsv(completed.stdout)
        except FieldROIError:
            return {"status": "error", "reason_code": "tesseract_tsv_invalid", "elapsed_seconds": time.perf_counter() - started, "private_stderr": completed.stderr, "words": []}
        for word in words:
            word["box_source"] = map_box_to_source(word["box_crop"], (0, 0, crop_rgb.shape[1], crop_rgb.shape[0]), scale_xy)
        return {
            "status": "ok" if words else "empty",
            "elapsed_seconds": time.perf_counter() - started,
            "scale_xy": list(scale_xy),
            "private_stderr": completed.stderr,
            "confidence_semantics": "raw_tesseract_word_confidence_not_calibrated",
            "words": words,
        }
    finally:
        shutil.rmtree(private_dir, ignore_errors=True)


def rapidocr_reader(engine: Any, crop_rgb: np.ndarray, *, mode: str) -> dict[str, Any]:
    """Run an already-created pinned CPU RapidOCR engine on one crop."""
    if mode not in {"rec_only", "det_rec"}:
        return {"status": "skipped", "reason_code": "rapidocr_mode_invalid", "outputs": []}
    scaled, scale_xy = resize_crop(crop_rgb)
    started = time.perf_counter()
    try:
        import cv2

        bgr = cv2.cvtColor(scaled, cv2.COLOR_RGB2BGR)
        result = engine(bgr, use_det=(mode == "det_rec"), use_cls=False, use_rec=True)
    except Exception as error:  # The message can contain raw OCR or local paths.
        return {"status": "error", "reason_code": f"rapidocr_{type(error).__name__.lower()}", "outputs": [], "elapsed_seconds": time.perf_counter() - started}
    outputs: list[dict[str, Any]] = []
    texts = getattr(result, "txts", None)
    if texts is None:
        texts = []
    boxes = getattr(result, "boxes", None)
    scores = getattr(result, "scores", None)
    for index, value in enumerate(texts):
        record: dict[str, Any] = {"text_raw": str(value), "raw_confidence": None, "geometry_kind": "entire_crop", "box_source": None}
        if mode == "det_rec" and boxes is not None and index < len(boxes):
            try:
                raw = np.asarray(boxes[index], dtype=float).reshape(4, 2)
                if np.isfinite(raw).all():
                    record["box_source"] = [
                        float(np.min(raw[:, 0]) / scale_xy[0]), float(np.min(raw[:, 1]) / scale_xy[1]),
                        float(np.max(raw[:, 0]) / scale_xy[0]), float(np.max(raw[:, 1]) / scale_xy[1]),
                    ]
                    record["geometry_kind"] = "detector_word_or_line_box"
            except (TypeError, ValueError):
                pass
        if scores is not None and index < len(scores):
            try:
                score = float(scores[index])
                record["raw_confidence"] = score if math.isfinite(score) else None
            except (TypeError, ValueError):
                pass
        outputs.append(record)
    return {
        "status": "ok" if outputs else "empty",
        "elapsed_seconds": time.perf_counter() - started,
        "scale_xy": list(scale_xy),
        "confidence_semantics": "raw_rapidocr_score_not_calibrated",
        "outputs": outputs,
    }


def create_pinned_cpu_engine(model_dir: str | Path) -> Any:
    """Load only the explicitly pinned PP-OCRv5 pair using the installed factory."""
    model_root = Path(model_dir)
    detector = model_root / MODEL_FILENAMES["detector"]
    recognizer = model_root / MODEL_FILENAMES["recognizer"]
    if not detector.is_file() or sha256_file(detector) != DETECTOR_SHA256:
        raise FieldROIError("detector_pin_mismatch")
    if not recognizer.is_file() or sha256_file(recognizer) != RECOGNIZER_SHA256:
        raise FieldROIError("recognizer_pin_mismatch")
    from ocr.experiments import region_pipeline

    engine = region_pipeline.create_cpu_engine(detector, recognizer)
    config = region_pipeline._engine_detector_config(engine)
    if config.get("limit_side_len") != 736 or config.get("limit_type") != "min":
        raise FieldROIError("rapidocr_default_profile_mismatch")
    # RapidOCR 3.10 lazily creates ONNX sessions on first invocation. The
    # factory is strict CPU-only; actual providers are checked immediately
    # after that first invocation, before any other reader can run.
    try:
        _PINNED_ENGINE_STATES[engine] = "pending_lazy_first_call"
    except Exception as error:
        raise FieldROIError("rapidocr_engine_state_unavailable") from error
    _set_provider_state(engine, "pending_lazy_first_call")
    _verify_existing_cpu_sessions(engine)
    return engine


def _verify_existing_cpu_sessions(engine: Any) -> None:
    """Check already-created lazy sessions without initializing new stages."""
    try:
        from ocr.experiments import region_pipeline

        sessions = region_pipeline.cpu_provider_report(engine).get("sessions", {})
        actual = {role: sessions.get(role) for role in ("det", "rec")}
        present = {role for role, providers in actual.items() if providers is not None}
        if not present:
            return
        if present == {"det", "rec"}:
            region_pipeline._assert_cpu_only(engine)
            _set_provider_state(engine, "verified_cpu_detector_and_recognizer")
            return
        for role in present:
            if actual[role] != ["CPUExecutionProvider"]:
                raise FieldROIError("rapidocr_cpu_provider_validation_failed")
        _set_provider_state(engine, f"verified_cpu_{next(iter(present))}_only_existing")
    except Exception as error:
        try:
            _set_provider_state(engine, "failed")
        except Exception:
            pass
        if isinstance(error, FieldROIError):
            raise
        raise FieldROIError("rapidocr_cpu_provider_validation_failed") from error


def _verify_lazy_cpu_provider_after_call(engine: Any, mode: str) -> None:
    """Validate actual providers for stages initialized by this RapidOCR call."""
    state = pinned_cpu_engine_state(engine) or getattr(engine, "_field_roi_cpu_provider_state", None)
    if state not in {
        "pending_lazy_first_call", "verified_cpu_rec_only_detector_not_used",
        "verified_cpu_det_only_existing", "verified_cpu_rec_only_existing",
    }:
        return
    try:
        from ocr.experiments import region_pipeline
        report = region_pipeline.cpu_provider_report(engine)
        sessions = report.get("sessions", {})
        # RapidOCR's installed __call__/run_ocr_steps is lazy by stage: a
        # rec-only invocation creates the recognizer but never the detector.
        # Verify exactly used stages; once both exist, use the shared strict
        # helper which also rejects unknown or extra actual providers.
        has_det = sessions.get("det") is not None
        has_rec = sessions.get("rec") is not None
        if has_det and has_rec:
            region_pipeline._assert_cpu_only(engine)
            next_state = "verified_cpu_detector_and_recognizer"
        elif mode == "rec_only" and has_rec and not has_det and sessions.get("rec") == ["CPUExecutionProvider"]:
            next_state = "verified_cpu_rec_only_detector_not_used"
        else:
            raise FieldROIError("rapidocr_cpu_provider_validation_failed")
    except Exception as error:
        if isinstance(error, FieldROIError):
            try:
                engine._field_roi_cpu_provider_state = "failed"
            except Exception:
                pass
            raise
        _set_provider_state(engine, "failed")
        raise FieldROIError("rapidocr_cpu_provider_validation_failed") from error
    _set_provider_state(engine, next_state)


def read_image_crop(image_path: str | Path, bounds: list[int]) -> np.ndarray:
    """Read one EXIF-normalized crop from the original image."""
    try:
        image = Image.open(image_path)
        image = ImageOps.exif_transpose(image).convert("RGB")
        x0, y0, x1, y1 = bounds
        if x0 < 0 or y0 < 0 or x1 > image.width or y1 > image.height or x1 <= x0 or y1 <= y0:
            raise FieldROIError("crop_bounds_out_of_image")
        return np.asarray(image.crop((x0, y0, x1, y1)))
    except FieldROIError:
        raise
    except Exception as error:
        raise FieldROIError("image_crop_read_failed") from error


def run_hypothesis_readers(
    hypothesis: dict[str, Any],
    image_path: str | Path,
    *,
    engine: Any,
    tesseract: str | Path,
    tessdata_dir: str | Path,
) -> dict[str, Any]:
    """Call at most the two declared readers once each, sequentially."""
    bounds = hypothesis.get("crop_bounds")
    if not isinstance(bounds, list) or len(bounds) != 4:
        raise FieldROIError("hypothesis_crop_bounds_invalid")
    crop = read_image_crop(image_path, bounds)
    result: dict[str, Any] = {"hypothesis_id": hypothesis.get("id"), "field_id": hypothesis.get("field_id"), "readers": []}
    for reader_spec in hypothesis.get("reader_plan", [])[:MAX_READERS_PER_ROI]:
        if not isinstance(reader_spec, dict):
            continue
        if reader_spec.get("reader") == "rapidocr":
            mode = reader_spec.get("mode", "")
            if mode == "det_rec":
                try:
                    from ocr.experiments import region_pipeline
                    from ocr.experiments.run_resolution_comparison import estimate_detector_pixels

                    config = region_pipeline._engine_detector_config(engine)
                    estimated_pixels = estimate_detector_pixels(crop.shape[1], crop.shape[0], config)
                except Exception:
                    estimated_pixels = None
                if estimated_pixels is None:
                    output = {"status": "skipped", "reason_code": "detector_budget_unavailable", "outputs": []}
                elif estimated_pixels > MAX_DETECTOR_PIXELS:
                    output = {"status": "skipped", "reason_code": "detector_pixel_budget_exceeded", "estimated_detector_pixels": estimated_pixels, "outputs": []}
                else:
                    try:
                        output = rapidocr_reader(engine, crop, mode=mode)
                    finally:
                        _verify_lazy_cpu_provider_after_call(engine, mode)
            else:
                try:
                    output = rapidocr_reader(engine, crop, mode=mode)
                finally:
                    _verify_lazy_cpu_provider_after_call(engine, mode)
        elif reader_spec.get("reader") == "tesseract_rus":
            output = run_tesseract_reader(tesseract, tessdata_dir, crop, mode=reader_spec.get("mode", ""))
        else:
            output = {"status": "skipped", "reason_code": "reader_unknown", "outputs": []}
        collection_name = "words" if reader_spec.get("reader") == "tesseract_rus" else "outputs"
        collection = output.get(collection_name)
        if isinstance(collection, list):
            for record in collection:
                if not isinstance(record, dict):
                    continue
                local_box = record.get("box_source")
                if isinstance(local_box, list) and len(local_box) == 4:
                    record["box_source"] = map_box_to_source(local_box, bounds, (1.0, 1.0))
                elif record.get("geometry_kind") == "entire_crop":
                    record["box_source"] = [float(value) for value in bounds]
        result["readers"].append({"reader": reader_spec.get("reader"), **output})
    return result
