"""Geometry-only MRZ crop localization and optional Tesseract OCR helpers.

This module does not parse MRZ fields, validate check digits, or repair OCR
characters. Candidate text is used only as a conservative crop anchor.
"""

from __future__ import annotations

import csv
import math
import os
import re
import shutil
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image


_COMPACT = re.compile(r"\s+")
_MRZ_CHARS = set("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789<")
_TSV_COLUMNS = (
    "level", "page_num", "block_num", "par_num", "line_num", "word_num",
    "left", "top", "width", "height", "conf", "text",
)
_TESSERACT_WHITELIST = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789<"
_TESSERACT_SCALE = 2


def _quad(line: dict[str, Any]) -> np.ndarray | None:
    try:
        points = np.asarray(line.get("quad"), dtype=np.float64).reshape(4, 2)
    except (TypeError, ValueError):
        return None
    return points if np.isfinite(points).all() else None


def _compact(text: Any) -> str:
    return _COMPACT.sub("", str(text)).upper()


def _rect_union(lines: list[dict[str, Any]]) -> tuple[float, float, float, float] | None:
    points = [_quad(line) for line in lines]
    valid = [point for point in points if point is not None]
    if not valid:
        return None
    joined = np.vstack(valid)
    return (
        float(np.min(joined[:, 0])), float(np.min(joined[:, 1])),
        float(np.max(joined[:, 0])), float(np.max(joined[:, 1])),
    )


def _candidate_pair(
    lines: list[dict[str, Any]], anchor_bounds: tuple[int, int, int, int]
) -> list[dict[str, Any]] | None:
    """Choose two wide, same-pass, lower-zone strings as a crop anchor."""
    x0, y0, x1, y1 = anchor_bounds
    width, height = x1 - x0, y1 - y0
    if width <= 0 or height <= 0:
        return None

    grouped: dict[str, list[tuple[float, dict[str, Any], np.ndarray, str]]] = {}
    for line in lines:
        points = _quad(line)
        if points is None:
            continue
        text = _compact(line.get("text_raw", ""))
        if len(text) < 20:
            continue
        # Unicode letters can anchor a crop, but are never mapped to ASCII or
        # digits here. Punctuation outside the MRZ alphabet is not an anchor.
        if any(char not in _MRZ_CHARS and not char.isalpha() for char in text):
            continue
        bx0, by0 = np.min(points, axis=0)
        bx1, by1 = np.max(points, axis=0)
        line_width = float(bx1 - bx0)
        center_y = float(np.mean(points[:, 1]))
        if line_width < 0.30 * width or center_y < y0 + 0.52 * height:
            continue
        pass_id = str(line.get("pass_id", ""))
        grouped.setdefault(pass_id, []).append((center_y, line, points, text))

    for entries in grouped.values():
        entries.sort(key=lambda item: item[0])
        for index in range(len(entries) - 1):
            top, bottom = entries[index], entries[index + 1]
            if top[0] >= bottom[0]:
                continue
            top_box = (float(np.min(top[2][:, 0])), float(np.max(top[2][:, 0])))
            bottom_box = (float(np.min(bottom[2][:, 0])), float(np.max(bottom[2][:, 0])))
            overlap = max(0.0, min(top_box[1], bottom_box[1]) - max(top_box[0], bottom_box[0]))
            min_width = max(1.0, min(top_box[1] - top_box[0], bottom_box[1] - bottom_box[0]))
            heights = [
                max(1.0, float(np.max(item[2][:, 1]) - np.min(item[2][:, 1])))
                for item in (top, bottom)
            ]
            separation = bottom[0] - top[0]
            if overlap / min_width < 0.45 or not 0.6 * max(heights) <= separation <= 9.0 * max(heights):
                continue
            # At least one additional national-format cue makes a coincidental
            # pair of long body-text rows less likely to anchor the ROI.
            if not ("<" in top[3] or "<" in bottom[3] or "PNRUS" in top[3] or "PNRUS" in bottom[3]):
                continue
            return [top[1], bottom[1]]
    return None


