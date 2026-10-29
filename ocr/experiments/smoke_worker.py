#!/usr/bin/env python3
"""Run the bounded OCR calls after the parent bootstrap has prepared local tools."""

from __future__ import annotations

import argparse
import csv
import io
import json
import os
import platform
import re
import subprocess
import sys
import time
import traceback
import unicodedata
from importlib.metadata import version
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw, ImageFont
from rapidocr import EngineType, LangDet, LangRec, ModelType, OCRVersion, RapidOCR


FIXTURE_LINES = ["ПРОВЕРКА РАСПОЗНАВАНИЯ", "ТЕСТОВЫЙ ОБРАЗЕЦ ТЕКСТА", "КОД 000000"]
FONT_CANDIDATES = [
    "/System/Library/Fonts/Supplemental/Arial.ttf",
    "/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
    "/System/Library/Fonts/Supplemental/DejaVuSans.ttf",
    "/Library/Fonts/Arial.ttf",
]


def normalize(text: str) -> str:
    folded = unicodedata.normalize("NFKC", text).upper()
    return "".join(char for char in folded if not char.isspace())


def edit_distance(left: str, right: str) -> int:
    previous = list(range(len(right) + 1))
    for i, left_char in enumerate(left, start=1):
        current = [i]
        for j, right_char in enumerate(right, start=1):
            current.append(
                min(
                    current[j - 1] + 1,
                    previous[j] + 1,
                    previous[j - 1] + (left_char != right_char),
                )
            )
        previous = current
    return previous[-1]


def fixture_metrics(texts: list[str]) -> dict[str, Any]:
    expected = normalize("".join(FIXTURE_LINES))
    actual = normalize("".join(texts))
    errors = edit_distance(expected, actual)
    return {
        "expected_text": " | ".join(FIXTURE_LINES),
        "normalized_character_error_count": errors,
        "normalized_expected_character_count": len(expected),
        "cer": round(errors / max(1, len(expected)), 6),
    }


def create_fixture(destination: Path) -> str:
    font_path = next((path for path in FONT_CANDIDATES if Path(path).is_file()), None)
    if font_path is None:
        raise RuntimeError("No known local font with Cyrillic coverage was found")
    image = Image.new("RGB", (1600, 460), color="white")
    draw = ImageDraw.Draw(image)
    font = ImageFont.truetype(font_path, 48)
    for index, line in enumerate(FIXTURE_LINES):
        draw.text((80, 50 + index * 125), line, fill="black", font=font)
    destination.parent.mkdir(parents=True, exist_ok=True)
    image.save(destination, format="PNG")
    return font_path


def plain_value(value: Any) -> Any:
    if value is None:
        return None
    if hasattr(value, "tolist"):
        return value.tolist()
    return value


def rapid_lines(result: Any) -> list[dict[str, Any]]:
    texts = [] if result.txts is None else list(result.txts)
    boxes = plain_value(result.boxes)
    scores = plain_value(result.scores)
    lines = []
    for index, text in enumerate(texts):
        lines.append(
            {
                "text": str(text),
                "box": boxes[index] if boxes is not None and index < len(boxes) else None,
                "score": float(scores[index]) if scores is not None and index < len(scores) else None,
            }
        )
    return lines


def safe_exception_message(error: BaseException) -> str:
    message = f"{type(error).__name__}: {error}"
    message = re.sub(r"(['\"]).*?\1", "<redacted>", message)
    message = re.sub(r"(?<!\w)\d{4,}(?!\w)", "<redacted-number>", message)
    message = re.sub(r"[А-Яа-яЁё]{2,}", "<redacted-text>", message)
    message = re.sub(r"(?<!\w)/(?:[^\s:,]+/?)+", "<path>", message)
    return message[:400]


def exception_details(error: BaseException, stage: str) -> tuple[dict[str, Any], dict[str, str]]:
    frames = traceback.extract_tb(error.__traceback__)
    frame = frames[-1] if frames else None
    location = (
        {
            "file": Path(frame.filename).name,
            "function": frame.name,
            "line": frame.lineno,
        }
        if frame is not None
        else {"file": "unknown", "function": "unknown", "line": 0}
    )
    public = {"stage": stage, "type": type(error).__name__, "location": location}
    private = {"message": safe_exception_message(error)}
    return public, private


