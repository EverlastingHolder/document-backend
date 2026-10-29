"""Conservative document-region hypotheses for the local OCR experiment.

This module does not extract passport fields or alter OCR text. Every emitted
quad is mapped to the EXIF-normalized source image coordinate frame. Boundary
scores and geometry cutoffs below are heuristics for proposing crops, not
probabilities or claims about recognition quality.
"""

from __future__ import annotations

import hashlib
import importlib.metadata
import math
import re
from functools import lru_cache
from pathlib import Path
from typing import Any, Iterable

import cv2
import numpy as np
import onnxruntime as ort
from PIL import Image, ImageOps
from rapidocr import EngineType, LangDet, LangRec, ModelType, OCRVersion, RapidOCR

try:
    from .mrz_pipeline import find_tesseract, locate_mrz_roi, run_tesseract_crop
except ImportError:  # Direct execution and the existing test import convention.
    from mrz_pipeline import find_tesseract, locate_mrz_roi, run_tesseract_crop


SCHEMA_VERSION = "passport-ocr-observations/v1"
SCHEMA_VERSION_V2 = "passport-ocr-observations/v2"
_MAX_SIDE = 1200
_MIN_CONTOUR_AREA = 0.18
_MIN_EDGE_SUPPORT = 0.52
_MIN_CANDIDATE_SCORE = 0.58
_AMBIGUITY_GAP = 0.09
_EXACT_TEN_DIGIT_TOKEN = re.compile(r"\d(?:[\s-]*\d){9}")
_MIN_DET_LIMIT_SIDE_LEN = 256
_MAX_DET_LIMIT_SIDE_LEN = 1920


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _model_sha256(path: str | Path) -> str:
    model = Path(path)
    return _file_sha256(model) if model.is_file() else "unavailable"


def _validate_det_limit_side_len(value: int | None) -> int | None:
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError("det_limit_side_len_must_be_integer")
    if not _MIN_DET_LIMIT_SIDE_LEN <= value <= _MAX_DET_LIMIT_SIDE_LEN:
        raise ValueError("det_limit_side_len_out_of_bounds")
    return value


@lru_cache(maxsize=4)
def _create_cpu_engine_cached(det_model: str, rec_model: str, det_limit_side_len: int | None) -> Any:
    params = {
        "Det.engine_type": EngineType.ONNXRUNTIME,
        "Det.lang_type": LangDet.CH,
        "Det.model_type": ModelType.MOBILE,
        "Det.ocr_version": OCRVersion.PPOCRV5,
        "Det.model_path": det_model,
        "Rec.engine_type": EngineType.ONNXRUNTIME,
        "Rec.lang_type": LangRec.ESLAV,
        "Rec.model_type": ModelType.MOBILE,
        "Rec.ocr_version": OCRVersion.PPOCRV5,
        "Rec.model_path": rec_model,
        "Global.use_cls": False,
        "EngineConfig.onnxruntime.use_cuda": False,
        "EngineConfig.onnxruntime.use_dml": False,
        "EngineConfig.onnxruntime.use_cann": False,
        "EngineConfig.onnxruntime.use_coreml": False,
    }
    if det_limit_side_len is not None:
        params["Det.limit_side_len"] = det_limit_side_len
    return RapidOCR(params=params)


def create_cpu_engine(
    det_model: str | Path,
    rec_model: str | Path,
    det_limit_side_len: int | None = None,
) -> Any:
    """Return a cached pinned-model RapidOCR CPU engine for these paths/config."""
    det_limit_side_len = _validate_det_limit_side_len(det_limit_side_len)
    return _create_cpu_engine_cached(
        str(Path(det_model).resolve()), str(Path(rec_model).resolve()), det_limit_side_len
    )