def locate_mrz_roi(
    *,
    image_size: tuple[int, int],
    document_bounds: tuple[int, int, int, int],
    lines: list[dict[str, Any]],
    document_method: str,
    lower_page: tuple[int, int, int, int] | None = None,
) -> tuple[int, int, int, int]:
    """Return an anchored MRZ union crop, or a cautious relative lower band.

    For OCR-envelope/full-frame fallbacks, candidate anchors are evaluated in
    full-image coordinates and an unanchored fallback uses a broad full-frame
    lower band. This avoids treating an OCR envelope as a physical page.
    """
    width, height = image_size
    full = (0, 0, width, height)
    method = str(document_method)
    if method in {"ocr_bbox_envelope", "full_fallback"}:
        anchor_bounds = full
    else:
        anchor_bounds = document_bounds
        if lower_page is not None:
            anchor_bounds = lower_page

    pair = _candidate_pair(lines, anchor_bounds)
    if pair:
        union = _rect_union(pair)
        if union is not None:
            ux0, uy0, ux1, uy1 = union
            source_bounds = anchor_bounds
            sx0, sy0, sx1, sy1 = source_bounds
            span_w, span_h = max(1, sx1 - sx0), max(1, sy1 - sy0)
            pad_x = max(8, int(round(span_w * 0.025)))
            row_height = max(1.0, uy1 - uy0)
            pad_y = max(5, int(round(row_height * 0.45)))
            return (
                max(0, int(math.floor(ux0)) - pad_x),
                max(0, int(math.floor(uy0)) - pad_y),
                min(width, int(math.ceil(ux1)) + pad_x),
                min(height, int(math.ceil(uy1)) + pad_y),
            )

    if method in {"ocr_bbox_envelope", "full_fallback"}:
        base = full
        fraction = 0.42
    elif lower_page is not None:
        base = lower_page
        fraction = 0.48
    else:
        base = document_bounds
        fraction = 0.34
    x0, y0, x1, y1 = base
    band_y = y0 + int(round((y1 - y0) * (1.0 - fraction)))
    return (max(0, x0), max(0, band_y), min(width, x1), min(height, y1))


def find_tesseract(explicit_path: str | Path | None = None) -> str | None:
    """Resolve an explicitly supplied binary or an already-installed binary."""
    if explicit_path is not None:
        candidate = Path(explicit_path).expanduser()
        return str(candidate) if candidate.is_file() else None
    return shutil.which("tesseract")


def parse_tesseract_tsv(stdout: str) -> list[dict[str, Any]]:
    """Parse Tesseract's 12-column TSV without interpreting text as CSV quotes."""
    reader = csv.reader(stdout.splitlines(), delimiter="\t", quoting=csv.QUOTE_NONE, strict=True)
    try:
        header = tuple(next(reader))
    except (StopIteration, csv.Error) as error:
        raise ValueError("tesseract_tsv_invalid") from error
    if header != _TSV_COLUMNS:
        raise ValueError("tesseract_tsv_invalid")

    rows: list[dict[str, Any]] = []
    try:
        for row in reader:
            if len(row) != len(_TSV_COLUMNS):
                raise ValueError("tesseract_tsv_invalid")
            if row[0] != "5" or not row[11]:
                continue
            try:
                left, top, width, height = map(int, row[6:10])
                confidence = float(row[10])
            except ValueError as error:
                raise ValueError("tesseract_tsv_invalid") from error
            if width <= 0 or height <= 0 or not math.isfinite(confidence):
                continue
            rows.append({
                "text_raw": row[11],
                "score": max(0.0, min(1.0, confidence / 100.0)),
                "quad": [[left, top], [left + width, top], [left + width, top + height], [left, top + height]],
            })
    except csv.Error as error:
        raise ValueError("tesseract_tsv_invalid") from error
    return rows


def run_tesseract_crop(
    executable: str,
    rgb: np.ndarray,
    *,
    timeout_seconds: float = 12.0,
) -> tuple[list[dict[str, Any]], str | None, float]:
    """Run installed English Tesseract on a private, bounded temporary crop."""
    started = time.perf_counter()
    with tempfile.TemporaryDirectory(prefix="passport-mrz-") as directory:
        private_dir = Path(directory)
        private_dir.chmod(0o700)
        input_path = private_dir / "mrz.png"
        source = Image.fromarray(rgb)
        scaled = source.resize(
            (source.width * _TESSERACT_SCALE, source.height * _TESSERACT_SCALE),
            Image.Resampling.BICUBIC,
        )
        scaled.save(input_path, format="PNG")
        input_path.chmod(0o600)
        command = [
            executable, str(input_path), "stdout", "--psm", "6", "-l", "eng",
            "-c", f"tessedit_char_whitelist={_TESSERACT_WHITELIST}",
            "-c", "load_system_dawg=0", "-c", "load_freq_dawg=0", "tsv",
        ]
        child_env = os.environ.copy()
        child_env.pop("TESSDATA_PREFIX", None)
        try:
            completed = subprocess.run(
                command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, text=True, encoding="utf-8", errors="replace",
                timeout=timeout_seconds, check=False, shell=False, env=child_env,
            )
        except subprocess.TimeoutExpired:
            return [], "tesseract_timeout", time.perf_counter() - started
        except OSError:
            return [], "tesseract_unavailable", time.perf_counter() - started
        if completed.returncode != 0:
            return [], "tesseract_failed", time.perf_counter() - started
        try:
            rows = parse_tesseract_tsv(completed.stdout)
        except ValueError:
            return [], "tesseract_tsv_invalid", time.perf_counter() - started
        for row in rows:
            row["quad"] = [
                [round(float(x) / _TESSERACT_SCALE, 3), round(float(y) / _TESSERACT_SCALE, 3)]
                for x, y in row["quad"]
            ]
        return rows, None, time.perf_counter() - started
