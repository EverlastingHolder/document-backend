"""Conservative, OCR-independent passport-spread geometry proposals.

This module proposes physical quadrilaterals and page transforms only. A
successful result is provisional geometric evidence, not a document or layout
classification. It deliberately never fits a rectangle to OCR-envelope bounds.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
import math
from typing import Any, Iterable

import cv2
import numpy as np


_MAX_SIDE = 1200
_MIN_AREA_FRACTION = 0.16
_MAX_AREA_FRACTION = 0.94
_MIN_SIDE_MARGIN = 0.004
_MIN_SIDE_SUPPORT = 0.36
_MIN_MEAN_SUPPORT = 0.54
_MIN_TEXT_LINES = 4
_MIN_TEXT_CELLS = 3
_MIN_LINE_COVERAGE = 0.42
_AMBIGUITY_MARGIN = 0.075
_NESTED_AMBIGUITY_IOU = 0.72
_PAGE_WIDTH = 1200
_MAPPING_TOLERANCE_PX = 1.0
_MAX_PAGE_TILT_DEGREES = 25.0
_MAX_SPREAD_ASPECT = 1.05


@dataclass(frozen=True)
class RegisteredPage:
    """A normalized page image coordinate frame and its original mapping."""

    name: str
    source_quad: list[list[float]]
    source_size: tuple[int, int]
    output_size: tuple[int, int]
    original_to_page: list[list[float]]
    page_to_original: list[list[float]]


@dataclass(frozen=True)
class RegistrationResult:
    status: str
    reason: str
    candidate_count: int
    orientation_status: str = "orientation_unverified"
    source_quad: list[list[float]] | None = None
    candidate_quads: tuple[list[list[float]], ...] = ()
    area_fraction: float | None = None
    edge_support: tuple[float, float, float, float] | None = None
    contrast_support: tuple[float, float, float, float] | None = None
    texture_asymmetry: tuple[float, float, float, float] | None = None
    candidate_score: float | None = None
    seam_ratio: float | None = None
    seam_support: float | None = None
    pages: tuple[RegisteredPage, ...] = ()
    input_size: tuple[int, int] | None = None
    stage_counts: dict[str, int] | None = None
    selected_candidate_source: str | None = None

    def summary(self) -> dict[str, Any]:
        """Return numeric, non-sensitive registration diagnostics."""
        return asdict(self)


@dataclass(frozen=True)
class _Candidate:
    quad: np.ndarray
    area_fraction: float
    side_support: tuple[float, float, float, float]
    score: float
    method_count: int
    text_line_count: int
    text_cell_count: int
    source: str = "contour"
    line_coverage: tuple[float, float, float, float] | None = None
    contrast_support: tuple[float, float, float, float] | None = None
    texture_asymmetry: tuple[float, float, float, float] | None = None


def _order_quad(points: np.ndarray) -> np.ndarray | None:
    """Order four contour vertices as top-left, top-right, bottom-right, bottom-left."""
    points = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    if points.shape != (4, 2) or not np.isfinite(points).all():
        return None
    center = points.mean(axis=0)
    angles = np.arctan2(points[:, 1] - center[1], points[:, 0] - center[0])
    ordered = points[np.argsort(angles)]
    start = int(np.argmin(ordered[:, 0] + ordered[:, 1]))
    ordered = np.roll(ordered, -start, axis=0)
    # Image coordinates increase downwards; TL->TR->BR->BL has positive area.
    signed = float(np.sum(ordered[:, 0] * np.roll(ordered[:, 1], -1) - ordered[:, 1] * np.roll(ordered[:, 0], -1)))
    if signed < 0:
        ordered = ordered[[0, 3, 2, 1]]
    return ordered


def _polygon_area(quad: np.ndarray) -> float:
    return float(abs(cv2.contourArea(np.asarray(quad, dtype=np.float32))))


def _source_to_working_points(points: np.ndarray, scale_x: float, scale_y: float) -> np.ndarray:
    """Map source pixel centers using OpenCV's resize half-pixel convention."""
    mapped = np.asarray(points, dtype=np.float64).copy().reshape(-1, 2)
    mapped[:, 0] = (mapped[:, 0] + 0.5) * scale_x - 0.5
    mapped[:, 1] = (mapped[:, 1] + 0.5) * scale_y - 0.5
    return mapped


def _working_to_source_points(points: np.ndarray, scale_x: float, scale_y: float) -> np.ndarray:
    """Invert :func:`_source_to_working_points` with actual rounded resize scales."""
    mapped = np.asarray(points, dtype=np.float64).copy().reshape(-1, 2)
    mapped[:, 0] = (mapped[:, 0] + 0.5) / scale_x - 0.5
    mapped[:, 1] = (mapped[:, 1] + 0.5) / scale_y - 0.5
    return mapped


def _side_support(quad: np.ndarray, edge_map: np.ndarray) -> tuple[float, float, float, float]:
    height, width = edge_map.shape[:2]
    search_radius = max(2, min(10, int(round(min(width, height) * 0.006))))
    values: list[float] = []
    for start, end in zip(quad, np.roll(quad, -1, axis=0)):
        length = float(np.linalg.norm(end - start))
        count = max(12, min(1500, int(length / 2)))
        xs = np.rint(np.linspace(start[0], end[0], count)).astype(int)
        ys = np.rint(np.linspace(start[1], end[1], count)).astype(int)
        valid = (xs >= 0) & (xs < width) & (ys >= 0) & (ys < height)
        xs, ys = xs[valid], ys[valid]
        if not len(xs):
            values.append(0.0)
            continue
        tangent = (end - start) / max(1.0, length)
        normal = np.array([-tangent[1], tangent[0]], dtype=np.float64)
        nearby = np.zeros(len(xs), dtype=np.uint8)
        for offset in range(-search_radius, search_radius + 1):
            nx = np.rint(xs + normal[0] * offset).astype(int)
            ny = np.rint(ys + normal[1] * offset).astype(int)
            valid = (nx >= 0) & (nx < width) & (ny >= 0) & (ny < height)
            nearby[valid] |= edge_map[ny[valid], nx[valid]]
        values.append(float(np.count_nonzero(nearby) / len(nearby)))
    return tuple(values)  # type: ignore[return-value]