def _engine_detector_config(engine: Any) -> dict[str, Any]:
    """Report actual resolved RapidOCR resize settings; unknown fake-engine values stay null."""
    config = getattr(engine, "cfg", None)
    detector = getattr(config, "Det", None)
    global_config = getattr(config, "Global", None)

    def read(source: Any, key: str) -> Any:
        if source is None:
            return None
        if isinstance(source, dict):
            return source.get(key)
        getter = getattr(source, "get", None)
        if callable(getter):
            try:
                return getter(key)
            except Exception:
                return None
        return getattr(source, key, None)

    return {
        "limit_side_len": read(detector, "limit_side_len"),
        "limit_type": read(detector, "limit_type"),
        "global_min_side_len": read(global_config, "min_side_len"),
        "global_max_side_len": read(global_config, "max_side_len"),
        "global_use_preprocess_img": read(global_config, "use_preprocess_img"),
        "global_use_vertical_padding": read(global_config, "use_vertical_padding"),
        "global_min_height": read(global_config, "min_height"),
        "global_width_height_ratio": read(global_config, "width_height_ratio"),
    }


def _sessions_with_providers(engine: Any) -> dict[str, list[str]]:
    """Inspect already-created nested ONNX sessions without forcing a model load."""
    found: dict[str, list[str]] = {}
    seen: set[int] = set()
    queue: list[tuple[str, Any, int]] = [("engine", engine, 0)]
    while queue:
        label, item, depth = queue.pop(0)
        if id(item) in seen or depth > 5:
            continue
        seen.add(id(item))
        getter = getattr(item, "get_providers", None)
        if callable(getter):
            try:
                found[label] = list(getter())
            except Exception:
                pass
            continue
        attrs = getattr(item, "__dict__", None)
        if not isinstance(attrs, dict):
            continue
        for key, child in attrs.items():
            if child is None or isinstance(child, (str, bytes, int, float, bool, np.ndarray)):
                continue
            if isinstance(child, (list, tuple)):
                for index, nested in enumerate(child[:8]):
                    if nested is not None and not isinstance(nested, (str, bytes, int, float, bool, np.ndarray)):
                        queue.append((f"{label}.{key}[{index}]", nested, depth + 1))
            else:
                queue.append((f"{label}.{key}", child, depth + 1))
    return found


def cpu_provider_report(engine: Any) -> dict[str, Any]:
    """Report actual nested session providers plus runtime availability."""
    # Match RapidOCR's current stage nesting used by the existing smoke worker:
    # engine.text_det.session.session and engine.text_rec.session.session.
    actual: dict[str, list[str] | None] = {}
    for role, stage_name in (("det", "text_det"), ("rec", "text_rec")):
        stage = getattr(engine, stage_name, None)
        inference_session = getattr(getattr(stage, "session", None), "session", None)
        getter = getattr(inference_session, "get_providers", None)
        try:
            actual[role] = list(getter()) if callable(getter) else None
        except Exception:
            actual[role] = None
    # Keep a bounded fallback for a future wrapper layout; do not mistake the
    # runtime's available providers for providers actually used by the models.
    if any(value is None for value in actual.values()):
        actual["nested_sessions"] = _sessions_with_providers(engine)
    return {
        "sessions": actual,
        "available": list(ort.get_available_providers()),
    }


class CPUProviderValidationError(RuntimeError):
    """Raised when actual OCR sessions cannot be verified as CPU-only."""

    def __init__(self) -> None:
        # Deliberately contains no engine details, model paths, image text or PII.
        super().__init__("OCR provider validation failed: expected CPUExecutionProvider for detector and recognizer")


def _assert_cpu_only(engine: Any) -> dict[str, Any]:
    report = cpu_provider_report(engine)
    sessions = report["sessions"]
    if any(sessions.get(role) != ["CPUExecutionProvider"] for role in ("det", "rec")):
        raise CPUProviderValidationError()
    return report


def _quad_area(points: np.ndarray) -> float:
    return float(abs(cv2.contourArea(points.astype(np.float32))))


def _side_edge_support(points: np.ndarray, edge_map: np.ndarray) -> list[float]:
    h, w = edge_map.shape[:2]
    supports: list[float] = []
    for start, end in zip(points, np.roll(points, -1, axis=0)):
        length = float(np.linalg.norm(end - start))
        count = max(8, int(length / 4))
        xs = np.rint(np.linspace(start[0], end[0], count)).astype(int)
        ys = np.rint(np.linspace(start[1], end[1], count)).astype(int)
        valid = (xs >= 0) & (xs < w) & (ys >= 0) & (ys < h)
        xs, ys = xs[valid], ys[valid]
        if not len(xs):
            supports.append(0.0)
            continue
        local = np.zeros(len(xs), dtype=np.uint8)
        for dx, dy in ((0, 0), (1, 0), (-1, 0), (0, 1), (0, -1)):
            nx = np.clip(xs + dx, 0, w - 1)
            ny = np.clip(ys + dy, 0, h - 1)
            local |= edge_map[ny, nx]
        supports.append(float(np.count_nonzero(local) / len(local)))
    return supports