class TesseractOutputFormatError(RuntimeError):
    def __init__(self, message: str, stdout: str, stderr: str):
        super().__init__(message)
        self.stdout = stdout
        self.stderr = stderr


TESSERACT_TSV_COLUMNS = (
    "level",
    "page_num",
    "block_num",
    "par_num",
    "line_num",
    "word_num",
    "left",
    "top",
    "width",
    "height",
    "conf",
    "text",
)


def parse_tesseract_tsv(stdout: str, stderr: str = "") -> tuple[list[dict[str, Any]], dict[str, Any]]:
    """Parse unquoted Tesseract TSV without treating OCR quotes as CSV syntax."""
    reader = csv.reader(
        io.StringIO(stdout, newline=""),
        delimiter="\t",
        quoting=csv.QUOTE_NONE,
        strict=True,
    )
    try:
        header = next(reader)
    except (StopIteration, csv.Error) as error:
        raise TesseractOutputFormatError(
            "Tesseract returned an empty or malformed TSV header", stdout, stderr
        ) from error

    header = [column.strip().lstrip("\ufeff") for column in header]
    if len(header) != len(TESSERACT_TSV_COLUMNS) or tuple(header) != TESSERACT_TSV_COLUMNS:
        raise TesseractOutputFormatError(
            "Tesseract returned an unexpected TSV column layout", stdout, stderr
        )

    lines = []
    data_rows = 0
    try:
        for row in reader:
            data_rows += 1
            if len(row) != len(TESSERACT_TSV_COLUMNS):
                raise TesseractOutputFormatError(
                    "Tesseract returned a TSV row with an unexpected column count",
                    stdout,
                    stderr,
                )
            text = row[11].strip()
            if any(character in text for character in "\t\r\n"):
                raise TesseractOutputFormatError(
                    "Tesseract TSV text field contains embedded column or row separators",
                    stdout,
                    stderr,
                )
            if not text:
                continue
            try:
                score = float(row[10])
                box = [int(row[index]) for index in (6, 7, 8, 9)]
            except ValueError as error:
                raise TesseractOutputFormatError(
                    "Tesseract returned invalid numeric TSV metadata", stdout, stderr
                ) from error
            lines.append({"text": text, "box": box, "score": score})
    except csv.Error as error:
        raise TesseractOutputFormatError(
            "Tesseract returned malformed TSV rows", stdout, stderr
        ) from error

    metadata = {
        "reader": "csv.QUOTE_NONE",
        "column_count": len(header),
        "validated_data_rows": data_rows,
        "nonempty_text_rows": len(lines),
        "embedded_tsv_columns_in_text": 0,
    }
    return lines, metadata


def write_private_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8", newline="") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def output_diagnostics(stdout: str, stderr: str) -> dict[str, str]:
    return {
        "stdout_tail": stdout[-4096:],
        "stderr_tail": stderr[-4096:],
    }