def _side_boundary_profile(
    quad: np.ndarray, image_rgb: np.ndarray
) -> tuple[tuple[float, float, float, float], tuple[float, float, float, float]]:
    """Compare paired interior/exterior strips for color and texture changes."""
    height, width = image_rgb.shape[:2]
    radius = max(3, min(10, int(round(min(width, height) * 0.008))))
    gray = cv2.cvtColor(image_rgb, cv2.COLOR_RGB2GRAY).astype(np.float32)
    gradient_x = cv2.Sobel(gray, cv2.CV_32F, 1, 0, ksize=3)
    gradient_y = cv2.Sobel(gray, cv2.CV_32F, 0, 1, ksize=3)
    gradient = cv2.magnitude(gradient_x, gradient_y)
    contrast_values: list[float] = []
    texture_values: list[float] = []
    for start, end in zip(quad, np.roll(quad, -1, axis=0)):
        length = float(np.linalg.norm(end - start))
        count = max(12, min(600, int(length / 3)))
        points = start[None, :] + (end - start)[None, :] * np.linspace(0.06, 0.94, count)[:, None]
        tangent = (end - start) / max(1.0, length)
        inward = np.array([-tangent[1], tangent[0]], dtype=np.float64)
        inside: list[np.ndarray] = []
        outside: list[np.ndarray] = []
        inside_texture: list[np.ndarray] = []
        outside_texture: list[np.ndarray] = []
        for offset in range(3, radius + 1):
            for sign, color_target, texture_target in (
                (1.0, inside, inside_texture), (-1.0, outside, outside_texture),
            ):
                coordinates = np.rint(points + sign * inward[None, :] * offset).astype(int)
                coordinates[:, 0] = np.clip(coordinates[:, 0], 0, width - 1)
                coordinates[:, 1] = np.clip(coordinates[:, 1], 0, height - 1)
                color_target.append(image_rgb[coordinates[:, 1], coordinates[:, 0]].astype(np.float32))
                texture_target.append(gradient[coordinates[:, 1], coordinates[:, 0]])
        contrast = np.mean(np.abs(np.mean(inside, axis=0) - np.mean(outside, axis=0)), axis=1)
        texture_delta = np.mean(inside_texture, axis=0) - np.mean(outside_texture, axis=0)
        contrast_values.append(float(np.mean(contrast >= 8.0)))
        texture_values.append(float(np.mean(texture_delta >= 1.5)))
    return tuple(contrast_values), tuple(texture_values)  # type: ignore[return-value]


def _text_geometry(quad: np.ndarray, text_quads: Iterable[Any]) -> tuple[int, int]:
    """Count OCR boxes inside a candidate; text content is never inspected."""
    polygon = np.asarray(quad, dtype=np.float32)
    centers: list[tuple[float, float]] = []
    for candidate in text_quads:
        try:
            points = np.asarray(candidate, dtype=np.float64).reshape(4, 2)
        except (TypeError, ValueError):
            continue
        if not np.isfinite(points).all():
            continue
        center = points.mean(axis=0)
        if cv2.pointPolygonTest(polygon, (float(center[0]), float(center[1])), False) >= 0:
            centers.append((float(center[0]), float(center[1])))
    if not centers:
        return 0, 0
    x0, y0 = np.min(quad, axis=0)
    x1, y1 = np.max(quad, axis=0)
    cells = {
        (min(2, max(0, int((x - x0) / max(1.0, x1 - x0) * 3))),
         min(2, max(0, int((y - y0) / max(1.0, y1 - y0) * 3))))
        for x, y in centers
    }
    return len(centers), len(cells)


def _polygon_iou(a: np.ndarray, b: np.ndarray) -> float:
    area_a, area_b = _polygon_area(a), _polygon_area(b)
    if area_a <= 0 or area_b <= 0:
        return 0.0
    try:
        intersection, _ = cv2.intersectConvexConvex(a.astype(np.float32), b.astype(np.float32))
    except cv2.error:
        return 0.0
    union = area_a + area_b - float(intersection)
    return float(intersection) / union if union > 0 else 0.0


def _same_quad(a: np.ndarray, b: np.ndarray) -> bool:
    """Deduplicate only near-identical polygons, including small contour jitter."""
    if _polygon_iou(a, b) < 0.92:
        return False
    diagonal = max(1.0, float(np.linalg.norm(np.ptp(np.vstack((a, b)), axis=0))))
    # The points are already ordered consistently, so direct corner distance
    # distinguishes alternate perspective contours with similar bounding boxes.
    # Thick printed borders expose inner and outer edge contours; treat up to
    # 2% of the page diagonal as the same physical edge, while polygon IoU and
    # per-corner comparison keep alternative perspective fits distinct.
    return float(np.mean(np.linalg.norm(a - b, axis=1))) <= 0.02 * diagonal


