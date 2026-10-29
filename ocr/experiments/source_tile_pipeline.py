"""Private, opt-in source-frame OCR tiles for registration fallback experiments.

Tiles are unscaled crops from the EXIF-normalized original raster. Their OCR
quads are mapped back to that same source frame and are carried as observations
v3 ``source_tile`` passes. This module does not score or select field values.
"""
from __future__ import annotations

import copy
import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image, ImageOps


SCHEMA_VERSION = "passport-ocr-observations/v3"
SOURCE_FRAME = "exif_normalized_pixels"
MAX_TILE_SIDE = 2000
MAX_TILES = 6
OVERLAP_FRACTION = 0.20
TILE_MULTIPLE = 32
CPU_PROVIDER = "CPUExecutionProvider"


class SourceTileError(RuntimeError):
    """A coded error without image text, source paths, or PII."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _round_up_multiple(value: float, multiple: int = TILE_MULTIPLE) -> int:
    return int(math.ceil(value / multiple) * multiple)


def _axis_tile_plan(length: int, count: int) -> tuple[int, list[int]]:
    if length < 1 or count < 1:
        raise SourceTileError("tile_dimension_invalid")
    denominator = count - (count - 1) * OVERLAP_FRACTION
    tile_length = min(length, _round_up_multiple(length / denominator))
    if tile_length > MAX_TILE_SIDE:
        raise SourceTileError("tile_side_limit_exceeded")
    if count == 1:
        starts = [0]
    else:
        last_start = length - tile_length
        starts = [int(round(index * last_start / (count - 1))) for index in range(count)]
    return tile_length, starts


def plan_source_tiles(width: int, height: int) -> dict[str, Any]:
    """Return a deterministic <=6 tile grid covering the complete source raster."""
    if width < 1 or height < 1:
        raise SourceTileError("image_dimensions_invalid")
    def candidates(length: int) -> list[tuple[int, int, list[int]]]:
        options = []
        for count in range(1, MAX_TILES + 1):
            try:
                tile_length, starts = _axis_tile_plan(length, count)
            except SourceTileError:
                continue
            options.append((count, tile_length, starts))
        return options

    x_candidates, y_candidates = candidates(width), candidates(height)
    grids = [
        (columns * rows, abs(tile_width - tile_height), columns, rows, tile_width, x_starts, tile_height, y_starts)
        for columns, tile_width, x_starts in x_candidates
        for rows, tile_height, y_starts in y_candidates
        if columns * rows <= MAX_TILES
    ]
    if not grids:
        raise SourceTileError("tile_count_limit_exceeded")
    _, _, columns, rows, tile_width, x_starts, tile_height, y_starts = min(grids)
    tiles = []
    for row, y0 in enumerate(y_starts):
        for column, x0 in enumerate(x_starts):
            x1, y1 = min(width, x0 + tile_width), min(height, y0 + tile_height)
            tiles.append({
                "id": f"source_tile_{len(tiles) + 1:02d}",
                "row": row,
                "column": column,
                "crop": [x0, y0, x1, y1],
                "input_size_pixels": [x1 - x0, y1 - y0],
            })
    return {
        "policy": "source_full_resolution_grid_v1",
        "coordinate_frame": SOURCE_FRAME,
        "source_size_pixels": [width, height],
        "grid": {"columns": columns, "rows": rows},
        "overlap_fraction_target": OVERLAP_FRACTION,
        "max_tile_side": MAX_TILE_SIDE,
        "max_tiles": MAX_TILES,
        "tiles": tiles,
    }


def _is_derived_registered_frame(observations: dict[str, Any]) -> bool:
    runtime = observations.get("runtime")
    if not isinstance(runtime, dict):
        return False
    registered = runtime.get("registered_spread")
    if isinstance(registered, dict) and registered:
        return True
    return runtime.get("coordinate_space_role") == "derived_registered_spread"


def _validate_source_identity(
    image_path: Path, observations: dict[str, Any], raw_bytes: bytes, width: int, height: int,
) -> None:
    if not isinstance(observations, dict) or observations.get("schema_version") != "passport-ocr-observations/v2":
        raise SourceTileError("source_tile_requires_observations_v2")
    image = observations.get("image")
    if not isinstance(image, dict):
        raise SourceTileError("source_image_metadata_missing")
    if image.get("coordinate_frame") != SOURCE_FRAME:
        raise SourceTileError("source_coordinate_frame_mismatch")
    if image.get("sha256") != sha256_bytes(raw_bytes):
        raise SourceTileError("source_image_sha256_mismatch")
    if image.get("width") != width or image.get("height") != height:
        raise SourceTileError("source_image_dimensions_mismatch")
    if _is_derived_registered_frame(observations):
        raise SourceTileError("derived_registered_frame_not_supported")
    if not isinstance(image.get("id"), str) or not image.get("id"):
        raise SourceTileError("source_image_id_missing")
    passes = observations.get("passes")
    lines = observations.get("lines")
    if not isinstance(passes, list) or not isinstance(lines, list):
        raise SourceTileError("source_observations_invalid")
    pass_ids = {p.get("id") for p in passes if isinstance(p, dict)}
    if any(not isinstance(line, dict) or line.get("pass_id") not in pass_ids for line in lines):
        raise SourceTileError("source_observations_invalid")
    source_pass = next((p for p in passes if isinstance(p, dict) and p.get("id") == "original"), None)
    if not source_pass or source_pass.get("crop") != [0, 0, width, height] or source_pass.get("rotation_degrees") != 0:
        raise SourceTileError("source_original_pass_missing")


def _private_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("x", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, separators=(",", ":"))
        stream.write("\n")
    os.replace(temporary, path)
    os.chmod(path, 0o600)


def augment_source_tiles(
    image_path: str | Path,
    baseline_observations: dict[str, Any],
    detector: str | Path,
    recognizer: str | Path,
    *,
    private_output_dir: str | Path,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Append bounded source_tile passes without changing baseline evidence.

    Reader/model/provider failures are recorded as empty error passes. Identity,
    frame, and engine-profile failures abort augmentation before returning an
    augmented envelope. The caller can retain the untouched baseline then.
    """
    path = Path(image_path)
    raw_bytes = path.read_bytes()
    with Image.open(path) as opened:
        normalized = ImageOps.exif_transpose(opened).convert("RGB")
        rgb = np.asarray(normalized)
    height, width = rgb.shape[:2]
    _validate_source_identity(path, baseline_observations, raw_bytes, width, height)
    tiles = plan_source_tiles(width, height)

    runtime_models = baseline_observations.get("runtime", {}).get("model_sha256", {})
    if not isinstance(runtime_models, dict):
        raise SourceTileError("baseline_model_hashes_missing")
    detector_sha = sha256_file(detector)
    recognizer_sha = sha256_file(recognizer)
    if detector_sha != runtime_models.get("detector"):
        raise SourceTileError("detector_model_sha256_mismatch")
    if recognizer_sha != runtime_models.get("recognizer"):
        raise SourceTileError("recognizer_model_sha256_mismatch")

    # No second view is useful when the existing full-frame pass already stays
    # within RapidOCR's global max-side and detector min-side resize thresholds.
    if max(width, height) <= MAX_TILE_SIDE and min(width, height) >= 736:
        plan = {
            "schema_version": "passport-source-tile-plan/v1",
            "image": copy.deepcopy(baseline_observations["image"]),
            "status": "no_op",
            "reason": "source_within_detector_resize_bounds",
            "profile": tiles,
        }
        _private_write_json(Path(private_output_dir) / "source-tile-plan.json", plan)
        return copy.deepcopy(baseline_observations), plan

    from ocr.experiments import region_pipeline

    engine = region_pipeline.create_cpu_engine(detector, recognizer)
    detector_profile = region_pipeline._engine_detector_config(engine)
    baseline_profile = baseline_observations.get("runtime", {}).get("detector_profiles", {}).get("primary", {})
    if not isinstance(detector_profile, dict) or not isinstance(baseline_profile, dict):
        raise SourceTileError("detector_profile_unavailable")
    for key in ("limit_side_len", "limit_type", "global_min_side_len", "global_max_side_len",
                "global_use_preprocess_img", "global_use_vertical_padding"):
        if detector_profile.get(key) is None or detector_profile.get(key) != baseline_profile.get(key):
            raise SourceTileError("detector_profile_mismatch")

    augmented = copy.deepcopy(baseline_observations)
    augmented["schema_version"] = SCHEMA_VERSION
    augmented.setdefault("passes", [])
    augmented.setdefault("lines", [])
    runtime = augmented.setdefault("runtime", {})
    pass_profiles = runtime.setdefault("pass_detector_profiles", {})
    tile_results = []
    for tile in tiles["tiles"]:
        crop = tuple(tile["crop"])
        pass_id = tile["id"]
        record, lines = region_pipeline._run_pass(
            engine, rgb, pass_id, "source_tile", crop, 0,
        )
        x0, y0, x1, y1 = crop
        line_geometry_valid = all(
            all(
                math.isfinite(float(point[0])) and math.isfinite(float(point[1]))
                and x0 <= float(point[0]) <= x1 and y0 <= float(point[1]) <= y1
                for point in line.get("quad", [])
            )
            and len(line.get("quad", [])) == 4
            for line in lines
        )
        report = region_pipeline.cpu_provider_report(engine)
        sessions = report.get("sessions", {}) if isinstance(report, dict) else {}
        detector_providers = sessions.get("det") if isinstance(sessions, dict) else None
        recognizer_providers = sessions.get("rec") if isinstance(sessions, dict) else None
        cpu_verified = detector_providers == [CPU_PROVIDER] and recognizer_providers == [CPU_PROVIDER]
        if not cpu_verified or not line_geometry_valid:
            record["status"] = "error"
            lines = []
        augmented["passes"].append(record)
        augmented["lines"].extend(lines)
        pass_profiles[pass_id] = "primary"
        tile_results.append({
            **tile,
            "status": record.get("status"),
            "line_count": len(lines),
            "providers": {"det": detector_providers, "rec": recognizer_providers},
            "cpu_verified": cpu_verified,
            "line_geometry_valid": line_geometry_valid,
            "detector_profile": copy.deepcopy(detector_profile),
        })

    profile = {
        **tiles,
        "reader": "rapidocr",
        "reader_mode": "det_rec",
        "provider_policy": "actual_detector_and_recognizer_sessions_cpu",
        "detector_profile": copy.deepcopy(detector_profile),
        "model_sha256": {"detector": detector_sha, "recognizer": recognizer_sha},
        "passes": tile_results,
    }
    runtime["source_tile_profile"] = profile
    plan = {
        "schema_version": "passport-source-tile-plan/v1",
        "image": copy.deepcopy(baseline_observations["image"]),
        "profile": profile,
    }
    _private_write_json(Path(private_output_dir) / "source-tile-plan.json", plan)
    return augmented, plan