def detect_document_bounds(image: np.ndarray) -> tuple[tuple[int, int, int, int] | None, float]:
    """Find a conservative axis-aligned document candidate on an RGB image."""
    height, width = image.shape[:2]
    if height < 16 or width < 16:
        return None, 0.0
    scale = min(1.0, _MAX_SIDE / max(height, width))
    small = cv2.resize(image, None, fx=scale, fy=scale, interpolation=cv2.INTER_AREA) if scale < 1 else image
    gray = cv2.cvtColor(small, cv2.COLOR_RGB2GRAY)
    gray = cv2.GaussianBlur(gray, (3, 3), 0)
    edges = cv2.Canny(gray, 35, 120)
    support_edges = cv2.dilate(edges, cv2.getStructuringElement(cv2.MORPH_RECT, (7, 7)))
    kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (5, 5))
    closed = cv2.morphologyEx(edges, cv2.MORPH_CLOSE, kernel, iterations=2)
    # External contours suppress page-internal stamps, portraits and printed
    # boxes from competing with the outer document boundary.
    contours, _ = cv2.findContours(closed, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    img_area = float(gray.shape[0] * gray.shape[1])
    candidates: list[tuple[float, tuple[int, int, int, int], np.ndarray]] = []
    for contour in contours:
        perimeter = cv2.arcLength(contour, True)
        if perimeter <= 0:
            continue
        approx = cv2.approxPolyDP(contour, 0.025 * perimeter, True).reshape(-1, 2)
        if len(approx) != 4 or not cv2.isContourConvex(approx.reshape(-1, 1, 2)):
            continue
        area = _quad_area(approx)
        area_fraction = area / img_area
        if not (_MIN_CONTOUR_AREA <= area_fraction <= 0.985):
            continue
        x, y, w, h = cv2.boundingRect(approx.reshape(-1, 1, 2))
        if w < 0.24 * gray.shape[1] or h < 0.24 * gray.shape[0]:
            continue
        aspect = w / max(1, h)
        if not (0.30 <= aspect <= 3.2):
            continue
        support = _side_edge_support(approx.astype(np.float32), support_edges)
        mean_support = float(np.mean(support))
        weakest_side = float(min(support))
        if weakest_side < _MIN_EDGE_SUPPORT:
            continue
        # Area is a weak prior; closure and edge continuity dominate.
        score = 0.70 * mean_support + 0.20 * weakest_side + 0.10 * min(1.0, area_fraction / 0.5)
        # Reject contours touching the frame: they may be the canvas boundary.
        if x <= 1 or y <= 1 or x + w >= gray.shape[1] - 1 or y + h >= gray.shape[0] - 1:
            score -= 0.12
        sx0, sy0 = int(round(x / scale)), int(round(y / scale))
        sx1, sy1 = int(round((x + w) / scale)), int(round((y + h) / scale))
        candidates.append((score, (sx0, sy0, sx1, sy1), approx))
    candidates.sort(key=lambda candidate: candidate[0], reverse=True)
    distinct: list[tuple[float, tuple[int, int, int, int], np.ndarray]] = []
    for candidate in candidates:
        box = candidate[1]
        if all(_box_iou(box, existing[1]) < 0.88 for existing in distinct):
            distinct.append(candidate)
    if not distinct or distinct[0][0] < _MIN_CANDIDATE_SCORE:
        return None, distinct[0][0] if distinct else 0.0
    if len(distinct) > 1 and distinct[0][0] - distinct[1][0] < _AMBIGUITY_GAP:
        return None, distinct[0][0]
    bounds = _pad_and_clamp(distinct[0][1], width, height, fraction=0.025, minimum=8)
    return bounds, float(distinct[0][0])


def detect_ocr_bbox_envelope(
    lines: list[dict[str, Any]], width: int, height: int
) -> tuple[int, int, int, int] | None:
    """Propose a broad secondary ROI from dispersed original-pass OCR geometry.

    This is not a physical page boundary. It is gated on both a large envelope
    and broad 4x4 spatial distribution so a compact portrait, stamp, or text
    cluster cannot become the fallback crop. The original pass is always kept.
    """
    if width <= 0 or height <= 0 or len(lines) < 8:
        return None
    quads: list[np.ndarray] = []
    centers: list[tuple[float, float]] = []
    for line in lines:
        quad = _quad_to_array(line.get("quad"))
        if quad is None:
            continue
        quad[:, 0] = np.clip(quad[:, 0], 0, width)
        quad[:, 1] = np.clip(quad[:, 1], 0, height)
        quads.append(quad)
        centers.append((float(np.mean(quad[:, 0])) / width, float(np.mean(quad[:, 1])) / height))
    if len(quads) < 8:
        return None

    points = np.vstack(quads)
    x0, y0 = float(points[:, 0].min()), float(points[:, 1].min())
    x1, y1 = float(points[:, 0].max()), float(points[:, 1].max())
    span_x, span_y = (x1 - x0) / width, (y1 - y0) / height
    envelope_area = span_x * span_y
    if span_x < 0.35 or span_y < 0.35 or envelope_area < 0.18:
        return None

    cells = set()
    x_bins = set()
    y_bins = set()
    for center_x, center_y in centers:
        bin_x = min(3, max(0, int(center_x * 4)))
        bin_y = min(3, max(0, int(center_y * 4)))
        x_bins.add(bin_x)
        y_bins.add(bin_y)
        cells.add((bin_x, bin_y))
    if len(x_bins) < 3 or len(y_bins) < 3 or len(cells) < 4:
        return None

    envelope = (int(math.floor(x0)), int(math.floor(y0)), int(math.ceil(x1)), int(math.ceil(y1)))
    return _pad_and_clamp(envelope, width, height, fraction=0.12, minimum=8)


def _box_iou(a: tuple[int, int, int, int], b: tuple[int, int, int, int]) -> float:
    x0, y0 = max(a[0], b[0]), max(a[1], b[1])
    x1, y1 = min(a[2], b[2]), min(a[3], b[3])
    intersection = max(0, x1 - x0) * max(0, y1 - y0)
    area_a = max(0, a[2] - a[0]) * max(0, a[3] - a[1])
    area_b = max(0, b[2] - b[0]) * max(0, b[3] - b[1])
    union = area_a + area_b - intersection
    return intersection / union if union else 0.0


def _pad_and_clamp(
    box: tuple[int, int, int, int], width: int, height: int, *, fraction: float, minimum: int
) -> tuple[int, int, int, int]:
    x0, y0, x1, y1 = box
    padx = max(minimum, int(round((x1 - x0) * fraction)))
    pady = max(minimum, int(round((y1 - y0) * fraction)))
    return max(0, x0 - padx), max(0, y0 - pady), min(width, x1 + padx), min(height, y1 + pady)


def detect_horizontal_seam(image: np.ndarray, bounds: tuple[int, int, int, int]) -> int | None:
    """Return a strong internal horizontal seam y in original-image pixels."""
    x0, y0, x1, y1 = bounds
    crop = image[y0:y1, x0:x1]
    height, width = crop.shape[:2]
    if height < 80 or width < 80:
        return None
    gray = cv2.cvtColor(crop, cv2.COLOR_RGB2GRAY)
    edges = cv2.Canny(cv2.GaussianBlur(gray, (3, 3), 0), 30, 110)
    first = max(1, int(height * 0.20))
    last = min(height - 1, int(height * 0.80))
    radius = max(1, int(round(height * 0.004)))
    scores = []
    for y in range(first, last):
        band = edges[max(0, y - radius):min(height, y + radius + 1), :]
        coverage = np.count_nonzero(np.any(band > 0, axis=0)) / width
        scores.append((float(coverage), y))
    if not scores:
        return None
    score, local_y = max(scores)
    if score < 0.58:
        return None
    return y0 + local_y


def page_crops(
    bounds: tuple[int, int, int, int], seam_y: int | None, width: int, height: int
) -> list[tuple[int, int, int, int]]:
    """Create two overlapping page hypotheses only when a seam was detected."""
    if seam_y is None:
        return []
    x0, y0, x1, y1 = bounds
    doc_h = y1 - y0
    overlap = max(8, int(round(doc_h * 0.02)))
    split = min(max(seam_y, y0 + int(doc_h * 0.20)), y1 - int(doc_h * 0.20))
    upper = (max(0, x0), max(0, y0), min(width, x1), min(height, min(y1, split + overlap)))
    lower = (max(0, x0), max(0, max(y0, split - overlap)), min(width, x1), min(height, y1))
    return [upper, lower]


def _quad_to_array(box: Any) -> np.ndarray | None:
    try:
        quad = np.asarray(box, dtype=np.float64).reshape(4, 2)
    except (TypeError, ValueError):
        return None
    return quad if np.isfinite(quad).all() else None


def _transform_quad(quad: np.ndarray, matrix: np.ndarray) -> list[list[float]]:
    homogeneous = np.column_stack((quad, np.ones(4, dtype=np.float64)))
    mapped = homogeneous @ matrix.T
    return [[round(float(x), 3), round(float(y), 3)] for x, y in mapped[:, :2]]


def _crop_matrix(crop: tuple[int, int, int, int], rotation_degrees: int) -> np.ndarray:
    x0, y0, x1, y1 = crop
    w, h = x1 - x0, y1 - y0
    if rotation_degrees == 90:  # cv2.ROTATE_90_CLOCKWISE; integer pixel centers
        return np.array([[0, 1, x0], [-1, 0, y0 + h - 1], [0, 0, 1]], dtype=np.float64)
    if rotation_degrees == 270:  # cv2.ROTATE_90_COUNTERCLOCKWISE
        return np.array([[0, -1, x0 + w - 1], [1, 0, y0], [0, 0, 1]], dtype=np.float64)
    return np.array([[1, 0, x0], [0, 1, y0], [0, 0, 1]], dtype=np.float64)


def map_quad_to_source(
    quad: Any, crop: tuple[int, int, int, int], rotation_degrees: int = 0
) -> list[list[float]] | None:
    """Map an OCR quadrilateral from pass pixels to EXIF-normalized source pixels."""
    array = _quad_to_array(quad)
    if array is None:
        return None
    return _transform_quad(array, _crop_matrix(crop, rotation_degrees))


def _line_records(result: Any, pass_id: str, crop: tuple[int, int, int, int], rotation: int) -> list[dict[str, Any]]:
    texts = getattr(result, "txts", None)
    if texts is None:
        texts = []
    boxes = getattr(result, "boxes", None)
    scores = getattr(result, "scores", None)
    records = []
    for index, text in enumerate(texts):
        if boxes is None or index >= len(boxes):
            continue
        mapped = map_quad_to_source(boxes[index], crop, rotation)
        if mapped is None:
            continue
        try:
            score = float(scores[index]) if scores is not None and index < len(scores) else float("nan")
        except (TypeError, ValueError):
            continue
        if not math.isfinite(score) or not all(math.isfinite(value) for point in mapped for value in point):
            continue
        records.append({"id": f"{pass_id}-line-{index + 1}", "pass_id": pass_id, "text_raw": str(text), "score": score, "quad": mapped})
    return records


def _has_vertical_right_margin_serial_candidate(
    lines: Iterable[dict[str, Any]], bounds: tuple[int, int, int, int]
) -> bool:
    """Return whether original OCR already found a plausible vertical serial.

    Only an exact ten-digit token in the right margin can suppress rotated
    margin passes. A substring in a long MRZ-like row is not a serial
    candidate, even when it contains ten consecutive digits.
    """
    x0, y0, x1, y1 = bounds
    doc_width, doc_height = x1 - x0, y1 - y0
    if doc_width <= 0 or doc_height <= 0:
        return False

    for line in lines:
        text = str(line.get("text_raw", "")).strip()
        if _EXACT_TEN_DIGIT_TOKEN.fullmatch(text) is None:
            continue
        quad = _quad_to_array(line.get("quad"))
        if quad is None:
            continue
        bbox_width = float(np.max(quad[:, 0]) - np.min(quad[:, 0]))
        center_y = float(np.mean(quad[:, 1]))
        bottom_wide_mrz = center_y >= y0 + 0.85 * doc_height and bbox_width > 0.5 * doc_width
        if bottom_wide_mrz:
            continue

        edges = np.roll(quad, -1, axis=0) - quad
        lengths = np.linalg.norm(edges, axis=1)
        long_index = int(np.argmax(lengths))
        long_edge = edges[long_index]
        angle = abs(math.degrees(math.atan2(long_edge[1], long_edge[0]))) % 180
        short_length = max(1.0, float(np.min(lengths)))
        if float(np.max(lengths)) / short_length < 1.5 or not 70 <= angle <= 110:
            continue
        if float(np.max(quad[:, 0])) < x0 + 0.82 * doc_width:
            continue
        return True
    return False


def _is_vertical_quad(quad: Any) -> bool:
    points = _quad_to_array(quad)
    if points is None:
        return False
    edges = np.roll(points, -1, axis=0) - points
    lengths = np.linalg.norm(edges, axis=1)
    long_index = int(np.argmax(lengths))
    long = edges[long_index]
    angle = abs(math.degrees(math.atan2(long[1], long[0]))) % 180
    long_length = float(lengths[long_index])
    short_length = max(1.0, float(min(lengths)))
    return long_length / short_length >= 2.0 and 70 <= angle <= 110


def _right_margin_crop(
    bounds: tuple[int, int, int, int], lines: list[dict[str, Any]], width: int, height: int
) -> tuple[int, int, int, int]:
    x0, y0, x1, y1 = bounds
    candidates = []
    for line in lines:
        quad = np.asarray(line["quad"], dtype=np.float64)
        if not _is_vertical_quad(quad):
            continue
        if float(np.max(quad[:, 0])) < x0 + 0.82 * (x1 - x0):
            continue
        candidates.append(quad)
    if candidates:
        points = np.vstack(candidates)
        union = (
            int(np.floor(points[:, 0].min())),
            int(np.floor(points[:, 1].min())),
            int(np.ceil(points[:, 0].max())),
            int(np.ceil(points[:, 1].max())),
        )
        return _pad_and_clamp(union, width, height, fraction=0.15, minimum=8)
    margin_width = max(8, int(round((x1 - x0) * 0.12)))
    return max(0, x1 - margin_width), y0, min(width, x1), min(height, y1)


def _run_pass(
    engine: Any,
    rgb: np.ndarray,
    pass_id: str,
    kind: str,
    crop: tuple[int, int, int, int],
    rotation: int,
    *,
    reader: str | None = None,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    x0, y0, x1, y1 = crop
    image = rgb[y0:y1, x0:x1]
    if rotation == 90:
        image = cv2.rotate(image, cv2.ROTATE_90_CLOCKWISE)
    elif rotation == 270:
        image = cv2.rotate(image, cv2.ROTATE_90_COUNTERCLOCKWISE)
    if image.size == 0:
        record = {"id": pass_id, "kind": kind, "crop": list(crop), "rotation_degrees": rotation,
                  "transform_to_image": _crop_matrix(crop, rotation).tolist(), "elapsed_seconds": 0.0, "status": "error"}
        if reader is not None:
            record["reader"] = reader
            record["issue_code"] = "empty_crop"
        return record, []
    bgr = cv2.cvtColor(image, cv2.COLOR_RGB2BGR)
    import time
    started = time.perf_counter()
    try:
        # RapidOCR retains per-call switches. A preceding rec-only field call
        # must not silently disable detection on the next page/image.
        result = engine(bgr, use_det=True, use_cls=False, use_rec=True)
        elapsed = time.perf_counter() - started
        records = _line_records(result, pass_id, crop, rotation)
        status = "ok"
    except Exception:
        elapsed = time.perf_counter() - started
        records = []
        status = "error"
    record = {
        "id": pass_id,
        "kind": kind,
        "crop": list(crop),
        "rotation_degrees": rotation,
        "transform_to_image": _crop_matrix(crop, rotation).tolist(),
        "elapsed_seconds": round(elapsed, 6),
        "status": status,
    }
    if reader is not None:
        record["reader"] = reader
        if status == "error":
            record["issue_code"] = "rapidocr_error"
    return record, records


def build_observations(
    image_path: str | Path,
    image_id: str,
    det_model: str | Path,
    rec_model: str | Path,
    *,
    engine: Any = None,
    auxiliary_engine: Any = None,
    enhanced: bool = True,
    include_mrz: bool = False,
    tesseract_path: str | Path | None = None,
    det_limit_side_len: int | None = None,
) -> dict[str, Any]:
    """Run original plus bounded geometry-driven OCR passes.

    This function retains raw strings privately. Callers must keep its result in
    private scratch storage; it contains OCR text from the supplied image.
    """
    path = Path(image_path)
    det_limit_side_len = _validate_det_limit_side_len(det_limit_side_len)
    raw_bytes = path.read_bytes()
    with Image.open(path) as opened:
        normalized = ImageOps.exif_transpose(opened).convert("RGB")
        rgb = np.asarray(normalized)
    height, width = rgb.shape[:2]
    if engine is None:
        engine = create_cpu_engine(det_model, rec_model, det_limit_side_len=det_limit_side_len)
    elif det_limit_side_len is not None:
        engine_config = _engine_detector_config(engine)
        if engine_config["limit_side_len"] != det_limit_side_len or engine_config["limit_type"] != "min":
            raise ValueError("provided_engine_detector_config_mismatch")
    full = (0, 0, width, height)
    auxiliary = auxiliary_engine if auxiliary_engine is not None else engine
    used_auxiliary = False
    pass_profiles: dict[str, str] = {}

    def run_pass(
        pass_id: str,
        kind: str,
        crop: tuple[int, int, int, int],
        rotation: int,
        *,
        reader: str | None = None,
        use_auxiliary: bool = False,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        nonlocal used_auxiliary
        selected = auxiliary if use_auxiliary else engine
        record, found_lines = _run_pass(selected, rgb, pass_id, kind, crop, rotation, reader=reader)
        if use_auxiliary and auxiliary is not engine:
            used_auxiliary = True
            _assert_cpu_only(auxiliary)
        pass_profiles[pass_id] = "auxiliary" if use_auxiliary and auxiliary is not engine else "primary"
        return record, found_lines

    passes: list[dict[str, Any]] = []
    lines: list[dict[str, Any]] = []
    original_pass, original_lines = run_pass("original", "original", full, 0)
    passes.append(original_pass)
    lines.extend(original_lines)
    # RapidOCR 3.10 loads models lazily; validate actual nested ONNX sessions
    # immediately after the first call, not just explicit engine flags or the
    # runtime's list of providers available on this machine.
    providers = _assert_cpu_only(engine)

    document_bounds: tuple[int, int, int, int] | None = None
    seam: int | None = None
    document_method = "full_fallback"
    document_quality = "fallback"
    pages: list[tuple[int, int, int, int]] = []
    if enhanced:
        document_bounds, quality_score = detect_document_bounds(rgb)
        if document_bounds is None:
            document_bounds = detect_ocr_bbox_envelope(original_lines, width, height)
            if document_bounds is not None:
                document_method = "ocr_bbox_envelope"
                document_quality = "candidate"
        if document_bounds is not None:
            if document_method != "ocr_bbox_envelope":
                document_method = "contour"
                document_quality = "candidate"
            seam = detect_horizontal_seam(rgb, document_bounds)
            pages = page_crops(document_bounds, seam, width, height)
            if pages:
                for index, page in enumerate(pages, 1):
                    name = "upper_page" if index == 1 else "lower_page"
                    record, page_lines = run_pass(name, name, page, 0)
                    passes.append(record)
                    lines.extend(page_lines)
            elif document_bounds != full:
                record, document_lines = run_pass("document", "document", document_bounds, 0)
                passes.append(record)
                lines.extend(document_lines)

            if not _has_vertical_right_margin_serial_candidate(original_lines, document_bounds):
                margin = _right_margin_crop(document_bounds, original_lines, width, height)
                for rotation, kind in ((90, "right_margin_cw"), (270, "right_margin_ccw")):
                    pass_id = kind
                    record, margin_lines = run_pass(pass_id, kind, margin, rotation, use_auxiliary=True)
                    passes.append(record)
                    lines.extend(margin_lines)
        else:
            # Full image was already OCR'd. Document uncertainty never discards it.
            document_quality = "fallback"

    if include_mrz:
        effective_bounds = document_bounds or full
        lower_page = pages[1] if len(pages) > 1 else None
        mrz_crop = locate_mrz_roi(
            image_size=(width, height),
            document_bounds=effective_bounds,
            lines=original_lines,
            document_method=document_method,
            lower_page=lower_page,
        )
        rapid_record, rapid_lines = run_pass(
            "mrz_rapid", "mrz", mrz_crop, 0, reader="rapidocr", use_auxiliary=True
        )
        passes.append(rapid_record)
        lines.extend(rapid_lines)

        tesseract = find_tesseract(tesseract_path)
        if tesseract is None:
            passes.append({
                "id": "mrz_tesseract", "kind": "mrz", "crop": list(mrz_crop),
                "rotation_degrees": 0, "transform_to_image": _crop_matrix(mrz_crop, 0).tolist(),
                "elapsed_seconds": 0.0, "status": "error", "reader": "tesseract-eng",
                "issue_code": "tesseract_unavailable",
            })
        else:
            x0, y0, x1, y1 = mrz_crop
            crop_rgb = rgb[y0:y1, x0:x1]
            try:
                tess_rows, issue_code, elapsed = run_tesseract_crop(tesseract, crop_rgb)
            except Exception:
                tess_rows, issue_code, elapsed = [], "tesseract_failed", 0.0
            tess_record = {
                "id": "mrz_tesseract", "kind": "mrz", "crop": list(mrz_crop),
                "rotation_degrees": 0, "transform_to_image": _crop_matrix(mrz_crop, 0).tolist(),
                "elapsed_seconds": round(elapsed, 6),
                "status": "error" if issue_code else "ok", "reader": "tesseract-eng",
            }
            if issue_code:
                tess_record["issue_code"] = issue_code
            else:
                for index, row in enumerate(tess_rows, 1):
                    mapped = map_quad_to_source(row["quad"], mrz_crop, 0)
                    if mapped is None:
                        continue
                    lines.append({
                        "id": f"mrz_tesseract-line-{index}", "pass_id": "mrz_tesseract",
                        "text_raw": row["text_raw"], "score": row["score"], "quad": mapped,
                    })
            passes.append(tess_record)

    runtime = {
        "rapidocr_version": _distribution_version("rapidocr"),
        "onnxruntime_version": _distribution_version("onnxruntime"),
        "opencv_version": cv2.__version__,
        "numpy_version": np.__version__,
        "providers": providers,
        "auxiliary_providers": _assert_cpu_only(auxiliary) if used_auxiliary and auxiliary is not engine else None,
        "model_sha256": {"detector": _model_sha256(det_model), "recognizer": _model_sha256(rec_model)},
        "detector_profiles": {
            "primary": _engine_detector_config(engine),
            "auxiliary": _engine_detector_config(auxiliary) if auxiliary is not engine else None,
        },
        "pass_detector_profiles": pass_profiles,
    }
    return {
        "schema_version": SCHEMA_VERSION_V2 if include_mrz else SCHEMA_VERSION,
        "image": {
            "id": image_id,
            "width": width,
            "height": height,
            "sha256": hashlib.sha256(raw_bytes).hexdigest(),
            "coordinate_frame": "exif_normalized_pixels",
        },
        "document": {
            "bounds": list(document_bounds) if document_bounds else [0, 0, width, height],
            "method": document_method,
            "quality": document_quality,
            "seam_y": float(seam) if seam is not None else None,
        },
        "passes": passes,
        "lines": lines,
        "runtime": runtime,
    }


def _distribution_version(name: str) -> str:
    try:
        return importlib.metadata.version(name)
    except importlib.metadata.PackageNotFoundError:
        return "unknown"
