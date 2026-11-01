#!/usr/bin/env python3
"""Private newline-JSON worker for the RussianDocsOCR API provider."""

from __future__ import annotations

import argparse
import ast
import contextlib
import ctypes
import hashlib
import json
import os
import re
import stat
import sys
import signal
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


def validate_image(path: Path, image_root: Path, media_type: str, max_bytes: int, max_pixels: int) -> Path:
    try:
        resolved = path.resolve(strict=True)
        resolved.relative_to(image_root)
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


def validate_pipeline_snapshot(repo: Path, models_root: Path, expected_upstream_version: str,
                               expected_model_lock_version: str) -> None:
    """Validate the selected source and model snapshot without importing OCR code."""
    repo = repo.resolve(strict=True)
    models_root = models_root.resolve(strict=True)
    init_path = repo / "document_processing" / "__init__.py"
    try:
        syntax = ast.parse(init_path.read_text(encoding="utf-8"), filename="__init__.py")
        runtime_version = None
        for node in syntax.body:
            if isinstance(node, ast.Assign) and any(
                isinstance(target, ast.Name) and target.id == "__version__" for target in node.targets
            ):
                runtime_version = ast.literal_eval(node.value)
                break
            if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.target.id == "__version__":
                runtime_version = ast.literal_eval(node.value)
                break
    except Exception as error:
        raise RuntimeError("RussianDocsOCR source version unavailable") from error
    if not isinstance(runtime_version, str) or runtime_version != expected_upstream_version:
        raise RuntimeError("RussianDocsOCR source version mismatch")
    try:
        lock = json.loads((repo / "document_processing" / "models.lock.json").read_text(encoding="utf-8"))
    except Exception as error:
        raise RuntimeError("model lock unavailable") from error
    if not isinstance(lock, dict) or lock.get("models_version") != expected_model_lock_version:
        raise RuntimeError("RussianDocsOCR model version mismatch")
    files = lock.get("files")
    if not isinstance(files, list) or not files:
        raise RuntimeError("model lock invalid")
    for asset in files:
        if not isinstance(asset, dict):
            raise RuntimeError("model lock invalid")
        relative = asset.get("path")
        expected_size = asset.get("size")
        expected_hash = asset.get("sha256")
        if (not isinstance(relative, str) or not relative or Path(relative).is_absolute() or
                ".." in Path(relative).parts or isinstance(expected_size, bool) or
                not isinstance(expected_size, int) or expected_size <= 0 or
                not isinstance(expected_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", expected_hash)):
            raise RuntimeError("model lock invalid")
        model_path = models_root / relative
        try:
            info = model_path.lstat()
            resolved_model = model_path.resolve(strict=True)
            resolved_model.relative_to(models_root)
            if not stat.S_ISREG(info.st_mode) or info.st_size != expected_size:
                raise RuntimeError("model asset mismatch")
            digest = hashlib.sha256()
            with model_path.open("rb") as model_file:
                for chunk in iter(lambda: model_file.read(1 << 20), b""):
                    digest.update(chunk)
            if digest.hexdigest() != expected_hash:
                raise RuntimeError("model asset mismatch")
        except RuntimeError:
            raise
        except Exception as error:
            raise RuntimeError("model asset unavailable") from error


def load_pipeline(repo: Path, models_root: Path, expected_upstream_version: str | None = None,
                  expected_model_lock_version: str | None = None):
    repo = repo.resolve(strict=True)
    models_root = models_root.resolve(strict=True)
    sys.path.insert(0, str(repo))
    from document_processing import Pipeline
    from document_processing import __version__ as runtime_version
    from document_processing.config import DEFAULT_CFG, ROOT

    if expected_upstream_version is not None and runtime_version != expected_upstream_version:
        raise RuntimeError("RussianDocsOCR source version mismatch")
    if expected_model_lock_version is not None:
        validate_pipeline_snapshot(repo, models_root, expected_upstream_version or runtime_version,
                                   expected_model_lock_version)

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


def install_parent_death_signal(expected_parent_pid: int) -> None:
    """On Linux, ensure a killed async consumer cannot orphan the model child."""
    if not isinstance(expected_parent_pid, int) or expected_parent_pid <= 1:
        raise RuntimeError("invalid consumer parent pid")
    if not sys.platform.startswith("linux"):
        raise RuntimeError("parent death signal requires Linux")
    libc = ctypes.CDLL(None, use_errno=True)
    # PR_SET_PDEATHSIG=1. Configure this inside the child, never in Popen's
    # preexec_fn, because the consumer has other threads for Pika heartbeats.
    if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0:
        error = ctypes.get_errno()
        raise OSError(error, "prctl failed")
    # Close the race where the parent dies before prctl installs the signal.
    if os.getppid() != expected_parent_pid:
        os.kill(os.getpid(), signal.SIGKILL)


def handle(pipeline, request: dict, image_root: Path, max_input_bytes: int, max_pixels: int) -> dict:
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
    image_path = validate_image(Path(request["image_path"]), image_root, request["media_type"], max_input_bytes, max_pixels)
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
    parser.add_argument("--image-root", type=Path)
    parser.add_argument("--max-input-bytes", required=True, type=int)
    parser.add_argument("--max-image-pixels", required=True, type=int)
    parser.add_argument("--expected-russiandocs-version")
    parser.add_argument("--expected-models-version")
    parser.add_argument("--parent-pid", type=int)
    args = parser.parse_args()
    if args.parent_pid is not None:
        try:
            install_parent_death_signal(args.parent_pid)
        except Exception:
            return 2
    # Legacy synchronous callers keep the original workspace layout. The
    # asynchronous consumer supplies a separate, read-only image volume.
    image_root = (args.image_root or args.work_root).resolve(strict=True)
    protocol_out = sys.stdout
    try:
        with contextlib.redirect_stdout(sys.stderr):
            pipeline = load_pipeline(args.repo, args.models_root, args.expected_russiandocs_version,
                                     args.expected_models_version)
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
                response = handle(pipeline, request, image_root, args.max_input_bytes, args.max_image_pixels)
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
