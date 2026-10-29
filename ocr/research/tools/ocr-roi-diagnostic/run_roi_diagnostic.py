"""Run a sealed, manual-ROI diagnostic without reading any gold/reference data.

OCR text and Tesseract's TSV live only in the mode-0600 result JSON inside a
mode-0700 output directory. Console output contains task IDs and statuses only.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import math
import os
from pathlib import Path
import re
import signal
import shutil
import subprocess
import sys

_CODE_ROOT = Path(__file__).resolve().parents[4]
if str(_CODE_ROOT) not in sys.path:
    sys.path.insert(0, str(_CODE_ROOT))
from ocr.experiments.local_paths import (
    ARCHIVE_OUTPUT_ROOT,
    ARCHIVE_WORK_ROOT,
    PRIVATE_RESEARCH_ROOT,
    RAPIDOCR_MODEL_DIR,
    REPO_ROOT,
    VL_MODEL_DIR,
)
import time
import traceback
from typing import Any

# This diagnostic lives in the Codex task worktree; its read-only OCR helpers
# and pinned runtime live in the separate passport-backend checkout.
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import numpy as np
from PIL import Image, ImageOps

from ocr.experiments import field_roi_pipeline as roi
from ocr.experiments.template_field_pipeline import verify_template_cpu

SCHEMA = "passport-roi-diagnostic-crops/v1"
PURPOSE = "manual_visual_diagnostic_only"
EXPECTED_TASKS = 14
ALLOWED_FIELDS = {"issuer", "birth_place", "given_name", "gender"}
EXPECTED_TASKS_SET = {
    *((sample, field) for sample in ("sample01", "sample02", "sample03", "sample04", "sample05") for field in ("issuer", "birth_place")),
    *((sample, field) for sample in ("sample02", "sample04") for field in ("given_name", "gender")),
}
TESSERACT = Path(os.environ.get("PASSPORT_TESSERACT_BIN", shutil.which("tesseract") or "tesseract"))
TESSDATA_HASH = "e16e5e036cce1d9ec2b00063cf8b54472625b9e14d893a169e2b0dedeb4df225"
TESSERACT_TSV_COLUMNS = {
    "level", "page_num", "block_num", "par_num", "line_num", "word_num",
    "left", "top", "width", "height", "conf", "text",
}
CHILD_TIMEOUT = 360
TESS_TIMEOUT = 20
_SAFE_ID = re.compile(r"^[A-Za-z0-9_.-]{1,100}$")


class DiagnosticError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _sha_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _sha_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _private_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    path.write_text(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def _safe_id(value: Any) -> bool:
    return isinstance(value, str) and _SAFE_ID.fullmatch(value) is not None


def _load_manifest(path: Path) -> list[dict[str, Any]]:
    try:
        manifest = json.loads(path.read_text(encoding="utf-8"))
    except Exception as error:
        raise DiagnosticError("manifest_unreadable_or_invalid_json") from error
    if not isinstance(manifest, dict) or manifest.get("schema_version") != SCHEMA or manifest.get("purpose") != PURPOSE:
        raise DiagnosticError("manifest_schema_or_purpose_mismatch")
    crops = manifest.get("crops")
    if not isinstance(crops, list) or len(crops) != EXPECTED_TASKS:
        raise DiagnosticError("manifest_task_count_mismatch")
    seen: set[tuple[str, str]] = set()
    for item in crops:
        if not isinstance(item, dict):
            raise DiagnosticError("manifest_entry_invalid")
        sample_id, field_id = item.get("sample_id"), item.get("field_id")
        if not _safe_id(sample_id) or field_id not in ALLOWED_FIELDS:
            raise DiagnosticError("manifest_identity_invalid")
        key = (sample_id, field_id)
        if key in seen:
            raise DiagnosticError("manifest_task_duplicate")
        seen.add(key)
        if not isinstance(item.get("source_sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", item["source_sha256"]):
            raise DiagnosticError("manifest_source_sha_invalid")
        if not isinstance(item.get("crop_sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", item["crop_sha256"]):
            raise DiagnosticError("manifest_crop_sha_invalid")
        if not isinstance(item.get("geometry_provenance"), str) or not item["geometry_provenance"].strip():
            raise DiagnosticError("manifest_geometry_provenance_missing")
        for key_name in ("source_image_path", "crop_image_path"):
            if not isinstance(item.get(key_name), str) or not item[key_name]:
                raise DiagnosticError("manifest_image_path_missing")
        for key_name in ("source_width", "source_height"):
            value = item.get(key_name)
            if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
                raise DiagnosticError("manifest_source_dimensions_invalid")
        crop = item.get("crop")
        if not isinstance(crop, list) or len(crop) != 4:
            raise DiagnosticError("manifest_crop_geometry_invalid")
        for value in crop:
            if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                raise DiagnosticError("manifest_crop_geometry_invalid")
        x0, y0, x1, y1 = crop
        if x1 <= x0 or y1 <= y0:
            raise DiagnosticError("manifest_crop_geometry_invalid")
        if x1 > item["source_width"] or y1 > item["source_height"]:
            raise DiagnosticError("manifest_crop_out_of_bounds")
    if seen != EXPECTED_TASKS_SET:
        raise DiagnosticError("manifest_expected_task_set_mismatch")
    return crops


def _resolve_manifest_path(manifest_path: Path, value: str) -> Path:
    candidate = Path(value).expanduser()
    return candidate.resolve() if candidate.is_absolute() else (manifest_path.parent / candidate).resolve()


def _validate_input(item: dict[str, Any], manifest_path: Path) -> tuple[Path, Path, np.ndarray, dict[str, Any]]:
    source_path = _resolve_manifest_path(manifest_path, item["source_image_path"])
    crop_path = _resolve_manifest_path(manifest_path, item["crop_image_path"])
    try:
        source_raw = source_path.read_bytes()
        crop_raw = crop_path.read_bytes()
    except OSError as error:
        raise DiagnosticError("input_image_unreadable") from error
    if _sha_bytes(source_raw) != item["source_sha256"]:
        raise DiagnosticError("source_sha_mismatch")
    if _sha_bytes(crop_raw) != item["crop_sha256"]:
        raise DiagnosticError("crop_sha_mismatch")
    try:
        with Image.open(source_path) as opened:
            source = ImageOps.exif_transpose(opened).convert("RGB")
            source_size = source.size
            source_rgb = np.asarray(source)
        with Image.open(crop_path) as opened:
            crop_image = ImageOps.exif_transpose(opened).convert("RGB")
            crop_size = crop_image.size
            rgb = np.asarray(crop_image)
    except Exception as error:
        raise DiagnosticError("input_image_decode_failed") from error
    x0, y0, x1, y1 = item["crop"]
    if source_size != (item["source_width"], item["source_height"]):
        raise DiagnosticError("source_dimensions_mismatch")
    if crop_size != (x1 - x0, y1 - y0):
        raise DiagnosticError("crop_dimensions_mismatch")
    if not np.array_equal(rgb, source_rgb[y0:y1, x0:x1]):
        raise DiagnosticError("crop_pixels_do_not_match_source_rectangle")
    actual = {
        "source_path": str(source_path), "source_sha256": item["source_sha256"],
        "source_dimensions_exif_normalized": list(source_size),
        "crop_path": str(crop_path), "crop_sha256": item["crop_sha256"],
        "crop_dimensions_exif_normalized": list(crop_size),
        "crop_box_source_xyxy": [x0, y0, x1, y1],
        "geometry_provenance": item["geometry_provenance"],
    }
    return source_path, crop_path, rgb, actual


def _locate_tessdata(tessdata_dir: Path) -> Path:
    candidate = tessdata_dir / "rus.traineddata"
    if candidate.is_file() and _sha_file(candidate) == TESSDATA_HASH:
        return candidate.resolve()
    raise DiagnosticError("tesseract_russian_traineddata_pin_mismatch")


def _tesseract_version() -> str:
    try:
        completed = subprocess.run([str(TESSERACT), "--version"], capture_output=True, text=True, timeout=5, check=True)
    except Exception as error:
        raise DiagnosticError("tesseract_version_probe_failed") from error
    first = completed.stdout.splitlines()[0] if completed.stdout else ""
    if first != "tesseract 5.5.3":
        raise DiagnosticError("tesseract_version_mismatch")
    return first


def _parse_tesseract_tsv(text: str) -> dict[str, Any]:
    import csv
    import io
    header_reader = csv.reader(io.StringIO(text), delimiter="\t")
    header = next(header_reader, None)
    if not header or len(header) != len(set(header)) or not TESSERACT_TSV_COLUMNS.issubset(set(header)):
        raise DiagnosticError("tesseract_tsv_header_invalid")
    reader = csv.DictReader(io.StringIO(text), delimiter="\t")
    words = []
    lines: dict[tuple[str, str, str, str], dict[str, Any]] = {}
    for row in reader:
        raw = row.get("text", "")
        if raw == "":
            continue
        try:
            left, top, width, height = (int(row[key]) for key in ("left", "top", "width", "height"))
            raw_confidence = row.get("conf", "")
            confidence = float(raw_confidence) if raw_confidence not in {"", None} else None
            group = tuple(row[key] for key in ("page_num", "block_num", "par_num", "line_num"))
        except (KeyError, TypeError, ValueError) as error:
            raise DiagnosticError("tesseract_tsv_malformed") from error
        word = {"text_raw": raw, "confidence_raw": confidence, "confidence_semantics": "uncalibrated_tesseract_tsv", "box_crop_xywh": [left, top, width, height], "group_ids": list(group)}
        words.append(word)
        line = lines.setdefault(group, {"group_ids": list(group), "words": []})
        line["words"].append(len(words) - 1)
    ordered_lines = sorted(
        lines.values(),
        key=lambda line: min((words[index]["box_crop_xywh"][1], words[index]["box_crop_xywh"][0]) for index in line["words"]),
    )
    for line in ordered_lines:
        line["text_raw"] = " ".join(words[index]["text_raw"] for index in sorted(line["words"], key=lambda i: (words[i]["box_crop_xywh"][0], words[i]["box_crop_xywh"][1])))
    return {
        "status": "ok" if words else "empty", "words": words,
        "line_groups": ordered_lines,
        "natural_full_field_text_raw": " ".join(line["text_raw"] for line in ordered_lines),
        "raw_tsv": text,
    }


def _tesseract_read(crop_rgb: np.ndarray, *, single_line: bool, tessdata: Path) -> dict[str, Any]:
    psm = "7" if single_line else "6"
    env = dict(os.environ)
    try:
        completed = subprocess.run(
            [str(TESSERACT), "stdin", "stdout", "--tessdata-dir", str(tessdata.parent), "-l", "rus", "--psm", psm, "-c", "tessedit_create_tsv=1"],
            input=_png_bytes(crop_rgb), capture_output=True, timeout=TESS_TIMEOUT, check=False, env=env,
        )
    except subprocess.TimeoutExpired as error:
        stderr = error.stderr.decode("utf-8", errors="replace") if isinstance(error.stderr, bytes) else error.stderr
        return {"status": "timeout", "reason_code": "tesseract_task_timeout", "psm": psm, "stderr_raw": stderr or ""}
    except Exception as error:
        return {"status": "error", "reason_code": f"tesseract_{type(error).__name__.lower()}", "psm": psm}
    stderr = completed.stderr.decode("utf-8", errors="replace") if isinstance(completed.stderr, bytes) else completed.stderr
    if completed.returncode != 0:
        stdout = completed.stdout.decode("utf-8", errors="replace") if isinstance(completed.stdout, bytes) else completed.stdout
        return {"status": "error", "reason_code": "tesseract_nonzero_exit", "psm": psm, "stdout_raw_unparsed": stdout, "stderr_raw": stderr or ""}
    stdout = completed.stdout.decode("utf-8", errors="replace") if isinstance(completed.stdout, bytes) else completed.stdout
    try:
        parsed = _parse_tesseract_tsv(stdout)
    except DiagnosticError as error:
        return {"status": "error", "reason_code": error.code, "psm": psm, "stdout_raw_unparsed": stdout, "stderr_raw": stderr or ""}
    except Exception:
        return {"status": "error", "reason_code": "tesseract_tsv_parse_failed", "psm": psm, "stdout_raw_unparsed": stdout, "stderr_raw": stderr or ""}
    parsed["psm"] = psm
    parsed["stderr_raw"] = stderr or ""
    return parsed


def _rapid_natural_rows(outputs: list[dict[str, Any]], crop_size: list[int]) -> tuple[list[dict[str, Any]], str]:
    """Group detector outputs only by nearby vertical geometry, retaining raw segments."""
    positioned = []
    for index, output in enumerate(outputs):
        box = output.get("box_source")
        if not isinstance(box, list) or len(box) != 4 or not all(isinstance(v, (int, float)) and math.isfinite(v) for v in box):
            continue
        x0, y0, x1, y1 = box
        if not (0 <= x0 < x1 <= crop_size[0] and 0 <= y0 < y1 <= crop_size[1]):
            continue
        positioned.append((index, output, [float(x0), float(y0), float(x1), float(y1)]))
    positioned.sort(key=lambda item: ((item[2][1] + item[2][3]) / 2, item[2][0], item[0]))
    rows: list[list[tuple[int, dict[str, Any], list[float]]]] = []
    for entry in positioned:
        box = entry[2]
        cy = (box[1] + box[3]) / 2
        if rows:
            previous = rows[-1]
            row_y = sum((part[2][1] + part[2][3]) / 2 for part in previous) / len(previous)
            median_height = sorted(part[2][3] - part[2][1] for part in previous)[len(previous) // 2]
            if abs(cy - row_y) <= 0.5 * min(median_height, box[3] - box[1]):
                previous.append(entry)
                continue
        rows.append([entry])
    natural_rows = []
    for row in rows:
        row.sort(key=lambda item: (item[2][0], item[0]))
        boxes = [item[2] for item in row]
        natural_rows.append({
            "text_raw": " ".join(str(item[1].get("text_raw", "")) for item in row),
            "output_indices": [item[0] for item in row],
            "box_crop_xyxy": [min(b[0] for b in boxes), min(b[1] for b in boxes), max(b[2] for b in boxes), max(b[3] for b in boxes)],
            "geometry_scope": "union_of_detector_boxes_in_same_geometric_row",
        })
    return natural_rows, " ".join(row["text_raw"] for row in natural_rows)


def _rapid_geometry(outputs: list[dict[str, Any]], crop_xyxy: list[int], crop_size: list[int]) -> list[dict[str, Any]]:
    """Bind the reader's crop-local geometry to the normalized source frame."""
    x0, y0, x1, y1 = crop_xyxy
    geometry = []
    for index, output in enumerate(outputs):
        box = output.get("box_source")
        scope = output.get("geometry_kind")
        if box is None and scope == "entire_crop":
            crop_box = [0, 0, x1 - x0, y1 - y0]
            source_box = [x0, y0, x1, y1]
        elif isinstance(box, list) and len(box) == 4 and all(isinstance(value, (int, float)) and math.isfinite(value) for value in box):
            crop_box = [float(value) for value in box]
            if 0 <= crop_box[0] < crop_box[2] <= crop_size[0] and 0 <= crop_box[1] < crop_box[3] <= crop_size[1]:
                source_box = [crop_box[0] + x0, crop_box[1] + y0, crop_box[2] + x0, crop_box[3] + y0]
            else:
                crop_box, source_box = None, None
        else:
            crop_box = None
            source_box = None
        geometry.append({
            "output_index": index, "geometry_kind": scope,
            "box_crop_xyxy": crop_box, "box_source_xyxy": source_box,
        })
    return geometry