def run_rapidocr(
    image_path: Path,
    det_model: Path,
    rec_model: Path,
    set_stage: Any,
) -> dict[str, Any]:
    result: dict[str, Any] = {"status": "FAIL", "lines": [], "providers": {}}
    options = {
        "Det.engine_type": EngineType.ONNXRUNTIME,
        "Det.lang_type": LangDet.CH,
        "Det.model_type": ModelType.MOBILE,
        "Det.ocr_version": OCRVersion.PPOCRV5,
        "Det.model_path": str(det_model),
        "Rec.engine_type": EngineType.ONNXRUNTIME,
        "Rec.lang_type": LangRec.ESLAV,
        "Rec.model_type": ModelType.MOBILE,
        "Rec.ocr_version": OCRVersion.PPOCRV5,
        "Rec.model_path": str(rec_model),
        "Global.use_cls": False,
        "EngineConfig.onnxruntime.use_cuda": False,
        "EngineConfig.onnxruntime.use_dml": False,
        "EngineConfig.onnxruntime.use_cann": False,
        "EngineConfig.onnxruntime.use_coreml": False,
    }
    set_stage("rapidocr-init")
    init_start = time.perf_counter()
    engine = RapidOCR(params=options)
    initialization = time.perf_counter() - init_start
    call_times = []
    last_output = None
    set_stage("rapidocr-call")
    for _ in range(3):
        start = time.perf_counter()
        last_output = engine(str(image_path))
        call_times.append(time.perf_counter() - start)
    assert last_output is not None
    lines = rapid_lines(last_output)
    result.update(
        {
            "status": "PASS" if lines else "FAIL",
            "lines": lines,
            "line_count": len(lines),
            "timings_seconds": {
                "initialization": round(initialization, 6),
                "first_call": round(call_times[0], 6),
                "warm_calls": [round(value, 6) for value in call_times[1:]],
                "rapidocr_elapse": plain_value(getattr(last_output, "elapse", None)),
                "rapidocr_elapse_list": plain_value(getattr(last_output, "elapse_list", None)),
            },
            "runtime_version": version("rapidocr"),
            "onnxruntime_version": version("onnxruntime"),
        }
    )
    set_stage("provider-check")
    for role, model_attr in (("det", "text_det"), ("rec", "text_rec")):
        stage = getattr(engine, model_attr, None)
        inference_session = getattr(getattr(stage, "session", None), "session", None)
        get_providers = getattr(inference_session, "get_providers", None)
        try:
            providers = get_providers() if callable(get_providers) else None
        except Exception as error:
            failure, private_failure = exception_details(error, "provider-check")
            result["status"] = "ERROR"
            result["stage"] = "provider-check"
            result["error"] = {"type": failure["type"], "location": failure["location"]}
            result["failure"] = failure
            result["failure_private"] = private_failure
            return result
        result["providers"][role] = providers
        if providers is None:
            result.setdefault("provider_check", {})[role] = "provider-api-unavailable"
            result["status"] = "FAIL"
            result["stage"] = "provider-check"
            result["failure"] = {
                "stage": "provider-check",
                "type": "ProviderCheckFailed",
                "location": {"file": Path(__file__).name, "function": "run_rapidocr", "line": 200},
            }
            result["failure_private"] = {"message": f"{role} provider API unavailable"}
        elif providers != ["CPUExecutionProvider"]:
            result.setdefault("provider_check", {})[role] = "not-cpu-only"
            result["status"] = "FAIL"
            result["stage"] = "provider-check"
            result["failure"] = {
                "stage": "provider-check",
                "type": "ProviderCheckFailed",
                "location": {"file": Path(__file__).name, "function": "run_rapidocr", "line": 211},
            }
            result["failure_private"] = {
                "message": f"{role} expected CPUExecutionProvider only; actual providers {providers}"
            }
    if not result["providers"] or any(value is None for value in result["providers"].values()):
        result["status"] = "FAIL"
    return result


