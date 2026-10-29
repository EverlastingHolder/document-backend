"""Build a narrowly scoped Go sidecar from cached supported-name ROI artifacts."""

from __future__ import annotations

import hashlib
import json
import math
import re
from datetime import datetime
from pathlib import Path
from typing import Any


FIELD_EVIDENCE_SCHEMA = "passport-field-evidence/v1"
_SHA256 = re.compile(r"^[a-fA-F0-9]{64}$")
RECOGNIZER_FILENAME = "eslav_PP-OCRv5_rec_mobile.onnx"


class FieldEvidenceAdapterError(RuntimeError):
    """A safe adapter error containing only a reason code."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _bounds(line: dict[str, Any]) -> list[float] | None:
    quad = line.get("quad")
    if not isinstance(quad, list) or len(quad) != 4:
        return None
    try:
        points = [(float(point[0]), float(point[1])) for point in quad]
    except (TypeError, ValueError, IndexError):
        return None
    if not all(math.isfinite(value) for point in points for value in point):
        return None
    return [min(point[0] for point in points), min(point[1] for point in points), max(point[0] for point in points), max(point[1] for point in points)]


def _name_like(text: Any) -> bool:
    if not isinstance(text, str) or not text or re.search(r"\d", text):
        return False
    if not any(char.isalpha() for char in text):
        return False
    folded = text.upper().replace("Ё", "Е")
    return not any(label in folded for label in (
        "ФАМИЛИЯ", "ИМЯ", "ОТЧЕСТВО", "ПАСПОРТ", "РОССИЙСКОЙ", "ФЕДЕРАЦИИ",
        "КЕМВЫДАН", "ОРГАНВЫДАВШИЙ", "ДАТАВЫДАЧИ", "ДАТАРОЖДЕНИЯ", "МЕСТОРОЖДЕНИЯ",
    ))


def _date_text(text: Any) -> bool:
    if not isinstance(text, str):
        return False
    match = re.search(r"(?<!\d)(\d{2})[.\-/](\d{2})[.\-/](\d{4})(?!\d)", text)
    if match is None:
        return False
    day, month, year = match.groups()
    separator = match.group(0)[2]
    try:
        datetime.strptime(f"{day}{separator}{month}{separator}{year}", f"%d{separator}%m{separator}%Y")
        return True
    except ValueError:
        return False


def _mrz_like(line: dict[str, Any], box: list[float], document: list[float]) -> bool:
    text = str(line.get("text_raw", "")).strip().upper()
    if "<" in text:
        return True
    if (box[1] + box[3]) / 2 < document[1] + 0.84 * (document[3] - document[1]):
        return False
    return box[2] - box[0] > 0.5 * (document[2] - document[0]) and re.fullmatch(r"[A-Z0-9< ]{18,}", text) is not None


def _candidate_line_ids(field: Any) -> set[str]:
    ids: set[str] = set()
    if isinstance(field, dict):
        candidates = field.get("candidates")
        if isinstance(candidates, list):
            for candidate in candidates:
                if isinstance(candidate, dict) and isinstance(candidate.get("line_ids"), list):
                    ids.update(item for item in candidate["line_ids"] if isinstance(item, str))
        evidence = field.get("evidence")
        if isinstance(evidence, dict) and isinstance(evidence.get("line_ids"), list):
            ids.update(item for item in evidence["line_ids"] if isinstance(item, str))
    return ids


def _row_box(ids: list[str], lines: dict[str, dict[str, Any]], pass_id: str) -> tuple[list[str], list[float]]:
    selected = []
    for line_id in ids:
        line = lines.get(line_id)
        box = _bounds(line) if isinstance(line, dict) else None
        if line is None or box is None or line.get("pass_id") != pass_id or not _name_like(line.get("text_raw")):
            continue
        selected.append((line_id, box))
    if not selected:
        raise FieldEvidenceAdapterError("name_row_candidate_reference_unavailable")
    selected.sort(key=lambda item: (item[1][1], item[1][0], item[0]))
    return [item[0] for item in selected], [min(item[1][0] for item in selected), min(item[1][1] for item in selected), max(item[1][2] for item in selected), max(item[1][3] for item in selected)]


def build_sidecar(
    observations: dict[str, Any], extraction: dict[str, Any], plan: dict[str, Any],
    reader_results: list[dict[str, Any]], run_metadata: dict[str, Any],
) -> dict[str, Any]:
    """Use only the unique cached RapidOCR rec_only result; never reads gold."""
    image = observations.get("image")
    if not isinstance(image, dict):
        raise FieldEvidenceAdapterError("image_metadata_missing")
    image_sha = image.get("sha256")
    if not isinstance(image_sha, str) or not _SHA256.fullmatch(image_sha):
        raise FieldEvidenceAdapterError("image_sha_invalid")
    hypotheses = plan.get("hypotheses") if isinstance(plan, dict) else None
    if not isinstance(hypotheses, list):
        raise FieldEvidenceAdapterError("plan_invalid")
    matches = [item for item in hypotheses if isinstance(item, dict) and item.get("field_id") == "given_name"]
    if len(matches) != 1:
        raise FieldEvidenceAdapterError("supported_name_hypothesis_missing_or_ambiguous")
    hypothesis = matches[0]
    reader_plan = hypothesis.get("reader_plan")
    if (
        hypothesis.get("role_quality", {}).get("state") != "supported_row"
        or hypothesis.get("proposal_method") != "viz_layout_v1"
        or not isinstance(reader_plan, list)
        or not any(isinstance(item, dict) and item.get("reader") == "rapidocr" and item.get("mode") == "rec_only" for item in reader_plan)
    ):
        raise FieldEvidenceAdapterError("hypothesis_not_supported_name_rec_only")
    outputs = [item for item in reader_results if isinstance(item, dict) and item.get("hypothesis_id") == hypothesis.get("id") and item.get("field_id") == "given_name" and item.get("reader") == "rapidocr"]
    if len(outputs) != 1:
        raise FieldEvidenceAdapterError("rapidocr_result_missing_or_ambiguous")
    output = outputs[0]
    candidates = output.get("outputs")
    if output.get("status") != "ok" or output.get("candidate_count") != 1 or not isinstance(candidates, list) or len(candidates) != 1:
        raise FieldEvidenceAdapterError("rapidocr_rec_only_candidate_count_not_one")
    candidate = candidates[0]
    raw = candidate.get("text_raw")
    crop = hypothesis.get("crop_bounds")
    box = candidate.get("box_source")
    score = candidate.get("raw_confidence")
    if (
        candidate.get("geometry_kind") != "entire_crop"
        or not isinstance(raw, str) or not raw.strip() or "\n" in raw or "\r" in raw
        or not isinstance(crop, list) or len(crop) != 4
        or not isinstance(box, list) or len(box) != 4
        or any(not isinstance(n, (int, float)) or not math.isfinite(n) for n in crop + box)
        or any(abs(float(a) - float(b)) > 0.01 for a, b in zip(crop, box))
        or not isinstance(score, (int, float)) or not math.isfinite(score) or score < 0 or score > 1
    ):
        raise FieldEvidenceAdapterError("rapidocr_geometry_or_candidate_invalid")
    model_map = run_metadata.get("model_sha256") if isinstance(run_metadata, dict) else None
    model_sha = model_map.get(RECOGNIZER_FILENAME) if isinstance(model_map, dict) else None
    if not isinstance(model_sha, str) or not _SHA256.fullmatch(model_sha):
        raise FieldEvidenceAdapterError("reader_model_hash_invalid")

    source_pass_ids = hypothesis.get("source_pass_ids")
    middle_ids = hypothesis.get("source_line_ids")
    anchors = hypothesis.get("adjacent_anchors")
    if not isinstance(source_pass_ids, list) or len(source_pass_ids) != 1 or not isinstance(middle_ids, list) or not middle_ids or not isinstance(anchors, list) or len(anchors) != 1:
        raise FieldEvidenceAdapterError("supported_name_anchor_shape_invalid")
    source_pass_id = source_pass_ids[0]
    middle_anchor = anchors[0]
    if not isinstance(middle_anchor, dict) or middle_anchor.get("role") != "birth_date" or middle_anchor.get("relation") != "upper_row_boundary" or middle_anchor.get("pass_id") != source_pass_id:
        raise FieldEvidenceAdapterError("birth_date_anchor_invalid")
    line_records = observations.get("lines")
    pass_records = observations.get("passes")
    fields = extraction.get("fields") if isinstance(extraction, dict) else None
    if not isinstance(line_records, list) or not isinstance(pass_records, list) or not isinstance(fields, dict):
        raise FieldEvidenceAdapterError("baseline_observations_invalid")
    lines = {item.get("id"): item for item in line_records if isinstance(item, dict) and isinstance(item.get("id"), str)}
    passes = {item.get("id"): item for item in pass_records if isinstance(item, dict) and isinstance(item.get("id"), str)}
    pass_record = passes.get(source_pass_id)
    if pass_record is None or pass_record.get("status") != "ok" or pass_record.get("rotation_degrees") != 0 or pass_record.get("kind") not in {"original", "document", "upper_page", "lower_page"}:
        raise FieldEvidenceAdapterError("source_pass_invalid")
    if any(line_id not in lines or lines[line_id].get("pass_id") != source_pass_id for line_id in middle_ids):
        raise FieldEvidenceAdapterError("middle_name_line_reference_invalid")

    row_ids = []
    row_boxes = []
    for field_name, explicit_ids in (("surname", None), ("given_name", middle_ids), ("patronymic", None)):
        ids = explicit_ids if explicit_ids is not None else sorted(_candidate_line_ids(fields.get(field_name)) & set(lines))
        selected_ids, bounds = _row_box(ids, lines, source_pass_id)
        row_ids.append(selected_ids)
        row_boxes.append(bounds)
    if row_ids[1] != sorted(middle_ids, key=lambda line_id: (lines[line_id]["quad"][0][1], lines[line_id]["quad"][0][0], line_id)):
        # The source row in the plan is authoritative; retain its deterministic order.
        if set(row_ids[1]) != set(middle_ids):
            raise FieldEvidenceAdapterError("middle_row_plan_reference_mismatch")
    seam = observations.get("document", {}).get("seam_y")
    document_bounds = observations.get("document", {}).get("bounds")
    if not isinstance(seam, (int, float)) or not isinstance(document_bounds, list) or len(document_bounds) != 4:
        raise FieldEvidenceAdapterError("lower_page_boundary_unavailable")
    document_width = float(document_bounds[2]) - float(document_bounds[0])
    left_edges = [box[0] for box in row_boxes]
    if (
        not all((box[1] + box[3]) / 2 > seam and box[0] >= float(document_bounds[0]) + 0.42 * document_width for box in row_boxes)
        or not (row_boxes[0][1] + row_boxes[0][3]) / 2 < (row_boxes[1][1] + row_boxes[1][3]) / 2 < (row_boxes[2][1] + row_boxes[2][3]) / 2
        or max(left_edges) - min(left_edges) > 0.12 * document_width
    ):
        raise FieldEvidenceAdapterError("name_rows_not_lower_page_aligned")
    dob_line_id = middle_anchor.get("line_id")
    dob = lines.get(dob_line_id)
    dob_box = _bounds(dob) if isinstance(dob, dict) else None
    if dob is None or dob.get("pass_id") != source_pass_id or dob_box is None or not _date_text(dob.get("text_raw", "")) or row_boxes[1][3] >= dob_box[1] or float(crop[3]) > dob_box[1]:
        raise FieldEvidenceAdapterError("birth_date_boundary_invalid")

    rect = [float(value) for value in crop]
    for line in line_records:
        if not isinstance(line, dict):
            continue
        pass_meta = passes.get(line.get("pass_id"), {})
        box_line = _bounds(line)
        is_mrz = pass_meta.get("kind") == "mrz" or (_mrz_like(line, box_line, [float(v) for v in document_bounds]) if box_line is not None else False)
        if is_mrz and box_line is not None and rect[0] < box_line[2] and box_line[0] < rect[2] and rect[1] < box_line[3] and box_line[1] < rect[3]:
            raise FieldEvidenceAdapterError("roi_intersects_mrz")

    x0, y0, x1, y1 = rect
    return {
        "schema_version": FIELD_EVIDENCE_SCHEMA,
        "image": {
            "sha256": image_sha, "width": image.get("width"), "height": image.get("height"),
            "coordinate_frame": image.get("coordinate_frame"),
        },
        "evidence": [{
            "field_id": "given_name", "role_quality": "supported_row", "proposal_method": "viz_layout_v1",
            "source_pass_ids": [source_pass_id], "source_line_ids": list(middle_ids), "name_row_line_ids": row_ids,
            "adjacent_anchors": [{"role": "birth_date", "line_id": dob_line_id, "pass_id": source_pass_id, "relation": "upper_row_boundary"}],
            "roi_bounds": rect, "geometry_scope": "entire_crop", "reader": "rapidocr", "reader_mode": "rec_only",
            "reader_model_sha256": model_sha, "raw_text": raw, "score": float(score),
            "quad": [[x0, y0], [x1, y0], [x1, y1], [x0, y1]],
        }],
    }


def write_private_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    fd = path.open("w", encoding="utf-8")
    try:
        import os
        os.fchmod(fd.fileno(), 0o600)
        json.dump(payload, fd, ensure_ascii=False, indent=2)
        fd.write("\n")
    finally:
        fd.close()


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()