def _png_bytes(rgb: np.ndarray) -> bytes:
    from io import BytesIO
    stream = BytesIO()
    Image.fromarray(rgb, mode="RGB").save(stream, format="PNG")
    return stream.getvalue()


def _versions() -> dict[str, Any]:
    versions = {}
    for package in ("rapidocr", "onnxruntime", "opencv-python", "Pillow", "numpy"):
        try:
            versions[package] = importlib.metadata.version(package)
        except importlib.metadata.PackageNotFoundError:
            versions[package] = "not-installed"
    return versions


def _model_pins(model_dir: Path) -> dict[str, str]:
    detector = model_dir / roi.MODEL_FILENAMES["detector"]
    recognizer = model_dir / roi.MODEL_FILENAMES["recognizer"]
    expected = {"detector": roi.DETECTOR_SHA256, "recognizer": roi.RECOGNIZER_SHA256}
    for name, path in (("detector", detector), ("recognizer", recognizer)):
        if not path.is_file() or _sha_file(path) != expected[name]:
            raise DiagnosticError(f"rapidocr_{name}_pin_mismatch")
    return {"detector": expected["detector"], "recognizer": expected["recognizer"]}


def _run_tasks(manifest_path: Path, output_dir: Path, model_dir: Path, tessdata_dir: Path) -> int:
    if sys.version_info[:2] != (3, 12) or Path(sys.prefix).resolve() != (REPO_ROOT / ".venv").resolve():
        raise DiagnosticError("repository_pinned_python_3_12_venv_required")
    if (output_dir / "roi-diagnostic-private.json").exists():
        raise DiagnosticError("private_result_already_exists")
    manifest_raw = manifest_path.read_bytes()
    crops = _load_manifest(manifest_path)
    validated_inputs = [_validate_input(item, manifest_path) for item in crops]
    pins = _model_pins(model_dir)
    tessdata = _locate_tessdata(tessdata_dir)
    tess_version = _tesseract_version()
    tesseract_sha = _sha_file(TESSERACT)
    tessdata_sha = _sha_file(tessdata)
    code_sha_before = _sha_file(Path(__file__).resolve())
    helper_sha_before = _sha_file(Path(roi.__file__).resolve())
    template_sha_before = _sha_file(Path(sys.modules["ocr.experiments.template_field_pipeline"].__file__).resolve())
    path_helper_sha_before = _sha_file(REPO_ROOT / "ocr/experiments/local_paths.py")
    engine = roi.create_pinned_cpu_engine(model_dir)
    records = []
    for index, (item, validated) in enumerate(zip(crops, validated_inputs), 1):
        sample, field = item["sample_id"], item["field_id"]
        _, _, rgb, binding = validated
        mode = "rec_only" if field in {"given_name", "gender"} else "det_rec"
        started = time.monotonic()
        rapid = roi.rapidocr_reader(engine, rgb, mode=mode)
        providers = verify_template_cpu(engine, mode=mode)
        rapid_geometry = _rapid_geometry(rapid.get("outputs", []), binding["crop_box_source_xyxy"], binding["crop_dimensions_exif_normalized"])
        if rapid.get("status") in {"ok", "empty"}:
            rapid_rows, rapid_natural_text = _rapid_natural_rows(rapid.get("outputs", []), binding["crop_dimensions_exif_normalized"]) if mode == "det_rec" else ([], " ".join(output.get("text_raw", "") for output in rapid.get("outputs", [])))
        else:
            rapid_rows, rapid_natural_text = [], None
        tesseract = _tesseract_read(rgb, single_line=mode == "rec_only", tessdata=tessdata)
        if tesseract.get("status") not in {"ok", "empty"}:
            tesseract.pop("natural_full_field_text_raw", None)
        records.append({
            "sample_id": sample, "field_id": field, "status": "completed", "reader_mode": mode,
            "reader_statuses": {"rapidocr": rapid.get("status", "error"), "tesseract": tesseract.get("status", "error")},
            "input_binding": binding,
            "rapidocr": rapid,
            "rapidocr_geometry": rapid_geometry,
            "rapidocr_natural_rows": rapid_rows,
            "rapidocr_natural_full_field_text_raw": rapid_natural_text,
            "rapidocr_actual_cpu_providers_after_call": providers,
            "tesseract": tesseract,
            "elapsed_seconds": time.monotonic() - started,
        })
        print(f"task {index}/{EXPECTED_TASKS} sample={sample} field={field} rapidocr={rapid.get('status', 'error')} tesseract={tesseract.get('status', 'error')}", flush=True)
    code_sha_after = _sha_file(Path(__file__).resolve())
    helper_sha_after = _sha_file(Path(roi.__file__).resolve())
    template_sha_after = _sha_file(Path(sys.modules["ocr.experiments.template_field_pipeline"].__file__).resolve())
    path_helper_sha_after = _sha_file(REPO_ROOT / "ocr/experiments/local_paths.py")
    if (code_sha_before, helper_sha_before, template_sha_before, path_helper_sha_before) != (code_sha_after, helper_sha_after, template_sha_after, path_helper_sha_after):
        raise DiagnosticError("runtime_code_changed_during_run")
    if _sha_bytes(manifest_path.read_bytes()) != _sha_bytes(manifest_raw):
        raise DiagnosticError("manifest_changed_during_run")
    for _, _, _, binding in validated_inputs:
        if _sha_file(Path(binding["source_path"])) != binding["source_sha256"] or _sha_file(Path(binding["crop_path"])) != binding["crop_sha256"]:
            raise DiagnosticError("input_image_changed_during_run")
    if _model_pins(model_dir) != pins or _sha_file(TESSERACT) != tesseract_sha or _sha_file(tessdata) != tessdata_sha:
        raise DiagnosticError("toolchain_or_model_changed_during_run")
    _private_json(output_dir / "roi-diagnostic-private.json", {
        "schema_version": "passport-roi-diagnostic-results/v1",
        "purpose": PURPOSE,
        "manifest_sha256": _sha_bytes(manifest_raw),
        "runtime": {
            "runner_sha256": code_sha_before, "reader_helper_sha256": helper_sha_before,
            "template_helper_sha256": template_sha_before,
            "path_helper_sha256": path_helper_sha_before,
            "rapidocr_model_sha256": pins,
            "tesseract_version": tess_version, "tesseract_binary_sha256": tesseract_sha,
            "tessdata_rus_sha256": tessdata_sha, "python": sys.version.split()[0], "packages": _versions(),
        },
        "task_count": len(records), "tasks": records,
        "posthoc_interpretation": "none; raw reader outputs only; no gold/reference text loaded",
    })
    return 0


