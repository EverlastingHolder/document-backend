#!/usr/bin/env python3
"""Private newline-JSON worker for the RussianDocsOCR API provider."""

from __future__ import annotations

import argparse
import contextlib
import json
import os
import re
import stat
import sys
from pathlib import Path

SCHEMA = "passport-russiandocsocr-worker/v1"
FIELDS = (
    "surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
    "series", "number", "issue_date", "issuer", "department_code",
)
SUPPORTED_DOCTYPES = frozenset({"INTPASSPORT_1997", "INTPASSPORT_2011"})
IDENTIFIER = re.compile(r"^[0-9a-fA-F-]{36}$")
MAX_LINE_BYTES = 2 << 20


def emit(output, value: dict) -> None:
    output.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n")
    output.flush()


def split_passport_number(value: object) -> tuple[str | None, str | None]:
    if not isinstance(value, str) or not re.fullmatch(r"[0-9 -]+", value):
        return None, None
    digits = value.replace(" ", "").replace("-", "")
    if len(digits) != 10:
        return None, None
    return digits[:4], digits[4:]


def adapt(source: object) -> dict[str, str | None]:
    values = source if isinstance(source, dict) else {}

    def text(name: str) -> str | None:
        value = values.get(name)
        return value if isinstance(value, str) else None

    series, number = split_passport_number(values.get("Licence_number"))
    return {
        "surname": text("Last_name_ru"),
        "given_name": text("First_name_ru"),
        "patronymic": text("Middle_name_ru"),
        "gender": text("Sex_ru"),
        "birth_date": text("Birth_date"),
        "birth_place": text("Birth_place_ru"),
        "series": series,
        "number": number,
        "issue_date": text("Issue_date"),
        "issuer": text("Issue_organization_ru"),
        "department_code": text("Issue_organisation_code"),
    }


def supported_passport_doctype(value: object) -> bool:
    return isinstance(value, str) and value in SUPPORTED_DOCTYPES


def validate_image(path: Path, work_root: Path, media_type: str, max_bytes: int, max_pixels: int) -> Path:
    try:
        resolved = path.resolve(strict=True)
        resolved.relative_to(work_root)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size <= 0 or info.st_size > max_bytes:
            raise ValueError("invalid_image")
        from PIL import Image

        with Image.open(resolved) as image:
            expected = {"image/jpeg": "JPEG", "image/png": "PNG", "image/webp": "WEBP"}.get(media_type)
            if expected is None or image.format != expected or image.width * image.height > max_pixels:
                raise ValueError("invalid_image")
            image.verify()
    except ValueError:
        raise
    except Exception as error:
        raise ValueError("invalid_image") from error
    return resolved


def load_pipeline(repo: Path, models_root: Path):
    repo = repo.resolve(strict=True)
    models_root = models_root.resolve(strict=True)
    sys.path.insert(0, str(repo))
    from document_processing import Pipeline
    from document_processing.config import DEFAULT_CFG, ROOT

    defaults_root = Path(ROOT) / "models"
    for key, value in tuple(DEFAULT_CFG.items()):
        default_path = Path(value).resolve()
        try:
            relative = default_path.relative_to(defaults_root.resolve())
        except ValueError as error:
            raise RuntimeError("model config path escaped repository") from error
        DEFAULT_CFG[key] = str(models_root / relative)
        if not (models_root / relative).is_dir():
            raise RuntimeError("model directory unavailable")
    return Pipeline(device="cpu", ocr="accurate")


def handle(pipeline, request: dict, work_root: Path, max_input_bytes: int, max_pixels: int) -> dict:
    request_id = request.get("request_id")
    image_id = request.get("image_id")
    if not isinstance(request_id, str) or not IDENTIFIER.fullmatch(request_id):
        raise ValueError("invalid_image")
    if not isinstance(image_id, str) or not IDENTIFIER.fullmatch(image_id):
        raise ValueError("invalid_image")
    if request.get("schema_version") != SCHEMA or request.get("type") != "recognize":
        raise ValueError("invalid_image")
    if not isinstance(request.get("image_path"), str) or not isinstance(request.get("media_type"), str):
        raise ValueError("invalid_image")
    image_path = validate_image(Path(request["image_path"]), work_root, request["media_type"], max_input_bytes, max_pixels)
    result = pipeline.process_img(str(image_path))
    if not supported_passport_doctype(result.doctype):
        raise ValueError("invalid_image")
    return {
        "schema_version": SCHEMA,
        "type": "result",
        "request_id": request_id,
        "image_id": image_id,
        "status": "completed",
        "code": "ok",
        "fields": adapt(result.ocr),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", required=True, type=Path)
    parser.add_argument("--models-root", required=True, type=Path)
    parser.add_argument("--work-root", required=True, type=Path)
    parser.add_argument("--max-input-bytes", required=True, type=int)
    parser.add_argument("--max-image-pixels", required=True, type=int)
    args = parser.parse_args()
    work_root = args.work_root.resolve(strict=True)
    protocol_out = sys.stdout
    try:
        with contextlib.redirect_stdout(sys.stderr):
            pipeline = load_pipeline(args.repo, args.models_root)
    except Exception:
        return 2
    emit(protocol_out, {"schema_version": SCHEMA, "type": "ready"})
    for line in sys.stdin.buffer:
        if len(line) > MAX_LINE_BYTES or not line.endswith(b"\n"):
            return 3
        request = {}
        try:
            request = json.loads(line)
            if not isinstance(request, dict):
                raise ValueError("invalid_image")
            with contextlib.redirect_stdout(sys.stderr):
                response = handle(pipeline, request, work_root, args.max_input_bytes, args.max_image_pixels)
        except ValueError as error:
            code = "invalid_image" if str(error) == "invalid_image" else "pipeline_failed"
            response = {
                "schema_version": SCHEMA,
                "type": "result",
                "request_id": request.get("request_id") if isinstance(request, dict) else "",
                "image_id": request.get("image_id") if isinstance(request, dict) else "",
                "status": "failed",
                "code": code,
            }
        except Exception:
            response = {
                "schema_version": SCHEMA,
                "type": "result",
                "request_id": request.get("request_id") if isinstance(request, dict) else "",
                "image_id": request.get("image_id") if isinstance(request, dict) else "",
                "status": "failed",
                "code": "pipeline_failed",
            }
        encoded = json.dumps(response, ensure_ascii=False, separators=(",", ":"))
        if len(encoded.encode("utf-8")) > MAX_LINE_BYTES:
            return 4
        emit(protocol_out, response)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