def _orientation_supported(quad: np.ndarray) -> bool:
    """Accept portrait spread geometry with modest image-plane tilt only."""
    width = (float(np.linalg.norm(quad[1] - quad[0])) + float(np.linalg.norm(quad[2] - quad[3]))) / 2
    height = (float(np.linalg.norm(quad[3] - quad[0])) + float(np.linalg.norm(quad[2] - quad[1]))) / 2
    if width <= 0 or height <= 0 or width / height > _MAX_SPREAD_ASPECT:
        return False
    top = quad[1] - quad[0]
    angle = abs(math.degrees(math.atan2(float(top[1]), float(top[0]))))
    angle = min(angle % 180, 180 - (angle % 180))
    return angle <= _MAX_PAGE_TILT_DEGREES


def _candidate_contours(gray: np.ndarray, image_rgb: np.ndarray) -> list[tuple[np.ndarray, str]]:
    outputs: list[tuple[np.ndarray, str]] = []
    blurred = cv2.GaussianBlur(gray, (3, 3), 0)
    for low, high in ((25, 80), (35, 120), (50, 160)):
        edges = cv2.Canny(blurred, low, high)
        for kernel_size in (3, 7, 11):
            kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (kernel_size, kernel_size))
            closed = cv2.morphologyEx(edges, cv2.MORPH_CLOSE, kernel, iterations=2)
            contours, _ = cv2.findContours(closed, cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)
            for contour in contours:
                perimeter = cv2.arcLength(contour, True)
                if perimeter < 1:
                    continue
                for epsilon in (0.012, 0.018, 0.025, 0.035):
                    approx = cv2.approxPolyDP(contour, epsilon * perimeter, True).reshape(-1, 2)
                    if len(approx) == 4 and cv2.isContourConvex(approx.reshape(-1, 1, 2)):
                        outputs.append((approx.astype(np.float64), f"canny_{low}_{high}_k{kernel_size}_e{epsilon:.3f}"))

    # Threshold-based contour hypotheses handle pale page edges against a
    # slightly different background. These remain real contour approximations.
    for threshold_mode, threshold in (
        (cv2.THRESH_BINARY | cv2.THRESH_OTSU, 0),
        (cv2.THRESH_BINARY_INV | cv2.THRESH_OTSU, 0),
        (cv2.THRESH_BINARY, 90),
        (cv2.THRESH_BINARY_INV, 90),
    ):
        _, binary = cv2.threshold(gray, threshold, 255, threshold_mode)
        for kernel_size in (3, 7):
            kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (kernel_size, kernel_size))
            closed = cv2.morphologyEx(binary, cv2.MORPH_CLOSE, kernel, iterations=1)
            contours, _ = cv2.findContours(closed, cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)
            for contour in contours:
                perimeter = cv2.arcLength(contour, True)
                if perimeter < 1:
                    continue
                for epsilon in (0.012, 0.018, 0.025, 0.035):
                    approx = cv2.approxPolyDP(contour, epsilon * perimeter, True).reshape(-1, 2)
                    if len(approx) == 4 and cv2.isContourConvex(approx.reshape(-1, 1, 2)):
                        outputs.append((approx.astype(np.float64), f"threshold_{threshold_mode}_k{kernel_size}_e{epsilon:.3f}"))

    hsv = cv2.cvtColor(image_rgb, cv2.COLOR_RGB2HSV)
    color_channels = (("red", image_rgb[:, :, 0]), ("green", image_rgb[:, :, 1]),
                      ("blue", image_rgb[:, :, 2]), ("saturation", hsv[:, :, 1]))
    for name, channel in color_channels:
        for threshold_mode in (cv2.THRESH_BINARY | cv2.THRESH_OTSU, cv2.THRESH_BINARY_INV | cv2.THRESH_OTSU):
            _, binary = cv2.threshold(channel, 0, 255, threshold_mode)
            closed = cv2.morphologyEx(
                binary, cv2.MORPH_CLOSE,
                cv2.getStructuringElement(cv2.MORPH_RECT, (7, 7)), iterations=1,
            )
            contours, _ = cv2.findContours(closed, cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)
            for contour in contours:
                perimeter = cv2.arcLength(contour, True)
                if perimeter < 1:
                    continue
                for epsilon in (0.018, 0.028):
                    approx = cv2.approxPolyDP(contour, epsilon * perimeter, True).reshape(-1, 2)
                    if len(approx) == 4 and cv2.isContourConvex(approx.reshape(-1, 1, 2)):
                        outputs.append((approx.astype(np.float64), f"color_{name}_{threshold_mode}_e{epsilon:.3f}"))
    return outputs


