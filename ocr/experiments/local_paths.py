"""Repository-relative paths for local OCR research data and model caches."""
from __future__ import annotations

import os
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


def _env_path(name: str, default: Path) -> Path:
    value = os.environ.get(name)
    return Path(value).expanduser().resolve() if value else default


PRIVATE_RESEARCH_ROOT = _env_path(
    "PASSPORT_OCR_RESEARCH_ROOT", REPO_ROOT / ".local" / "ocr-research"
)
ARCHIVE_WORK_ROOT = PRIVATE_RESEARCH_ROOT / "work"
WORK_ROOT = ARCHIVE_WORK_ROOT
ARCHIVE_OUTPUT_ROOT = PRIVATE_RESEARCH_ROOT / "outputs"
VL_MODEL_DIR = _env_path(
    "PASSPORT_VL_MODEL_DIR",
    ARCHIVE_WORK_ROOT
    / "ocr-vl-work"
    / "private"
    / "model-cache"
    / "paddlex"
    / "official_models"
    / "PaddleOCR-VL-1.6",
)
RAPIDOCR_MODEL_DIR = _env_path(
    "PASSPORT_RAPIDOCR_MODEL_DIR",
    REPO_ROOT / "ocr" / "experiments" / "models",
)