def run_tesseract(
    executable: str,
    image_path: Path,
    tessdata_dir: Path,
    work_dir: Path,
    *,
    preserve_raw: bool,
) -> dict[str, Any]:
    result: dict[str, Any] = {"status": "FAIL", "lines": []}
    command = [
        executable,
        str(image_path),
        "stdout",
        "-l",
        "rus",
        "--oem",
        "1",
        "--psm",
        "6",
        "--tessdata-dir",
        str(tessdata_dir),
        "-c",
        "tessedit_create_tsv=1",
    ]
    outputs = []
    call_times = []
    raw_stdout_path = work_dir / "private-tesseract-output.tsv"
    raw_stderr_path = work_dir / "private-tesseract-stderr.txt"
    if preserve_raw:
        raw_stdout_path.unlink(missing_ok=True)
        raw_stderr_path.unlink(missing_ok=True)
    for _ in range(3):
        start = time.perf_counter()
        completed = subprocess.run(command, check=True, capture_output=True, text=True, timeout=180)
        call_times.append(time.perf_counter() - start)
        outputs.append(completed.stdout)
    if preserve_raw:
        write_private_text(raw_stdout_path, outputs[-1])
        write_private_text(raw_stderr_path, completed.stderr)
    lines, data_integrity = parse_tesseract_tsv(outputs[-1], completed.stderr)
    version = subprocess.run([executable, "--version"], check=True, capture_output=True, text=True, timeout=20).stdout.strip()
    result.update(
        {
            "status": "PASS" if lines else "FAIL",
            "lines": lines,
            "line_count": len(lines),
            "timings_seconds": {"process_calls_seconds": [round(value, 6) for value in call_times]},
            "runtime_version": version,
            "traineddata_tag": "tessdata_fast 4.1.0",
            "data_integrity": data_integrity,
        }
    )
    if not lines:
        result["failure"] = {
            "stage": "tesseract-output",
            "type": "EmptyOCRResult",
            "location": {
                "file": Path(__file__).name,
                "function": "run_tesseract",
                "line": sys._getframe().f_lineno,
            },
        }
        result["failure_private"] = {
            "message": "Tesseract returned valid TSV without non-empty text rows",
            **output_diagnostics(outputs[-1], completed.stderr),
        }
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--det-model", required=True, type=Path)
    parser.add_argument("--rec-model", required=True, type=Path)
    parser.add_argument("--tesseract", required=True)
    parser.add_argument("--tessdata-dir", required=True, type=Path)
    parser.add_argument("--work-dir", required=True, type=Path)
    parser.add_argument("--image", type=Path)
    args = parser.parse_args()

    image_mode = args.image is not None
    payload: dict[str, Any] = {
        "schema": "ocr-native-smoke-v1",
        "status": "FAIL",
        "input": {"kind": "user-provided-image" if image_mode else "synthetic-fixture"},
        "host": {"system": platform.platform(), "machine": platform.machine()},
        "rapidocr": {"status": "not-run"},
        "tesseract": {"status": "not-run"},
    }
    fixture_path = args.work_dir / "synthetic-russian-ocr.png"
    current_stage = "fixture"

    def set_stage(value: str) -> None:
        nonlocal current_stage
        current_stage = value

    try:
        set_stage("fixture")
        if image_mode:
            if not args.image.is_file():
                raise FileNotFoundError("Input image does not exist")
            image_path = args.image
            payload["input"]["source_name"] = "omitted-for-privacy"
        else:
            payload["input"]["font_path"] = create_fixture(fixture_path)
            image_path = fixture_path

        rapid = run_rapidocr(image_path, args.det_model, args.rec_model, set_stage)
        payload["rapidocr"] = rapid
        rapid_failure = rapid.pop("failure", None)
        rapid_private_failure = rapid.pop("failure_private", None)
        if rapid_failure is not None:
            payload["failure"] = rapid_failure
            payload["failure_private"] = rapid_private_failure

        set_stage("tesseract-output")
        tesseract = run_tesseract(
            args.tesseract,
            image_path,
            args.tessdata_dir,
            args.work_dir,
            preserve_raw=image_mode,
        )
        tesseract_failure = tesseract.pop("failure", None)
        tesseract_private_failure = tesseract.pop("failure_private", None)
        payload["tesseract"] = tesseract
        if tesseract_failure is not None:
            if "failure" not in payload:
                payload["failure"] = tesseract_failure
                payload["failure_private"] = tesseract_private_failure
            else:
                payload.setdefault("failure_private", {}).setdefault(
                    "engine_failures", []
                ).append(
                    {
                        "failure": tesseract_failure,
                        "details": tesseract_private_failure,
                    }
                )
        if not image_mode:
            rapid["fixture_metrics"] = fixture_metrics([line["text"] for line in rapid["lines"]])
            tesseract["fixture_metrics"] = fixture_metrics([line["text"] for line in tesseract["lines"]])
        payload["status"] = "PASS" if rapid["status"] == "PASS" and tesseract["status"] == "PASS" else "FAIL"
    except Exception as error:
        failure, private_failure = exception_details(error, current_stage)
        if isinstance(error, TesseractOutputFormatError):
            private_failure.update(output_diagnostics(error.stdout, error.stderr))
        payload["failure"] = failure
        payload["failure_private"] = private_failure
        if current_stage.startswith("rapidocr-") or current_stage == "provider-check":
            payload["rapidocr"] = {
                "status": "ERROR",
                "stage": current_stage,
                "error": {"type": failure["type"], "location": failure["location"]},
            }
        elif current_stage in {"tesseract", "tesseract-output"}:
            payload["tesseract"] = {
                "status": "ERROR",
                "stage": current_stage,
                "error": {"type": failure["type"], "location": failure["location"]},
            }
        print(f"OCR {current_stage} failed ({failure['type']})", file=sys.stderr)
    print(json.dumps(payload, ensure_ascii=False))
    return 0 if payload["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
