"""Registration-first OCR handoff over a private canonical spread image.

The observations emitted here describe the saved, derived canonical raster.
Their pixel coordinates are never represented as coordinates in the source
photo. ``lineage`` records the per-panel mapping needed to backproject them.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
import time
from typing import Any, Callable

import cv2
import numpy as np
from PIL import Image, ImageOps

from ocr.experiments.page_registration import (
    RegistrationResult,
    RegisteredPage,
    register_spread,
    warp_registered_page,
)


SCHEMA_VERSION = "passport-ocr-observations/v1"
_PAGE_ORDER = ("image_upper", "image_lower")
_PASS_KIND = {"image_upper": "upper_page", "image_lower": "lower_page"}


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _private_directory(path: Path) -> None:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path, 0o700)


def _write_private_json(path: Path, value: dict[str, Any]) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    os.chmod(path, 0o600)


def _finite_quad(value: Any) -> np.ndarray | None:
    try:
        points = np.asarray(value, dtype=np.float64).reshape(4, 2)
    except (TypeError, ValueError):
        return None
    return points if np.isfinite(points).all() else None


def _contour_registration(registration: RegistrationResult) -> bool:
    """Fail closed unless the registration API explicitly reports contour provenance."""
    return (
        registration.status == "accepted"
        and registration.selected_candidate_source == "contour"
        and len(registration.pages) == 2
        and {page.name for page in registration.pages} == set(_PAGE_ORDER)
    )


def _canonical_panels(
    source_rgb: np.ndarray, pages: tuple[RegisteredPage, ...]
) -> tuple[np.ndarray, dict[str, dict[str, Any]]]:
    by_name = {page.name: page for page in pages}
    if set(by_name) != set(_PAGE_ORDER):
        raise ValueError("registered_page_set_invalid")
    rectified = {name: warp_registered_page(source_rgb, by_name[name]) for name in _PAGE_ORDER}
    target_width = max(int(rectified[name].shape[1]) for name in _PAGE_ORDER)
    panels: dict[str, np.ndarray] = {}
    mappings: dict[str, dict[str, Any]] = {}
    y_offset = 0
    for name in _PAGE_ORDER:
        page = by_name[name]
        panel = rectified[name]
        old_height, old_width = panel.shape[:2]
        target_height = max(1, int(round(old_height * target_width / old_width)))
        if (old_width, old_height) != (target_width, target_height):
            panel = cv2.resize(panel, (target_width, target_height), interpolation=cv2.INTER_LINEAR)
        scale_x = target_width / old_width
        scale_y = target_height / old_height
        panels[name] = panel

        # Invert OpenCV's half-pixel resize mapping and then undo the vertical
        # stack offset, yielding canonical-raster pixels -> page pixels.
        canonical_to_page = np.array([
            [1.0 / scale_x, 0.0, 0.5 / scale_x - 0.5],
            [0.0, 1.0 / scale_y, (0.5 - y_offset) / scale_y - 0.5],
            [0.0, 0.0, 1.0],
        ], dtype=np.float64)
        page_to_source = np.asarray(page.page_to_original, dtype=np.float64)
        if page_to_source.shape != (3, 3) or not np.isfinite(page_to_source).all():
            raise ValueError("registered_page_inverse_invalid")
        canonical_to_source = page_to_source @ canonical_to_page
        mappings[name] = {
            "physical_page": name,
            "canonical_rect": [0, y_offset, target_width, y_offset + target_height],
            "registered_page_size": [old_width, old_height],
            "canonical_size": [target_width, target_height],
            "resize_scale_xy": [scale_x, scale_y],
            "canonical_to_registered_page": canonical_to_page.tolist(),
            "registered_page_to_source": page_to_source.tolist(),
            "canonical_to_source": canonical_to_source.tolist(),
            "source_quad": page.source_quad,
        }
        y_offset += target_height
    canonical = np.concatenate([panels[name] for name in _PAGE_ORDER], axis=0)
    return canonical, mappings


def _engine_lines(result: Any, pass_id: str, y_offset: int, width: int, height: int) -> list[dict[str, Any]]:
    texts = getattr(result, "txts", None) or []
    boxes = getattr(result, "boxes", None)
    scores = getattr(result, "scores", None)
    if boxes is None:
        return []
    lines: list[dict[str, Any]] = []
    for index, text in enumerate(texts):
        if index >= len(boxes) or not isinstance(text, str) or not text.strip():
            continue
        points = _finite_quad(boxes[index])
        if points is None:
            continue
        if np.any(points[:, 0] < 0.0) or np.any(points[:, 0] > width):
            continue
        if np.any(points[:, 1] < 0.0) or np.any(points[:, 1] > height):
            continue
        try:
            score = float(scores[index]) if scores is not None and index < len(scores) else float("nan")
        except (TypeError, ValueError):
            continue
        if not math.isfinite(score) or not 0 <= score <= 1:
            continue
        points[:, 1] += y_offset
        lines.append({
            "id": f"{pass_id}-line-{index + 1}",
            "pass_id": pass_id,
            "text_raw": text,
            "score": score,
            "quad": points.tolist(),
        })
    return lines


def _validate_observations(observations: dict[str, Any]) -> None:
    """Validate the strict v1 envelope and the geometry Go consumes."""
    image = observations["image"]
    document = observations["document"]
    width, height = image["width"], image["height"]
    if observations.get("schema_version") != SCHEMA_VERSION:
        raise ValueError("observation_schema_invalid")
    if image.get("coordinate_frame") != "exif_normalized_pixels" or len(image.get("sha256", "")) != 64:
        raise ValueError("observation_image_invalid")
    if document != {
        "bounds": [0, 0, width, height],
        "method": "contour",
        "quality": "candidate",
        "seam_y": observations["runtime"]["registered_spread"]["seam_y"],
    }:
        raise ValueError("observation_document_invalid")
    pass_ids: set[str] = set()
    for item in observations["passes"]:
        if item["id"] in pass_ids or item["kind"] not in {"upper_page", "lower_page"}:
            raise ValueError("observation_pass_invalid")
        pass_ids.add(item["id"])
        if item["status"] not in {"ok", "error"} or len(item["crop"]) != 4:
            raise ValueError("observation_pass_invalid")
        if np.asarray(item["transform_to_image"]).shape != (3, 3):
            raise ValueError("observation_pass_transform_invalid")
    line_ids: set[str] = set()
    for line in observations["lines"]:
        points = _finite_quad(line.get("quad"))
        if (
            points is None
            or line.get("pass_id") not in pass_ids
            or line.get("id") in line_ids
            or not line.get("text_raw", "").strip()
            or not 0 <= line.get("score", -1) <= 1
            or np.any(points[:, 0] < 0)
            or np.any(points[:, 0] > width)
            or np.any(points[:, 1] < 0)
            or np.any(points[:, 1] > height)
        ):
            raise ValueError("observation_line_invalid")
        line_ids.add(line["id"])


def _lazy_engine(det_model: str | Path, rec_model: str | Path, engine_factory: Callable[..., Any] | None) -> Any:
    if engine_factory is not None:
        return engine_factory(det_model, rec_model)
    from ocr.experiments.region_pipeline import create_cpu_engine
    return create_cpu_engine(det_model, rec_model)


def _fallback(
    *, image_path: Path, image_id: str, det_model: str | Path, rec_model: str | Path,
    reason: str, engine: Any | None, engine_factory: Callable[..., Any] | None,
) -> dict[str, Any]:
    if engine is None and engine_factory is not None:
        engine = _lazy_engine(det_model, rec_model, engine_factory)
    from ocr.experiments.region_pipeline import build_observations
    observations = build_observations(
        image_path, image_id, det_model, rec_model, engine=engine, include_mrz=True,
    )
    return {
        "status": "fallback",
        "reason": reason,
        "observations": observations,
        "canonical_image_path": None,
        "lineage_path": None,
    }


def build_registered_observations(
    image_path: str | Path,
    image_id: str,
    det_model: str | Path,
    rec_model: str | Path,
    *,
    private_output_dir: str | Path,
    engine: Any | None = None,
    engine_factory: Callable[..., Any] | None = None,
    registration_fn: Callable[..., RegistrationResult] = register_spread,
    provider_validator: Callable[[Any], dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Register before OCR, then hand off only explicit contour-family pages.

    Rejected, ambiguous, or unsupported-source registrations use the current
    source-image region pipeline. No OCR engine is created before registration.
    """
    source_path = Path(image_path)
    output_dir = Path(private_output_dir)
    raw = source_path.read_bytes()
    with Image.open(source_path) as opened:
        source_rgb = np.asarray(ImageOps.exif_transpose(opened).convert("RGB"))
    source_height, source_width = source_rgb.shape[:2]
    source_sha = _sha256_bytes(raw)

    registration = registration_fn(source_rgb, text_quads=())
    if registration.status != "accepted":
        return _fallback(
            image_path=source_path, image_id=image_id, det_model=det_model, rec_model=rec_model,
            reason=f"registration_{registration.status}:{registration.reason}",
            engine=engine, engine_factory=engine_factory,
        )
    if not _contour_registration(registration):
        return _fallback(
            image_path=source_path, image_id=image_id, det_model=det_model, rec_model=rec_model,
            reason="registered_source_unsupported_or_unverified",
            engine=engine, engine_factory=engine_factory,
        )
    if registration.input_size != (source_width, source_height):
        return _fallback(
            image_path=source_path, image_id=image_id, det_model=det_model, rec_model=rec_model,
            reason="registration_source_size_mismatch", engine=engine, engine_factory=engine_factory,
        )

    _private_directory(output_dir)
    canonical_rgb, page_mappings = _canonical_panels(source_rgb, registration.pages)
    canonical_path = output_dir / "canonical-spread.png"
    Image.fromarray(canonical_rgb).save(canonical_path, format="PNG")
    os.chmod(canonical_path, 0o600)
    canonical_sha = _sha256_file(canonical_path)
    canonical_height, canonical_width = canonical_rgb.shape[:2]

    selected_engine = engine if engine is not None else _lazy_engine(det_model, rec_model, engine_factory)
    if provider_validator is None:
        from ocr.experiments.region_pipeline import _assert_cpu_only
        provider_validator = _assert_cpu_only
    provider_report: dict[str, Any] | None = None
    passes: list[dict[str, Any]] = []
    lines: list[dict[str, Any]] = []
    y_offset = 0
    by_name = {page.name: page for page in registration.pages}
    for page_name in _PAGE_ORDER:
        mapping = page_mappings[page_name]
        _, panel_y0, panel_x1, panel_y1 = mapping["canonical_rect"]
        panel_rgb = canonical_rgb[panel_y0:panel_y1, 0:panel_x1]
        pass_id = _PASS_KIND[page_name]
        started = time.perf_counter()
        try:
            result = selected_engine(cv2.cvtColor(panel_rgb, cv2.COLOR_RGB2BGR), use_det=True, use_cls=False, use_rec=True)
            elapsed = time.perf_counter() - started
            if provider_report is None:
                provider_report = provider_validator(selected_engine)
            page_lines = _engine_lines(result, pass_id, y_offset, canonical_width, panel_rgb.shape[0])
            status, issue = "ok", None
        except Exception:
            elapsed = time.perf_counter() - started
            page_lines, status, issue = [], "error", "registered_page_ocr_or_provider_validation_failed"
        pass_rect = [0, panel_y0, panel_x1, panel_y1]
        passes.append({
            "id": pass_id,
            "kind": _PASS_KIND[page_name],
            "crop": pass_rect,
            "rotation_degrees": 0,
            "transform_to_image": [[1, 0, 0], [0, 1, panel_y0], [0, 0, 1]],
            "elapsed_seconds": round(elapsed, 6),
            "status": status,
        })
        lines.extend(page_lines)
        y_offset = panel_y1
        if provider_report is None:
            break

    seam_y = float(page_mappings["image_lower"]["canonical_rect"][1])
    runtime: dict[str, Any] = {
        "model_sha256": {
            "detector": _sha256_file(Path(det_model)) if Path(det_model).is_file() else "unavailable",
            "recognizer": _sha256_file(Path(rec_model)) if Path(rec_model).is_file() else "unavailable",
        },
        "providers": provider_report,
        "registered_spread": {
            "status": registration.status,
            "reason": registration.reason,
            "selected_candidate_source": registration.selected_candidate_source,
            "orientation_status": registration.orientation_status,
            "source_image_sha256": source_sha,
            "source_image_size": [source_width, source_height],
            "canonical_image_sha256": canonical_sha,
            "canonical_image_size": [canonical_width, canonical_height],
            "coordinate_space_role": "derived_registered_spread",
            "source_coordinate_frame": "exif_normalized_pixels",
            "physical_page_order": list(_PAGE_ORDER),
            "seam_y": seam_y,
            "lineage_file": "registration-lineage.json",
            "page_mappings": page_mappings,
            "providers": provider_report,
        }
    }
    observations = {
        "schema_version": SCHEMA_VERSION,
        "image": {
            "id": f"{image_id}:registered-spread",
            "width": canonical_width,
            "height": canonical_height,
            "sha256": canonical_sha,
            "coordinate_frame": "exif_normalized_pixels",
        },
        "document": {
            "bounds": [0, 0, canonical_width, canonical_height],
            "method": "contour",
            "quality": "candidate",
            "seam_y": seam_y,
        },
        "passes": passes,
        "lines": lines,
        "runtime": runtime,
    }
    _validate_observations(observations)

    lineage = {
        "schema_version": "passport-registered-spread-lineage/v1",
        "source_image": {
            "id": image_id,
            "sha256": source_sha,
            "width": source_width,
            "height": source_height,
            "coordinate_frame": "exif_normalized_pixels",
        },
        "canonical_image": {
            "id": observations["image"]["id"],
            "sha256": canonical_sha,
            "width": canonical_width,
            "height": canonical_height,
            "coordinate_frame": "exif_normalized_pixels",
            "coordinate_space_role": "derived_registered_spread",
        },
        "registration": registration.summary(),
        "page_mappings": page_mappings,
    }
    lineage_path = output_dir / "registration-lineage.json"
    _write_private_json(lineage_path, lineage)
    observation_path = output_dir / "observations.json"
    _write_private_json(observation_path, observations)
    return {
        "status": "accepted" if all(item["status"] == "ok" for item in passes) else "partial_ocr_error",
        "reason": registration.reason,
        "observations": observations,
        "canonical_image_path": str(canonical_path),
        "lineage_path": str(lineage_path),
        "observations_path": str(observation_path),
    }