def _terminate_group(process: subprocess.Popen[str]) -> None:
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass
    try:
        # Kill the whole group even when the direct child has exited: a spawned
        # Tesseract process may still be alive in the same session.
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass


def _parent(args: argparse.Namespace) -> int:
    output_dir = Path(args.output_dir).resolve()
    if output_dir.exists() and any(output_dir.iterdir()):
        raise DiagnosticError("output_directory_must_be_new_or_empty")
    output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    output_dir.chmod(0o700)
    if (output_dir / "roi-diagnostic-private.json").exists():
        raise DiagnosticError("private_result_already_exists")
    manifest_path = Path(args.crop_manifest).resolve()
    model_dir = Path(args.model_dir).resolve()
    tessdata_dir = Path(args.tessdata_dir).resolve()
    command = [sys.executable, str(Path(__file__).resolve()), "--worker", "--crop-manifest", str(manifest_path), "--output-dir", str(output_dir), "--model-dir", str(model_dir), "--tessdata-dir", str(tessdata_dir)]
    process = subprocess.Popen(command, cwd=str(REPO_ROOT), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
    try:
        stdout, stderr = process.communicate(timeout=CHILD_TIMEOUT)
    except subprocess.TimeoutExpired:
        _terminate_group(process)
        _private_json(output_dir / "timeout-status.json", {"status": "timeout", "timeout_seconds": CHILD_TIMEOUT})
        print(f"runner status=timeout tasks={EXPECTED_TASKS}", flush=True)
        return 124
    log_dir = output_dir / "private-logs"
    log_dir.mkdir(mode=0o700, exist_ok=True)
    _private_json(log_dir / "child-console.json", {"stdout": stdout, "stderr": stderr, "returncode": process.returncode})
    if process.returncode:
        # Do not print child exception messages; logs are private and may contain paths.
        for line in stdout.splitlines():
            if line.startswith("task ") and re.fullmatch(r"task \d+/14 sample=[A-Za-z0-9_.-]+ field=[A-Za-z0-9_.-]+ rapidocr=[a-z_]+ tesseract=[a-z_]+", line):
                print(line, flush=True)
        print(f"runner status=failed code={process.returncode}", flush=True)
        return process.returncode
    result = output_dir / "roi-diagnostic-private.json"
    if not result.is_file():
        print("runner status=failed code=result_missing", flush=True)
        return 1
    for line in stdout.splitlines():
        if line.startswith("task ") and re.fullmatch(r"task \d+/14 sample=[A-Za-z0-9_.-]+ field=[A-Za-z0-9_.-]+ rapidocr=[a-z_]+ tesseract=[a-z_]+", line):
            print(line, flush=True)
    print(f"runner status=complete tasks={EXPECTED_TASKS}", flush=True)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--crop-manifest", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--model-dir", required=True)
    parser.add_argument("--tessdata-dir", default=str(REPO_ROOT / "ocr/experiments/models/tessdata_fast-4.1.0"))
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    try:
        if args.worker:
            return _run_tasks(Path(args.crop_manifest).resolve(), Path(args.output_dir).resolve(), Path(args.model_dir).resolve(), Path(args.tessdata_dir).resolve())
        return _parent(args)
    except DiagnosticError as error:
        if args.worker:
            _private_json(Path(args.output_dir).resolve() / "worker-error-private.json", {"error_code": error.code})
        print(f"runner status=failed code={error.code}", flush=True)
        return 2
    except Exception as error:
        if args.worker:
            _private_json(Path(args.output_dir).resolve() / "worker-error-private.json", {
                "exception_type": type(error).__name__, "traceback": traceback.format_exc(),
            })
        # Exception messages and tracebacks stay in the private file.
        print(f"runner status=failed code=internal_{type(error).__name__.lower()}", flush=True)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
