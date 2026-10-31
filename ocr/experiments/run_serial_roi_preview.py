#!/usr/bin/env python3
"""Render private, offline previews for saved serial ROI proposals.

This module only decodes and annotates the explicitly bound raster. It never
imports an OCR reader or runs recognition. Proposals come from the Go CLI.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw, ImageFont, ImageOps


SCHEMA = "passport-serial-roi-preview/v1"
MANIFEST_SCHEMA = "serial-roi-preview-run/v1"
REGISTERED_LINEAGE_SCHEMA = "passport-registered-spread-lineage/v1"
COLORS = {"series": (255, 70, 170), "number": (30, 210, 255), "shared_column": (245, 190, 45)}
FIELDS = ("series", "number")


class PreviewError(RuntimeError):
    """Raised when a frame or locator result cannot be safely rendered."""


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def write_private(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
    os.chmod(path, 0o600)


def json_bytes(value: Any) -> bytes:
    return (json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode()


def load_bound_frame(item: dict[str, Any], dataset_root: Path) -> tuple[Image.Image, dict[str, Any], Path, bytes]:
    """Load only the exact raster declared by the saved visualization manifest."""
    dataset_root = dataset_root.resolve()
    source_rel = Path(item["source_raster"])
    parts = source_rel.parts
    try:
        marker = parts.index(dataset_root.name)
    except ValueError as exc:
        raise PreviewError("manifest raster path is not rooted at the selected dataset") from exc
    source = (dataset_root.joinpath(*parts[marker + 1 :])).resolve()
    if not source.is_file():
        raise PreviewError("manifest source raster is missing")
    try:
        source.relative_to(dataset_root)
    except ValueError as exc:
        raise PreviewError("manifest source raster escapes dataset root") from exc

    raw = source.read_bytes()
    digest = sha256_bytes(raw)
    if digest != item.get("plan_image_sha256"):
        raise PreviewError("source raster bytes do not match manifest SHA-256")
    try:
        with Image.open(source) as opened:
            direct = item.get("source_kind", "").startswith("pipeline canonical")
            frame = opened.copy() if direct else ImageOps.exif_transpose(opened)
            frame.load()
    except Exception as exc:
        raise PreviewError("source raster cannot be decoded") from exc
    dims = item.get("plan_dimensions")
    if not isinstance(dims, list) or len(dims) != 2 or list(frame.size) != dims:
        raise PreviewError("normalized frame dimensions do not match manifest")
    obs_path = source.parent.parent / "ocr" / "observations.json" if direct else source.parent / "output/ocr/observations.json"
    # Canonical rasters live at output/ocr/canonical-spread.png; input JPEGs at sample-NN/input.jpg.
    if direct:
        obs_path = source.parent / "observations.json"
    if not obs_path.is_file():
        raise PreviewError("saved observations are missing for manifest raster")
    observations = json.loads(obs_path.read_text())
    image = observations.get("image", {})
    if (not item.get("sample") or image.get("id") == "" or image.get("width") != frame.width
            or image.get("height") != frame.height or image.get("sha256") != digest
            or image.get("sha256") != item.get("plan_image_sha256")):
        raise PreviewError("normalized raster does not match observations image binding")
    return frame.convert("RGB"), observations, source, raw


def _finite_rect(value: Any, width: int, height: int) -> tuple[float, float, float, float]:
    if not isinstance(value, list) or len(value) != 4:
        raise PreviewError("ROI bounds must contain four coordinates")
    rect = tuple(float(v) for v in value)
    if not all(math.isfinite(v) for v in rect):
        raise PreviewError("ROI bounds must be finite")
    x0, y0, x1, y1 = rect
    if x0 < 0 or y0 < 0 or x1 <= x0 or y1 <= y0 or x1 > width or y1 > height:
        raise PreviewError("ROI bounds are outside the bound raster")
    return rect


def validate_proposal(proposal: dict[str, Any], raster: dict[str, Any]) -> None:
    if proposal.get("schema_version") != SCHEMA:
        raise PreviewError("unsupported Go proposal schema")
    if proposal.get("status") not in {"proposed", "ambiguous", "insufficient_geometry"}:
        raise PreviewError("invalid Go proposal status")
    got = proposal.get("raster")
    if got != raster:
        raise PreviewError("Go proposal raster binding changed")
    for field in (*FIELDS, "shared_column"):
        roi = proposal.get(field)
        if not isinstance(roi, dict) or roi.get("status") not in {"proposed", "ambiguous", "unavailable"}:
            raise PreviewError("invalid ROI status")
        if roi["status"] == "unavailable":
            if roi.get("quad") is not None or roi.get("bounds") is not None:
                raise PreviewError("unavailable ROI must not carry geometry")
            continue
        if roi.get("frame_id") != raster["id"]:
            raise PreviewError("ROI frame does not match raster")
        if roi.get("manual_review_required") is not True:
            raise PreviewError("preview ROI must require manual review")
        _finite_rect(roi.get("bounds"), raster["width"], raster["height"])
        quad = roi.get("quad")
        if not isinstance(quad, list) or len(quad) != 4:
            raise PreviewError("ROI quad must contain four points")
        for point in quad:
            if not isinstance(point, list) or len(point) != 2 or not all(math.isfinite(float(v)) for v in point):
                raise PreviewError("ROI quad contains invalid coordinates")
            if not (0 <= float(point[0]) <= raster["width"] and 0 <= float(point[1]) <= raster["height"]):
                raise PreviewError("ROI quad is outside the bound raster")
    for hypothesis in proposal.get("alternative_hypotheses", []):
        roi = hypothesis.get("shared_column", {})
        if roi.get("status") not in {"proposed", "ambiguous"}:
            continue
        if roi.get("frame_id") != raster["id"]:
            raise PreviewError("alternative frame does not match raster")
        _finite_rect(roi.get("bounds"), raster["width"], raster["height"])


def page_frame_request(item: dict[str, Any], raster: dict[str, Any],
                       lineage: dict[str, Any] | None) -> dict[str, Any]:
    """Use physical page boundaries only when bound to a registered canonical raster."""
    fallback = {"page_frame_kind": "whole_document_fallback"}
    if not item.get("source_kind", "").startswith("pipeline canonical") or not isinstance(lineage, dict):
        return fallback
    canonical = lineage.get("canonical_image")
    mappings = lineage.get("page_mappings")
    if (lineage.get("schema_version") != REGISTERED_LINEAGE_SCHEMA
            or not isinstance(canonical, dict)
            or canonical.get("id") != raster["id"]
            or canonical.get("sha256") != raster["sha256"]
            or canonical.get("width") != raster["width"]
            or canonical.get("height") != raster["height"]
            or not isinstance(mappings, dict) or len(mappings) < 2):
        return fallback
    pages: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    for mapping in mappings.values():
        if not isinstance(mapping, dict):
            return fallback
        page_id = mapping.get("physical_page")
        bounds = mapping.get("canonical_rect")
        if not isinstance(page_id, str) or not page_id or page_id in seen_ids:
            return fallback
        try:
            rect = _finite_rect(bounds, raster["width"], raster["height"])
        except (PreviewError, TypeError, ValueError):
            return fallback
        seen_ids.add(page_id)
        pages.append({"id": page_id, "bounds": list(rect)})
    if len(pages) != len(mappings) or len(pages) < 2:
        return fallback
    pages.sort(key=lambda page: page["id"])
    return {"page_frame_kind": "registered_pages", "pages": pages}


def _draw_dashed(draw: ImageDraw.ImageDraw, points: list[tuple[float, float]], fill: tuple[int, int, int], width: int = 3, dash: float = 12.0) -> None:
    for start, end in zip(points, points[1:] + points[:1]):
        dx, dy = end[0] - start[0], end[1] - start[1]
        length = math.hypot(dx, dy)
        if length == 0:
            continue
        ux, uy = dx / length, dy / length
        pos = 0.0
        while pos < length:
            stop = min(pos + dash, length)
            draw.line((start[0] + ux * pos, start[1] + uy * pos,
                       start[0] + ux * stop, start[1] + uy * stop), fill=fill, width=width)
            pos += dash * 1.8


def _draw_tag(draw: ImageDraw.ImageDraw, image_size: tuple[int, int], anchor_x: float,
              anchor_y: float, label: str, color: tuple[int, int, int], row: int = 0,
              fallback_y: float | None = None) -> None:
    font = ImageFont.load_default()
    measured = draw.textbbox((0, 0), label, font=font)
    text_width = measured[2] - measured[0]
    tx = int(anchor_x)
    if tx + text_width + 4 > image_size[0]:
        tx = max(3, int(anchor_x) - text_width - 4)
    ty = int(anchor_y) - 16 - row * 13
    if ty < 2:
        fy = anchor_y if fallback_y is None else fallback_y
        ty = min(image_size[1] - 14, int(fy) + 2 + row * 13)
    box = draw.textbbox((tx, ty), label, font=font)
    draw.rectangle((box[0] - 2, box[1] - 1, box[2] + 2, box[3] + 1), fill=(15, 20, 28))
    draw.text((tx, ty), label, fill=color, font=font)


def _draw_roi(draw: ImageDraw.ImageDraw, roi: dict[str, Any], label: str,
              color: tuple[int, int, int], dashed: bool, image_size: tuple[int, int], thin: bool = False) -> None:
    if roi.get("status") not in {"proposed", "ambiguous"}:
        return
    points = [(float(p[0]), float(p[1])) for p in roi["quad"]]
    if dashed:
        _draw_dashed(draw, points, color, width=2 if thin else 4)
    else:
        draw.line(points + [points[0]], fill=color, width=2 if thin else 4)
    x0, y0 = min(p[0] for p in points), min(p[1] for p in points)
    x1, y1 = max(p[0] for p in points), max(p[1] for p in points)
    _draw_tag(draw, image_size, x0, y0, label, color, fallback_y=y1)


def _draw_overlap(draw: ImageDraw.ImageDraw, rois: list[dict[str, Any]], labels: list[str],
                  colors: list[tuple[int, int, int]], dashed: bool, image_size: tuple[int, int]) -> None:
    """Show coincident proposals with interleaved colors without moving either box."""
    points = [(float(p[0]), float(p[1])) for p in rois[0]["quad"]]
    segment = 14.0
    for start, end in zip(points, points[1:] + points[:1]):
        dx, dy = end[0] - start[0], end[1] - start[1]
        length = math.hypot(dx, dy)
        if length == 0:
            continue
        ux, uy = dx / length, dy / length
        pos, index = 0.0, 0
        stride = segment * (1.7 if dashed else 1.0)
        while pos < length:
            stop = min(pos + segment, length)
            color = colors[index % len(colors)]
            draw.line((start[0] + ux * pos, start[1] + uy * pos,
                       start[0] + ux * stop, start[1] + uy * stop), fill=color, width=4)
            pos += stride
            index += 1
    x0 = min(p[0] for p in points)
    y0 = min(p[1] for p in points)
    y1 = max(p[1] for p in points)
    for index, (label, color) in enumerate(zip(labels, colors)):
        _draw_tag(draw, image_size, x0, y0, label, color, index, y1)


def _draw_note(draw: ImageDraw.ImageDraw, image_size: tuple[int, int], bounds: list[float],
               label: str, color: tuple[int, int, int], row: int = 0) -> None:
    """Place a status note beside a real box without inventing another box."""
    _draw_tag(draw, image_size, bounds[0], bounds[1], label, color, row, bounds[3])


def draw_overlay(frame: Image.Image, proposal: dict[str, Any]) -> Image.Image:
    canvas = frame.copy()
    draw = ImageDraw.Draw(canvas)
    parent_ambiguous = proposal["status"] == "ambiguous"
    series, number = proposal["series"], proposal["number"]
    field_assessment = assess_field_split(series, number)
    both = all(roi.get("status") in {"proposed", "ambiguous"} for roi in (series, number))
    coincident = both and series.get("bounds") == number.get("bounds")
    shared = proposal["shared_column"]
    shared_matches_field = shared.get("bounds") in [series.get("bounds"), number.get("bounds")]
    if field_assessment["status"] != "distinct":
        if shared.get("status") in {"proposed", "ambiguous"}:
            _draw_roi(draw, shared, "shared candidate column", COLORS["shared_column"],
                      parent_ambiguous or shared.get("status") == "ambiguous", canvas.size, thin=True)
            bounds = shared.get("bounds")
        elif coincident:
            _draw_roi(draw, number, "shared field candidate", COLORS["shared_column"],
                      parent_ambiguous or number.get("status") == "ambiguous", canvas.size, thin=True)
            bounds = number.get("bounds")
        else:
            bounds = None
        if bounds:
            _draw_note(draw, canvas.size, bounds,
                       field_split_status_label(field_assessment, proposal), COLORS["series"], row=1)
    elif not shared_matches_field:
        _draw_roi(draw, shared, "shared column", COLORS["shared_column"], parent_ambiguous, canvas.size, thin=True)
    if field_assessment["status"] != "distinct":
        pass
    elif coincident:
        ambiguous = parent_ambiguous or series["status"] == "ambiguous" or number["status"] == "ambiguous"
        _draw_roi(draw, number, f"number: {'ambiguous' if ambiguous else 'proposed'}",
                  COLORS["number"], ambiguous, canvas.size)
        series_note = "series: same candidate bounds; not separately located"
        if series.get("reason") == "sibling_numeric_geometry_only":
            series_note = "series: sibling geometry only; not separately located"
        _draw_note(draw, canvas.size, number["bounds"], series_note, COLORS["series"], row=1)
    else:
        for field in FIELDS:
            roi = proposal[field]
            if roi["status"] == "unavailable":
                continue
            ambiguous = parent_ambiguous or roi["status"] == "ambiguous"
            status = "ambiguous" if ambiguous else "proposed"
            _draw_roi(draw, roi, f"{field}: {status}", COLORS[field], ambiguous, canvas.size)
    for index, hypothesis in enumerate(proposal.get("alternative_hypotheses", []), start=1):
        roi = hypothesis.get("shared_column", {})
        if roi.get("status") in {"proposed", "ambiguous"}:
            _draw_roi(draw, roi, f"alternative {index}: ambiguous", (230, 230, 230), True, canvas.size, thin=True)
    banner = "GEOMETRY ONLY - NO OCR RUN - MANUAL REVIEW REQUIRED"
    if field_assessment["status"] == "invalid":
        banner += " - FIELD SPLIT INVALID"
    elif field_assessment["status"] == "provisional":
        banner += " - FIELD SPLIT PROVISIONAL"
    font = ImageFont.load_default()
    box = draw.textbbox((0, 0), banner, font=font)
    draw.rectangle((0, 0, box[2] + 8, 18), fill=(15, 20, 28))
    draw.text((4, 4), banner, font=font, fill=(255, 255, 255))
    return canvas


def crop_bounds(bounds: tuple[float, float, float, float], image_size: tuple[int, int], padding_fraction: float = 0.35) -> tuple[int, int, int, int]:
    """Return half-open, centered crop bounds with scale-relative padding."""
    if not math.isfinite(padding_fraction) or padding_fraction < 0:
        raise PreviewError("invalid crop padding")
    x0, y0, x1, y1 = bounds
    cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
    w, h = x1 - x0, y1 - y0
    pad_x, pad_y = w * padding_fraction, h * padding_fraction
    left = max(0, math.floor(cx - w / 2 - pad_x))
    top = max(0, math.floor(cy - h / 2 - pad_y))
    right = min(image_size[0], math.ceil(cx + w / 2 + pad_x))
    bottom = min(image_size[1], math.ceil(cy + h / 2 + pad_y))
    if right <= left or bottom <= top:
        raise PreviewError("empty centered crop")
    return left, top, right, bottom


def centered_field_crop_bounds(bounds: tuple[float, float, float, float], image_size: tuple[int, int],
                               field: str, proposal: dict[str, Any],
                               padding_fraction: float = 0.35) -> tuple[int, int, int, int]:
    """Pad a field crop, then clip padding at the geometric split to its sibling ROI."""
    if field not in FIELDS:
        raise PreviewError("unknown field crop")
    result = list(crop_bounds(bounds, image_size, padding_fraction))
    series = proposal.get("series", {}).get("bounds")
    number = proposal.get("number", {}).get("bounds")
    if not (isinstance(series, list) and isinstance(number, list)
            and len(series) == 4 and len(number) == 4):
        return tuple(result)

    series_width, number_width = series[2] - series[0], number[2] - number[0]
    series_height, number_height = series[3] - series[1], number[3] - number[1]
    x_separation = abs((series[0] + series[2] - number[0] - number[2]) / 2) / max(series_width, number_width, 1.0)
    y_separation = abs((series[1] + series[3] - number[1] - number[3]) / 2) / max(series_height, number_height, 1.0)
    axis = 1 if y_separation >= x_separation else 0
    series_center = (series[axis] + series[axis + 2]) / 2
    number_center = (number[axis] + number[axis + 2]) / 2
    if series_center <= number_center:
        split = (series[axis + 2] + number[axis]) / 2
        if field == "series":
            result[axis + 2] = min(result[axis + 2], math.floor(split))
        else:
            result[axis] = max(result[axis], math.ceil(split))
    else:
        split = (number[axis + 2] + series[axis]) / 2
        if field == "number":
            result[axis + 2] = min(result[axis + 2], math.floor(split))
        else:
            result[axis] = max(result[axis], math.ceil(split))
    if result[2] <= result[0] or result[3] <= result[1]:
        raise PreviewError("field crop is empty after sibling-boundary clipping")
    return tuple(result)


def assess_field_split(series: dict[str, Any], number: dict[str, Any]) -> dict[str, Any]:
    """Assess whether two proposed field boxes are geometrically distinguishable."""
    if series.get("status") not in {"proposed", "ambiguous"} or number.get("status") not in {"proposed", "ambiguous"}:
        return {"status": "provisional", "reason": "one_or_both_field_rois_unavailable", "overlap_of_smaller": None}
    a, b = series.get("bounds"), number.get("bounds")
    if not isinstance(a, list) or not isinstance(b, list) or len(a) != 4 or len(b) != 4:
        return {"status": "provisional", "reason": "field_roi_bounds_missing", "overlap_of_smaller": None}
    intersection = max(0.0, min(a[2], b[2]) - max(a[0], b[0])) * max(0.0, min(a[3], b[3]) - max(a[1], b[1]))
    area_a = max(0.0, a[2] - a[0]) * max(0.0, a[3] - a[1])
    area_b = max(0.0, b[2] - b[0]) * max(0.0, b[3] - b[1])
    smaller = min(area_a, area_b)
    ratio = intersection / smaller if smaller > 0 else 0.0
    if a == b:
        return {"status": "invalid", "reason": "series_and_number_bounds_identical", "overlap_of_smaller": ratio}
    if ratio >= 0.8:
        return {"status": "invalid", "reason": "field_rois_overlap_at_least_80_percent", "overlap_of_smaller": ratio}
    if ratio >= 0.5:
        return {"status": "provisional", "reason": "field_rois_overlap_at_least_50_percent", "overlap_of_smaller": ratio}
    return {"status": "distinct", "reason": "field_roi_bounds_are_separate", "overlap_of_smaller": ratio}


def field_split_status_label(assessment: dict[str, Any], proposal: dict[str, Any]) -> str:
    status = assessment["status"].upper()
    rois = (proposal["series"], proposal["number"], proposal["shared_column"])
    ambiguous = proposal.get("status") == "ambiguous" or any(roi.get("status") == "ambiguous" for roi in rois)
    if ambiguous:
        status = f"AMBIGUOUS + {status}"
    return f"field split {status}: {assessment['reason']}"


def field_crop_plan(proposal: dict[str, Any]) -> tuple[dict[str, dict[str, Any]], dict[str, str]]:
    """Select crops only when series and number have distinguishable boxes."""
    crops: dict[str, dict[str, Any]] = {}
    omissions: dict[str, str] = {}
    series, number = proposal["series"], proposal["number"]
    assessment = assess_field_split(series, number)
    if assessment["status"] != "distinct":
        reason = f"field split {assessment['status']}: {assessment['reason']}"
        return {}, {"series": reason, "number": reason}
    for field in FIELDS:
        roi = proposal[field]
        if roi.get("status") not in {"proposed", "ambiguous"}:
            continue
        crops[field] = roi
    return crops, omissions


def _proposal_summary(proposal: dict[str, Any]) -> dict[str, Any]:
    summary = {"status": proposal["status"], "reason": proposal.get("reason"),
               "orientation_status": proposal.get("orientation_status"),
               "rotation_hypotheses": proposal.get("rotation_hypotheses", []),
               "geometry_support": proposal.get("geometry_support"),
               "pattern_categories": proposal.get("pattern_categories", []),
               "source_mapping": "unavailable"}
    for field in (*FIELDS, "shared_column"):
        roi = proposal[field]
        summary[field] = {"status": roi["status"], "reason": roi.get("reason"),
                          "bounds": roi.get("bounds"), "page_id": roi.get("page_id"),
                          "manual_review_required": roi.get("manual_review_required", False),
                          "transform_validation": roi.get("transform_validation", "unavailable")}
    summary["alternative_hypotheses"] = [
        {"page_id": hypothesis.get("page_id"), "geometry_support": hypothesis.get("geometry_support"),
         "shared_column_bounds": hypothesis.get("shared_column", {}).get("bounds")}
        for hypothesis in proposal.get("alternative_hypotheses", [])
    ]
    return summary


def _resize_preview(image: Image.Image, max_side: int = 1400) -> Image.Image:
    preview = image.copy()
    preview.thumbnail((max_side, max_side), Image.Resampling.LANCZOS)
    return preview


def _sha_file(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def run(manifest_path: Path, binary_path: Path, dataset_root: Path, output_root: Path) -> Path:
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("schema") != "diagnostic-vl-roi-visualizations/v1" or len(manifest.get("items", [])) != 5:
        raise PreviewError("expected the retained five-item visualization manifest")
    if not binary_path.is_file():
        raise PreviewError("Go preview CLI binary is missing")
    binary_hash_before = _sha_file(binary_path)
    repo = Path(__file__).resolve().parents[2]
    tracked_sources = {
        "renderer": Path(__file__).resolve(),
        "tests": Path(__file__).with_name("test_serial_roi_preview.py").resolve(),
        "go_locator": repo / "internal/passport/serial_roi.go",
    }
    source_hashes_before = {name: _sha_file(path) for name, path in tracked_sources.items()}
    now = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output_root.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(output_root, 0o700)
    run_dir = output_root / now
    run_dir.mkdir(parents=True, mode=0o700, exist_ok=False)
    os.chmod(run_dir, 0o700)
    sample_entries: list[dict[str, Any]] = []
    for item in manifest["items"]:
        sample = item["sample"]
        frame, observations, source, source_bytes = load_bound_frame(item, dataset_root)
        raster = {"id": observations["image"]["id"], "width": frame.width,
                  "height": frame.height, "sha256": sha256_bytes(source_bytes)}
        lineage: dict[str, Any] | None = None
        if item.get("source_kind", "").startswith("pipeline canonical"):
            lineage_path = source.parent / "registration-lineage.json"
            if lineage_path.is_file():
                lineage = json.loads(lineage_path.read_text())
        frame_request = page_frame_request(item, raster, lineage)
        request: dict[str, Any] = {"raster": raster, "observations": observations, **frame_request}
        sample_dir = run_dir / sample
        sample_dir.mkdir(mode=0o700)
        req_path = sample_dir / "request.json"
        proposal_path = sample_dir / "proposal.json"
        write_private(req_path, json_bytes(request))
        subprocess.run([str(binary_path), "-input", str(req_path), "-output", str(proposal_path)],
                       check=True, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        os.chmod(proposal_path, 0o600)
        proposal = json.loads(proposal_path.read_text())
        validate_proposal(proposal, raster)
        overlay = draw_overlay(frame, proposal)
        overlay_path = sample_dir / "overlay-full.png"
        overlay.save(overlay_path, format="PNG")
        os.chmod(overlay_path, 0o600)
        preview_path = sample_dir / "overlay-preview.png"
        _resize_preview(overlay).save(preview_path, format="PNG")
        os.chmod(preview_path, 0o600)
        crop_files: dict[str, str] = {}
        crop_boxes: dict[str, list[int]] = {}
        crop_omissions: dict[str, str] = {}
        crop_targets, crop_omissions = field_crop_plan(proposal)
        for field, roi in crop_targets.items():
            box = _finite_rect(roi.get("bounds"), frame.width, frame.height)
            crop_box = centered_field_crop_bounds(box, frame.size, field, proposal)
            crop_path = sample_dir / f"{field}-centered-crop.png"
            frame.crop(crop_box).save(crop_path, format="PNG")
            os.chmod(crop_path, 0o600)
            crop_files[field] = crop_path.name
            crop_boxes[field] = list(crop_box)
        after = source.read_bytes()
        if sha256_bytes(after) != raster["sha256"]:
            raise PreviewError("source raster changed during preview generation")
        sample_entries.append({
            "sample": sample, "source_raster": str(source.relative_to(dataset_root)),
            "source_kind": item["source_kind"], "frame_id": raster["id"],
            "frame_dimensions": [frame.width, frame.height], "frame_sha256": raster["sha256"],
            "coordinate_frame": item["coordinate_frame"], "frame_transform": "EXIF transpose" if "EXIF-transposed" in item["source_kind"] else "direct canonical raster",
            "page_frame_kind": frame_request["page_frame_kind"],
            "source_immutable": True, "proposal_file": f"{sample}/proposal.json",
            "overlay_full": f"{sample}/overlay-full.png", "overlay_preview": f"{sample}/overlay-preview.png",
            "centered_crops": crop_files, "crop_omissions": crop_omissions,
            "centered_crop_bounds": crop_boxes,
            "field_geometry_assessment": assess_field_split(proposal["series"], proposal["number"]),
            "proposal": _proposal_summary(proposal),
        })
    if _sha_file(binary_path) != binary_hash_before:
        raise PreviewError("Go CLI binary changed during preview generation")
    source_hashes_after = {name: _sha_file(path) for name, path in tracked_sources.items()}
    if source_hashes_after != source_hashes_before:
        raise PreviewError("renderer, test or Go locator source changed during preview generation")
    out_manifest = {"schema": MANIFEST_SCHEMA, "go_binary_sha256": binary_hash_before,
                    "source_sha256": source_hashes_before,
                    "no_ocr_or_model_calls": True, "all_outputs_require_manual_review": True,
                    "samples": sample_entries}
    write_private(run_dir / "manifest.json", json_bytes(out_manifest))
    return run_dir


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    repo = Path(__file__).resolve().parents[2]
    dataset = repo / ".local/ocr-research/datasets/current-five"
    parser.add_argument("--manifest", type=Path, default=dataset / "diagnostics/20261030T054331Z-failure-trace/visualizations/manifest.json")
    parser.add_argument("--dataset-root", type=Path, default=dataset)
    parser.add_argument("--binary", type=Path, default=repo / ".local/ocr-research/bin/serial-roi-preview")
    parser.add_argument("--output-root", type=Path, default=dataset / "serial-roi-previews")
    args = parser.parse_args(argv)
    try:
        result = run(args.manifest, args.binary, args.dataset_root, args.output_root)
    except (PreviewError, OSError, ValueError, KeyError, json.JSONDecodeError, subprocess.CalledProcessError) as exc:
        print(f"serial ROI preview: {exc}", file=sys.stderr)
        return 2
    print(result)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