def _line_groups(edge_map: np.ndarray) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Cluster long Hough segments into supported horizontal and vertical lines."""
    height, width = edge_map.shape[:2]
    min_dimension = min(width, height)
    detected = cv2.HoughLinesP(
        edge_map, 1, np.pi / 180, threshold=max(24, int(min_dimension * 0.045)),
        minLineLength=max(24, int(min_dimension * 0.14)),
        maxLineGap=max(5, int(min_dimension * 0.025)),
    )
    if detected is None:
        return [], []
    segments = detected.reshape(-1, 4)
    records: list[dict[str, Any]] = []
    for x0, y0, x1, y1 in segments:
        p0 = np.array([x0, y0], dtype=np.float64)
        p1 = np.array([x1, y1], dtype=np.float64)
        delta = p1 - p0
        length = float(np.linalg.norm(delta))
        if length < max(24, min_dimension * 0.14):
            continue
        angle = math.atan2(float(delta[1]), float(delta[0]))
        # Line direction is undirected, so normalize its angle to [0, pi).
        angle %= math.pi
        horizontal_distance = min(angle, math.pi - angle)
        vertical_distance = abs(angle - math.pi / 2)
        axis = "horizontal" if horizontal_distance <= math.radians(28) else "vertical" if vertical_distance <= math.radians(28) else None
        if axis is None:
            continue
        direction = np.array([math.cos(angle), math.sin(angle)], dtype=np.float64)
        normal = np.array([-direction[1], direction[0]], dtype=np.float64)
        records.append({"axis": axis, "direction": direction, "normal": normal,
                        "rho": float(np.dot((p0 + p1) * 0.5, normal)),
                        "interval": sorted((float(np.dot(p0, direction)), float(np.dot(p1, direction)))),
                        "length": length, "points": np.stack((p0, p1))})
    records.sort(key=lambda item: item["length"], reverse=True)
    groups: dict[str, list[dict[str, Any]]] = {"horizontal": [], "vertical": []}
    offset_tolerance = min_dimension * 0.014
    angle_tolerance = math.radians(7.0)
    for record in records:
        target = groups[record["axis"]]
        match = None
        for group in target:
            if abs(float(np.dot(record["direction"], group["direction"]))) < math.cos(angle_tolerance):
                continue
            if abs(record["rho"] - float(np.dot(record["points"].mean(axis=0), group["normal"]))) <= offset_tolerance:
                match = group
                break
        if match is None:
            target.append({"axis": record["axis"], "direction": record["direction"], "normal": record["normal"],
                           "rho": record["rho"], "segments": [record], "length": record["length"]})
        else:
            match["segments"].append(record)
            match["length"] += record["length"]
            if record["length"] > max(item["length"] for item in match["segments"][:-1]):
                match["direction"] = record["direction"]
                match["normal"] = record["normal"]
                match["rho"] = record["rho"]
    for axis_groups in groups.values():
        axis_groups.sort(key=lambda item: item["length"], reverse=True)
    return groups["horizontal"][:16], groups["vertical"][:16]


def _line_intersection(a: dict[str, Any], b: dict[str, Any]) -> np.ndarray | None:
    matrix = np.stack((a["normal"], b["normal"]))
    determinant = float(np.linalg.det(matrix))
    if abs(determinant) < 0.15:
        return None
    point = np.linalg.solve(matrix, np.array([a["rho"], b["rho"]], dtype=np.float64))
    return point if np.isfinite(point).all() else None


def _line_coverage(group: dict[str, Any], start: np.ndarray, end: np.ndarray) -> float:
    direction = group["direction"]
    lo, hi = sorted((float(np.dot(start, direction)), float(np.dot(end, direction))))
    span = max(1.0, hi - lo)
    intervals: list[tuple[float, float]] = []
    for segment in group["segments"]:
        a, b = segment["interval"]
        a, b = max(lo, a), min(hi, b)
        if b > a:
            intervals.append((a, b))
    intervals.sort()
    covered = 0.0
    if intervals:
        left, right = intervals[0]
        for a, b in intervals[1:]:
            if a <= right:
                right = max(right, b)
            else:
                covered += right - left
                left, right = a, b
        covered += right - left
    return min(1.0, covered / span)


def _line_quad_candidates(edge_map: np.ndarray) -> tuple[list[tuple[np.ndarray, tuple[float, float, float, float], str]], int, int]:
    """Fit bounded quads from four independently supported long edge lines."""
    horizontal, vertical = _line_groups(edge_map)
    outputs: list[tuple[np.ndarray, tuple[float, float, float, float], str]] = []
    examined = 0
    for hi in range(len(horizontal)):
        for hj in range(hi + 1, len(horizontal)):
            top, bottom = horizontal[hi], horizontal[hj]
            for vi in range(len(vertical)):
                for vj in range(vi + 1, len(vertical)):
                    examined += 1
                    left, right = vertical[vi], vertical[vj]
                    points = [_line_intersection(top, left), _line_intersection(top, right),
                              _line_intersection(bottom, right), _line_intersection(bottom, left)]
                    if any(point is None for point in points):
                        continue
                    quad = _order_quad(np.asarray(points, dtype=np.float64))
                    if quad is None or not cv2.isContourConvex(quad.astype(np.float32).reshape(-1, 1, 2)):
                        continue
                    by_side = (
                        _line_coverage(top, quad[0], quad[1]),
                        _line_coverage(right, quad[1], quad[2]),
                        _line_coverage(bottom, quad[2], quad[3]),
                        _line_coverage(left, quad[3], quad[0]),
                    )
                    if min(by_side) >= _MIN_LINE_COVERAGE:
                        outputs.append((quad, by_side, "line_intersections"))
    return outputs, examined, len(horizontal) + len(vertical)


def _find_candidates(image: np.ndarray, text_quads: Iterable[Any]) -> tuple[list[_Candidate], dict[str, int]]:
    height, width = image.shape[:2]
    gray = cv2.cvtColor(image, cv2.COLOR_RGB2GRAY)
    edge = cv2.Canny(cv2.GaussianBlur(gray, (3, 3), 0), 35, 120)
    edge = cv2.dilate(edge, cv2.getStructuringElement(cv2.MORPH_RECT, (5, 5)))
    counters = {"contour_hypotheses": 0, "line_hypotheses": 0, "non_convex_or_invalid": 0,
                "implausible_geometry": 0, "unsupported_orientation": 0, "weak_edge_support": 0,
                "insufficient_text_geometry": 0, "line_coverage_insufficient": 0,
                "contour_candidates": 0, "line_candidates": 0, "line_groups": 0,
                "contrast_supported_sides": 0, "texture_supported_sides": 0,
                "line_boundary_profile_unconfirmed": 0}
    unique: list[_Candidate] = []
    method_for: list[set[str]] = []
    contour_hypotheses = _candidate_contours(gray, image)
    counters["contour_hypotheses"] = len(contour_hypotheses)
    for raw_quad, method in contour_hypotheses:
        ordered = _order_quad(raw_quad)
        if ordered is None or not cv2.isContourConvex(ordered.astype(np.float32).reshape(-1, 1, 2)):
            counters["non_convex_or_invalid"] += 1
            continue
        area_fraction = _polygon_area(ordered) / float(width * height)
        x0, y0 = np.min(ordered, axis=0)
        x1, y1 = np.max(ordered, axis=0)
        box_width, box_height = x1 - x0, y1 - y0
        margin = min(x0 / width, y0 / height, (width - x1) / width, (height - y1) / height)
        aspect = box_width / max(1.0, box_height)
        if (
            area_fraction < _MIN_AREA_FRACTION
            or area_fraction > _MAX_AREA_FRACTION
            or box_width < 0.35 * width
            or box_height < 0.32 * height
            or not 0.35 <= aspect <= 3.0
            or margin < _MIN_SIDE_MARGIN
        ):
            counters["implausible_geometry"] += 1
            continue
        if not _orientation_supported(ordered):
            counters["unsupported_orientation"] += 1
            continue
        support = _side_support(ordered, edge)
        contrast, texture = _side_boundary_profile(ordered, image)
        physical_support = tuple(max(edge_value, contrast_value, texture_value)
                                 for edge_value, contrast_value, texture_value in zip(support, contrast, texture))
        counters["contrast_supported_sides"] += sum(value >= _MIN_SIDE_SUPPORT for value in contrast)
        counters["texture_supported_sides"] += sum(value >= _MIN_SIDE_SUPPORT for value in texture)
        if min(physical_support) < _MIN_SIDE_SUPPORT or float(np.mean(physical_support)) < _MIN_MEAN_SUPPORT:
            counters["weak_edge_support"] += 1
            continue
        text_count, cell_count = _text_geometry(ordered, text_quads)
        # OCR layout is diagnostic only. It cannot establish or veto physical edges.
        if text_count < _MIN_TEXT_LINES or cell_count < _MIN_TEXT_CELLS:
            counters["insufficient_text_geometry"] += 1
        score = (0.26 * float(np.mean(support)) + 0.13 * min(support)
                 + 0.25 * float(np.mean(contrast)) + 0.13 * min(contrast)
                 + 0.16 * float(np.mean(texture)) + 0.07 * min(1.0, area_fraction / 0.55))
        existing_index = next((i for i, old in enumerate(unique) if _same_quad(old.quad, ordered)), None)
        if existing_index is None:
            unique.append(_Candidate(ordered, area_fraction, support, score, 1, text_count, cell_count,
                                     "contour", None, contrast, texture))
            counters["contour_candidates"] += 1
            method_for.append({method})
        else:
            old = unique[existing_index]
            method_for[existing_index].add(method)
            if score > old.score:
                unique[existing_index] = _Candidate(ordered, area_fraction, support, score,
                    len(method_for[existing_index]), text_count, cell_count, "contour", None, contrast, texture)
            else:
                unique[existing_index] = _Candidate(old.quad, old.area_fraction, old.side_support, old.score,
                    len(method_for[existing_index]), max(old.text_line_count, text_count),
                    max(old.text_cell_count, cell_count), old.source, old.line_coverage,
                    old.contrast_support, old.texture_asymmetry)
    line_edge = cv2.dilate(
        cv2.Canny(cv2.GaussianBlur(gray, (3, 3), 0), 20, 65),
        cv2.getStructuringElement(cv2.MORPH_RECT, (5, 5)),
    )
    line_hypotheses, line_pair_count, line_groups = _line_quad_candidates(line_edge)
    counters["line_hypotheses"] = line_pair_count
    counters["line_groups"] = line_groups
    for raw_quad, coverage, method in line_hypotheses:
        ordered = _order_quad(raw_quad)
        if ordered is None or not cv2.isContourConvex(ordered.astype(np.float32).reshape(-1, 1, 2)):
            counters["non_convex_or_invalid"] += 1
            continue
        area_fraction = _polygon_area(ordered) / float(width * height)
        x0, y0 = np.min(ordered, axis=0)
        x1, y1 = np.max(ordered, axis=0)
        box_width, box_height = x1 - x0, y1 - y0
        margin = min(x0 / width, y0 / height, (width - x1) / width, (height - y1) / height)
        aspect = box_width / max(1.0, box_height)
        if (area_fraction < _MIN_AREA_FRACTION or area_fraction > _MAX_AREA_FRACTION
                or box_width < 0.35 * width or box_height < 0.32 * height
                or not 0.35 <= aspect <= 3.0 or margin < _MIN_SIDE_MARGIN):
            counters["implausible_geometry"] += 1
            continue
        if not _orientation_supported(ordered):
            counters["unsupported_orientation"] += 1
            continue
        if min(coverage) < _MIN_LINE_COVERAGE:
            counters["line_coverage_insufficient"] += 1
            continue
        support = _side_support(ordered, edge)
        contrast, texture = _side_boundary_profile(ordered, image)
        physical_support = tuple(max(edge_value, contrast_value, texture_value)
                                 for edge_value, contrast_value, texture_value in zip(support, contrast, texture))
        counters["contrast_supported_sides"] += sum(value >= _MIN_SIDE_SUPPORT for value in contrast)
        counters["texture_supported_sides"] += sum(value >= _MIN_SIDE_SUPPORT for value in texture)
        if min(physical_support) < _MIN_SIDE_SUPPORT or float(np.mean(physical_support)) < _MIN_MEAN_SUPPORT:
            counters["weak_edge_support"] += 1
            continue
        # Intersections are accepted only when long segments support all four
        # fitted sides. Nearby edge support is retained as independent evidence.
        counters["line_candidates"] += 1
        if min(contrast) < _MIN_SIDE_SUPPORT or min(physical_support) < _MIN_SIDE_SUPPORT:
            counters["line_boundary_profile_unconfirmed"] += 1
    unique.sort(key=lambda item: item.score, reverse=True)
    return unique, counters


def _red_band_seam(rectified_rgb: np.ndarray) -> tuple[float | None, float | None, str]:
    """Find a unique long fold-like edge; red color is a secondary cue."""
    height, width = rectified_rgb.shape[:2]
    if height < 80 or width < 80:
        return None, None, "supported_seam_not_found"
    rgb = rectified_rgb.astype(np.int16)
    red = (rgb[:, :, 0] >= 70) & (rgb[:, :, 0] >= 1.18 * rgb[:, :, 1]) & (rgb[:, :, 0] >= 1.12 * rgb[:, :, 2])
    gray = cv2.cvtColor(rectified_rgb, cv2.COLOR_RGB2GRAY)
    edges = cv2.Canny(cv2.GaussianBlur(gray, (3, 3), 0), 25, 100) > 0
    vertical_gradient = np.abs(cv2.Sobel(gray, cv2.CV_32F, 0, 1, ksize=3))
    start, stop = int(height * 0.30), int(height * 0.70)
    radius = max(1, int(round(height * 0.002)))
    peaks: list[tuple[float, float, float, int]] = []
    for y in range(start, stop):
        y0, y1 = max(0, y - radius), min(height, y + radius + 1)
        red_columns = np.any(red[y0:y1], axis=0)
        red_support = float(np.mean(red_columns))
        indices = np.flatnonzero(red_columns)
        if len(indices):
            runs = np.split(indices, np.where(np.diff(indices) > 1)[0] + 1)
            longest_red_span = max(len(run) for run in runs) / width
        else:
            longest_red_span = 0.0
        edge_columns = np.any(edges[y0:y1], axis=0)
        edge_support = float(np.mean(edge_columns))
        edge_indices = np.flatnonzero(edge_columns)
        longest_edge_span = 0.0
        if len(edge_indices):
            edge_runs = np.split(edge_indices, np.where(np.diff(edge_indices) > 1)[0] + 1)
            longest_edge_span = max(len(run) for run in edge_runs) / width
        contrast_columns = np.max(vertical_gradient[y0:y1], axis=0) >= 18.0
        contrast_support = float(np.mean(contrast_columns))
        contrast_indices = np.flatnonzero(contrast_columns)
        longest_contrast_span = 0.0
        if len(contrast_indices):
            contrast_runs = np.split(contrast_indices, np.where(np.diff(contrast_indices) > 1)[0] + 1)
            longest_contrast_span = max(len(run) for run in contrast_runs) / width
        edge_supported = edge_support >= 0.52 and longest_edge_span >= 0.58
        red_supported = red_support >= 0.18 and longest_red_span >= 0.58
        contrast_supported = contrast_support >= 0.50 and longest_contrast_span >= 0.58
        # Red is optional. A long crease needs broad edge/contrast support;
        # compact stamps and isolated page rules do not establish a seam.
        if edge_supported and (contrast_supported or red_supported):
            score = 0.55 * edge_support + 0.25 * contrast_support + 0.20 * red_support
            peaks.append((score, red_support, longest_edge_span, y))
    if not peaks:
        return None, None, "supported_seam_not_found"
    peaks.sort(key=lambda item: item[3])
    cluster_radius = max(2, int(round(height * 0.03)))
    clusters: list[list[tuple[float, float, float, int]]] = []
    for peak in peaks:
        if not clusters or peak[3] - clusters[-1][-1][3] > cluster_radius:
            clusters.append([peak])
        else:
            clusters[-1].append(peak)
    representatives = [max(cluster, key=lambda item: item[0]) for cluster in clusters]
    representatives.sort(reverse=True)
    score, red_support, _, y = representatives[0]
    if len(representatives) > 1:
        second_score = representatives[1][0]
        if score - second_score < 0.08 or second_score >= 0.88 * score:
            return None, score, "seam_ambiguous"
    return y / max(1, height - 1), score, "supported_seam"


def _matrix_to_page(source_quad: np.ndarray, output_size: tuple[int, int]) -> tuple[np.ndarray, np.ndarray] | None:
    width, height = output_size
    if width < 2 or height < 2:
        return None
    target = np.array([[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]], dtype=np.float32)
    matrix = cv2.getPerspectiveTransform(source_quad.astype(np.float32), target)
    if not np.isfinite(matrix).all() or abs(float(np.linalg.det(matrix))) < 1e-12:
        return None
    inverse = np.linalg.inv(matrix)
    if not np.isfinite(inverse).all():
        return None
    return matrix.astype(np.float64), inverse.astype(np.float64)


def _build_pages(
    source_quad: np.ndarray,
    seam_ratio: float,
    original_size: tuple[int, int],
) -> tuple[RegisteredPage, RegisteredPage] | None:
    width, height = original_size
    points = source_quad.astype(np.float32)
    top_width = float(np.linalg.norm(points[1] - points[0]))
    bottom_width = float(np.linalg.norm(points[2] - points[3]))
    left_height = float(np.linalg.norm(points[3] - points[0]))
    right_height = float(np.linalg.norm(points[2] - points[1]))
    if min(top_width, bottom_width, left_height, right_height) < 2:
        return None

    # Get the seam endpoints by mapping a normalized split line through the
    # provisional document homography back into EXIF-normalized source pixels.
    doc_width = min(_PAGE_WIDTH, max(400, int(round((top_width + bottom_width) / 2))))
    doc_height = max(200, int(round(doc_width * ((left_height + right_height) / (top_width + bottom_width)))))
    doc_transform = _matrix_to_page(points, (doc_width, doc_height))
    if doc_transform is None:
        return None
    _, page_to_source = doc_transform
    split_y = seam_ratio * (doc_height - 1)
    doc_seam = np.array([[[0.0, split_y], [doc_width - 1.0, split_y]]], dtype=np.float32)
    seam_source = cv2.perspectiveTransform(doc_seam, page_to_source.astype(np.float64))[0]
    tl, tr, br, bl = points
    upper_quad = np.array([tl, tr, seam_source[1], seam_source[0]], dtype=np.float32)
    lower_quad = np.array([seam_source[0], seam_source[1], br, bl], dtype=np.float32)
    pages: list[RegisteredPage] = []
    for name, quad in (("image_upper", upper_quad), ("image_lower", lower_quad)):
        if not cv2.isContourConvex(quad.reshape(-1, 1, 2)) or _polygon_area(quad) <= 1:
            return None
        if np.min(quad[:, 0]) < -0.5 or np.min(quad[:, 1]) < -0.5 or np.max(quad[:, 0]) > width - 0.5 or np.max(quad[:, 1]) > height - 0.5:
            return None
        page_width = _PAGE_WIDTH
        mean_h = (float(np.linalg.norm(quad[3] - quad[0])) + float(np.linalg.norm(quad[2] - quad[1]))) / 2
        mean_w = (float(np.linalg.norm(quad[1] - quad[0])) + float(np.linalg.norm(quad[2] - quad[3]))) / 2
        page_height = int(round(page_width * mean_h / max(1.0, mean_w)))
        page_height = max(200, min(1800, page_height))
        transforms = _matrix_to_page(quad, (page_width, page_height))
        if transforms is None:
            return None
        to_page, to_source = transforms
        pages.append(RegisteredPage(
            name=name,
            source_quad=[[float(x), float(y)] for x, y in quad],
            source_size=(width, height),
            output_size=(page_width, page_height),
            original_to_page=to_page.tolist(),
            page_to_original=to_source.tolist(),
        ))
    return pages[0], pages[1]


def register_spread(image_rgb: np.ndarray, *, text_quads: Iterable[Any]) -> RegistrationResult:
    """Propose a physical spread quad, long-band seam and two page transforms.

    Candidate search runs on a max-side-1200 copy. The caller's array is never
    modified. No OCR text or values are read; ``text_quads`` are geometry only.
    """
    source = np.asarray(image_rgb)
    if source.ndim != 3 or source.shape[2] != 3 or source.dtype != np.uint8:
        return RegistrationResult("rejected", "invalid_rgb_image", 0)
    original_height, original_width = source.shape[:2]
    if min(original_height, original_width) < 80:
        return RegistrationResult("rejected", "image_too_small", 0, input_size=(original_width, original_height))
    scale = min(1.0, _MAX_SIDE / max(original_width, original_height))
    working = cv2.resize(source, None, fx=scale, fy=scale, interpolation=cv2.INTER_AREA) if scale < 1 else source.copy()
    work_height, work_width = working.shape[:2]
    scale_x = work_width / original_width
    scale_y = work_height / original_height
    working_text_quads: list[np.ndarray] = []
    for raw in text_quads:
        try:
            quad = np.asarray(raw, dtype=np.float64).reshape(4, 2).copy()
        except (TypeError, ValueError):
            continue
        if np.isfinite(quad).all():
            # OpenCV resize maps pixel centers. Use actual rounded output sizes,
            # not the requested fx/fy, for a reversible coordinate transform.
            working_text_quads.append(_source_to_working_points(quad, scale_x, scale_y))
    candidates, rejection_counts = _find_candidates(working, working_text_quads)
    if not candidates:
        if rejection_counts["weak_edge_support"]:
            reason = "edge_support_insufficient"
        elif rejection_counts["unsupported_orientation"]:
            reason = "orientation_outside_supported_range"
        elif rejection_counts["implausible_geometry"]:
            reason = "quad_implausible"
        elif rejection_counts["non_convex_or_invalid"]:
            reason = "convex_quad_not_found"
        else:
            reason = "contour_not_found"
        return RegistrationResult("rejected", reason, 0, input_size=(original_width, original_height), stage_counts=rejection_counts)

    if len(candidates) > 1 and (
        candidates[0].score - candidates[1].score < _AMBIGUITY_MARGIN
        or _polygon_iou(candidates[0].quad, candidates[1].quad) >= _NESTED_AMBIGUITY_IOU
    ):
        candidate_quads = tuple(
            _working_to_source_points(candidate.quad, scale_x, scale_y).tolist()
            for candidate in candidates[:4]
        )
        return RegistrationResult(
            "ambiguous", "multiple_comparable_or_nested_physical_boundaries", len(candidates),
            candidate_quads=candidate_quads,
            candidate_score=candidates[0].score,
            input_size=(original_width, original_height),
            stage_counts=rejection_counts,
        )
    chosen = candidates[0]
    working_quad = chosen.quad
    rect_width = min(_PAGE_WIDTH, max(400, int(round((np.linalg.norm(working_quad[1] - working_quad[0]) + np.linalg.norm(working_quad[2] - working_quad[3])) / 2))))
    rect_height = max(240, min(1800, int(round(rect_width * ((np.linalg.norm(working_quad[3] - working_quad[0]) + np.linalg.norm(working_quad[2] - working_quad[1])) / (np.linalg.norm(working_quad[1] - working_quad[0]) + np.linalg.norm(working_quad[2] - working_quad[3])))))))
    transform = _matrix_to_page(working_quad, (rect_width, rect_height))
    if transform is None:
        return RegistrationResult("rejected", "document_transform_invalid", len(candidates), input_size=(original_width, original_height), stage_counts=rejection_counts)
    to_rect, _ = transform
    rectified = cv2.warpPerspective(working, to_rect, (rect_width, rect_height), flags=cv2.INTER_LINEAR)
    seam_ratio, seam_support, seam_reason = _red_band_seam(rectified)
    original_quad = _working_to_source_points(working_quad, scale_x, scale_y)
    if seam_ratio is None:
        return RegistrationResult(
            "rejected", seam_reason, len(candidates),
            source_quad=original_quad.tolist(), area_fraction=chosen.area_fraction,
            edge_support=chosen.side_support, candidate_score=chosen.score,
            contrast_support=chosen.contrast_support, texture_asymmetry=chosen.texture_asymmetry,
            seam_support=seam_support, input_size=(original_width, original_height),
            stage_counts=rejection_counts,
            selected_candidate_source=chosen.source,
        )
    pages = _build_pages(original_quad, seam_ratio, (original_width, original_height))
    if pages is None:
        return RegistrationResult(
            "rejected", "page_transform_invalid", len(candidates),
            source_quad=original_quad.tolist(), area_fraction=chosen.area_fraction,
            edge_support=chosen.side_support, candidate_score=chosen.score,
            contrast_support=chosen.contrast_support, texture_asymmetry=chosen.texture_asymmetry,
            seam_ratio=seam_ratio, seam_support=seam_support,
            input_size=(original_width, original_height),
            stage_counts=rejection_counts,
            selected_candidate_source=chosen.source,
        )
    return RegistrationResult(
        "accepted", "provisional_quad_and_seam_supported", len(candidates),
        source_quad=original_quad.tolist(), area_fraction=chosen.area_fraction,
        edge_support=chosen.side_support, candidate_score=chosen.score,
        contrast_support=chosen.contrast_support, texture_asymmetry=chosen.texture_asymmetry,
        seam_ratio=seam_ratio, seam_support=seam_support,
        pages=pages, input_size=(original_width, original_height),
        stage_counts=rejection_counts,
        selected_candidate_source=chosen.source,
    )


def warp_registered_page(image_rgb: np.ndarray, page: RegisteredPage) -> np.ndarray:
    """Rectify one registered page without mutating the source image."""
    width, height = page.output_size
    matrix = np.asarray(page.original_to_page, dtype=np.float64)
    if matrix.shape != (3, 3) or not np.isfinite(matrix).all() or abs(float(np.linalg.det(matrix))) < 1e-12:
        raise ValueError("page_transform_invalid")
    return cv2.warpPerspective(np.asarray(image_rgb), matrix, (width, height), flags=cv2.INTER_LINEAR)


def map_page_quad_to_original(quad: Any, page: RegisteredPage) -> list[list[float]] | None:
    """Map an OCR quadrilateral from normalized page pixels to source pixels."""
    try:
        points = np.asarray(quad, dtype=np.float64).reshape(4, 2)
    except (TypeError, ValueError):
        return None
    inverse = np.asarray(page.page_to_original, dtype=np.float64)
    if not np.isfinite(points).all() or inverse.shape != (3, 3) or not np.isfinite(inverse).all():
        return None
    if abs(float(np.linalg.det(inverse))) < 1e-12:
        return None
    width, height = page.output_size
    # OCR boxes use continuous pixel edges, so a one-pixel tolerance is
    # allowed around [0,width]x[0,height]. Larger errors are rejected, never clipped.
    if (
        np.any(points[:, 0] < -_MAPPING_TOLERANCE_PX)
        or np.any(points[:, 1] < -_MAPPING_TOLERANCE_PX)
        or np.any(points[:, 0] > width + _MAPPING_TOLERANCE_PX)
        or np.any(points[:, 1] > height + _MAPPING_TOLERANCE_PX)
    ):
        return None
    mapped = cv2.perspectiveTransform(points.astype(np.float32).reshape(1, 4, 2), inverse.astype(np.float32))[0]
    source_quad = np.asarray(page.source_quad, dtype=np.float64)
    if not np.isfinite(mapped).all():
        return None
    # Source page corners and all interior points must remain within the image
    # coordinate frame established by the registration result.
    max_x = max(point[0] for point in source_quad)
    max_y = max(point[1] for point in source_quad)
    min_x = min(point[0] for point in source_quad)
    min_y = min(point[1] for point in source_quad)
    source_width, source_height = page.source_size
    if (
        np.any(mapped[:, 0] < -_MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 1] < -_MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 0] > source_width + _MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 1] > source_height + _MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 0] < min_x - _MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 1] < min_y - _MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 0] > max_x + _MAPPING_TOLERANCE_PX)
        or np.any(mapped[:, 1] > max_y + _MAPPING_TOLERANCE_PX)
    ):
        return None
    return [[float(x), float(y)] for x, y in mapped]
