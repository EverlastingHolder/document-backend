#!/usr/bin/env python3
"""Prepare and run one local RapidOCR/Tesseract CPU smoke pass.

The parent process creates a project-local venv, installs pinned Python wheels,
fetches two SHA-256-pinned ONNX models and one tagged Tesseract language file,
then invokes smoke_worker.py. It never sends OCR input to a hosted service.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
REQUIREMENTS = HERE / "requirements.txt"
MODELS = HERE / "models"
RAPIDOCR = "3.10.0"
ONNXRUNTIME = "1.22.0"
PILLOW = "11.3.0"
DOWNLOAD_ATTEMPTS = 3
DOWNLOAD_TIMEOUT_SECONDS = 120
MODEL_SPECS = {
    "det": {
        "filename": "ch_PP-OCRv5_det_mobile.onnx",
        "url": "https://modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/det/ch_PP-OCRv5_det_mobile.onnx",
        "urls": [
            "https://modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/det/ch_PP-OCRv5_det_mobile.onnx",
            "https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/det/ch_PP-OCRv5_det_mobile.onnx",
        ],
        "sha256": "4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae",
    },
    "rec": {
        "filename": "eslav_PP-OCRv5_rec_mobile.onnx",
        "url": "https://modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/rec/eslav_PP-OCRv5_rec_mobile.onnx",
        "urls": [
            "https://modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/rec/eslav_PP-OCRv5_rec_mobile.onnx",
            "https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.10.0/onnx/PP-OCRv5/rec/eslav_PP-OCRv5_rec_mobile.onnx",
        ],
        "sha256": "08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab",
    },
}
TESSDATA_URL = (
    "https://raw.githubusercontent.com/tesseract-ocr/tessdata_fast/4.1.0/rus.traineddata"
)
TESSDATA_DIR = MODELS / "tessdata_fast-4.1.0"
REPORT_SCHEMA = "ocr-native-smoke-v1"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def sanitized_url(value: str) -> str:
    """Remove credentials, query strings, and fragments before reporting a URL."""
    from urllib.parse import urlsplit, urlunsplit

    parts = urlsplit(value)
    host = parts.hostname or ""
    if parts.port is not None:
        host = f"{host}:{parts.port}"
    return urlunsplit((parts.scheme, host, parts.path, "", ""))


def sanitized_reason(error: BaseException) -> str:
    reason = error.reason if isinstance(error, urllib.error.URLError) else error
    value = str(reason)
    value = re.sub(r"[\r\n\t]+", " ", value)
    return value[:240]


class DownloadError(RuntimeError):
    def __init__(self, url: str, attempt: int, error: BaseException):
        self.url = sanitized_url(url)
        self.attempt = attempt
        self.reason_type = type(error.reason).__name__ if isinstance(error, urllib.error.URLError) else type(error).__name__
        self.reason = sanitized_reason(error)
        super().__init__(f"Download failed after {attempt} attempt(s): {self.reason_type}")


def is_retryable_download_error(error: BaseException) -> bool:
    if isinstance(error, urllib.error.HTTPError):
        return error.code in {408, 429} or 500 <= error.code <= 599
    if isinstance(error, urllib.error.URLError):
        reason = error.reason
        if isinstance(reason, ssl.SSLCertVerificationError):
            return False
        return isinstance(reason, (TimeoutError, socket.timeout, ConnectionError, OSError))
    if isinstance(error, ssl.SSLCertVerificationError):
        return False
    return isinstance(error, (TimeoutError, socket.timeout, ConnectionError))


def download_verified(spec: dict[str, Any], destination: Path) -> dict[str, str]:
    destination.parent.mkdir(parents=True, exist_ok=True)
    expected = spec.get("sha256")
    if destination.exists():
        actual = sha256_file(destination)
        if expected and actual != expected:
            raise RuntimeError(f"Existing artifact hash mismatch: {destination.name}")
        return {"path": str(destination), "sha256": actual, "source": spec["url"]}

    urls = spec.get("urls") or [spec["url"]]
    last_error: BaseException | None = None
    last_url = urls[0]
    last_attempt = 0
    attempt_urls = list(urls[:DOWNLOAD_ATTEMPTS])
    while len(attempt_urls) < DOWNLOAD_ATTEMPTS:
        attempt_urls.append(urls[0])
    for attempt, url in enumerate(attempt_urls, start=1):
        last_url, last_attempt = url, attempt
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{destination.name}.", suffix=".part", dir=destination.parent
        )
        os.close(descriptor)
        temporary = Path(temporary_name)
        try:
            request = urllib.request.Request(
                url, headers={"User-Agent": "passport-backend-local-ocr-smoke/1"}
            )
            with urllib.request.urlopen(request, timeout=DOWNLOAD_TIMEOUT_SECONDS) as response, temporary.open("wb") as out:
                total = 0
                while block := response.read(1024 * 1024):
                    total += len(block)
                    if total > 256 * 1024 * 1024:
                        raise RuntimeError("Artifact exceeds the configured 256 MiB limit")
                    out.write(block)
            actual = sha256_file(temporary)
            if expected and actual != expected:
                raise RuntimeError(f"Downloaded artifact hash mismatch: {destination.name}")
            temporary.replace(destination)
            return {"path": str(destination), "sha256": actual, "source": sanitized_url(url)}
        except urllib.error.HTTPError as error:
            last_error = error
            if not is_retryable_download_error(error):
                break
            if attempt < DOWNLOAD_ATTEMPTS:
                time.sleep(1.0 * attempt)
        except (urllib.error.URLError, TimeoutError, socket.timeout, ConnectionError) as error:
            last_error = error
            if not is_retryable_download_error(error):
                break
            if attempt < DOWNLOAD_ATTEMPTS:
                time.sleep(1.0 * attempt)
        finally:
            temporary.unlink(missing_ok=True)
    if last_error is None:
        raise RuntimeError("No download source was configured")
    raise DownloadError(last_url, last_attempt, last_error) from last_error


def run_checked(
    args: list[str], *, timeout: int, env: dict[str, str] | None = None
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        check=True,
        capture_output=True,
        text=True,
        timeout=timeout,
        env=env,
    )


def parse_version(value: str) -> tuple[int, int, int]:
    try:
        return tuple(int(part) for part in value.split(".")[:3])  # type: ignore[return-value]
    except ValueError as error:
        raise RuntimeError(f"Cannot parse Python version: {value}") from error


def installed_smoke_packages(python: Path) -> list[str]:
    code = (
        "from importlib.metadata import version; "
        "print('rapidocr='+version('rapidocr')); "
        "print('onnxruntime='+version('onnxruntime')); "
        "print('Pillow='+version('Pillow'))"
    )
    return run_checked([str(python), "-c", code], timeout=30).stdout.splitlines()


def exact_smoke_packages(packages: list[str]) -> bool:
    return packages == [f"rapidocr={RAPIDOCR}", f"onnxruntime={ONNXRUNTIME}", f"Pillow={PILLOW}"]


def ensure_tesseract() -> str:
    executable = shutil.which("tesseract")
    if executable is None:
        brew = shutil.which("brew")
        if brew is None:
            raise RuntimeError("Tesseract is missing and Homebrew was not found")
        env = os.environ.copy()
        env["HOMEBREW_NO_AUTO_UPDATE"] = "1"
        env["HOMEBREW_NO_INSTALL_CLEANUP"] = "1"
        run_checked([brew, "install", "tesseract"], timeout=1800, env=env)
        executable = shutil.which("tesseract")
    if executable is None:
        raise RuntimeError("Tesseract installation finished but executable is not on PATH")
    version = run_checked([executable, "--version"], timeout=20).stdout.splitlines()[0]
    return f"{executable} ({version})"


def write_reports(
    report_json: Path,
    report_md: Path,
    payload: dict[str, Any],
    *,
    image_mode: bool,
) -> None:
    report_json.parent.mkdir(parents=True, exist_ok=True)
    report_md.parent.mkdir(parents=True, exist_ok=True)
    json_fd = os.open(report_json, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(json_fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(json.dumps(payload, ensure_ascii=False, indent=2) + "\n")

    status = payload.get("status", "FAILED")
    mode = payload.get("input", {}).get("kind", "not-started")
    rapid_status = payload.get("rapidocr", {}).get("status", "not-run")
    tess_status = payload.get("tesseract", {}).get("status", "not-run")
    lines = [
        "# Локальный OCR smoke test",
        "",
        f"- Итог: **{status}**",
        f"- Вход: `{mode}`",
        f"- RapidOCR: `{rapid_status}`",
        f"- Tesseract: `{tess_status}`",
        f"- Хост: `{payload.get('host', {}).get('system', 'unknown')} / {payload.get('host', {}).get('machine', 'unknown')}`",
    ]
    if payload.get("failure"):
        failure = payload["failure"]
        lines.extend(["", f"Ошибка этапа: `{failure.get('stage', 'unknown')}` ({failure.get('type', 'error')})."])
        location = failure.get("location", {})
        if location:
            lines.append(
                f"- Место: `{location.get('file', 'unknown')}:{location.get('line', 0)}` "
                f"(`{location.get('function', 'unknown')}`)"
            )
        if payload["failure"].get("url"):
            lines.append(f"- URL: `{payload['failure']['url']}`")
        if payload["failure"].get("attempt") is not None:
            lines.append(f"- Попытка: `{payload['failure']['attempt']}`")
        if payload["failure"].get("reason"):
            lines.append(f"- Причина: `{payload['failure']['reason']}`")

    for candidate in ("rapidocr", "tesseract"):
        section = payload.get(candidate, {})
        if section.get("timings_seconds"):
            timing = section["timings_seconds"]
            lines.extend(["", f"## {candidate}", ""])
            if "initialization" in timing:
                lines.extend(
                    [
                        f"- Инициализация API: `{timing.get('initialization')}` с",
                        f"- Первый вызов: `{timing.get('first_call')}` с",
                        f"- Повторные вызовы: `{timing.get('warm_calls', [])}` с",
                    ]
                )
            else:
                lines.append(f"- Полное время CLI-вызовов: `{timing.get('process_calls_seconds', [])}` с")
        lines.extend(["", f"- Найдено строк: `{section.get('line_count', 0)}`"])
        if section.get("fixture_metrics"):
            metrics = section["fixture_metrics"]
            lines.append(f"- CER по синтетическому образцу: `{metrics.get('cer')}`")
        providers = section.get("providers")
        if providers is not None:
            lines.append(f"- ONNX providers: `{providers}`")
        if section.get("error"):
            lines.append(f"- Ошибка: `{section['error'].get('type', 'error')}`")
        if not image_mode:
            recognized = [line.get("text", "") for line in section.get("lines", [])]
            lines.append(f"- Синтетический OCR текст: `{ ' | '.join(recognized) }`")

    lines.extend(
        [
            "",
            "Отчёт содержит результаты одного локального smoke-запуска. Он проверяет вызов OCR и запись результата; это не оценка качества на паспортах и не production-решение.",
        ]
    )
    if image_mode:
        lines.extend(["", "Сырые строки распознавания не включены в Markdown-отчёт."])
    md_fd = os.open(report_md, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(md_fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write("\n".join(lines) + "\n")


def run_parent(args: argparse.Namespace) -> int:
    python = Path(args.python).expanduser().resolve()
    work = Path(args.work_dir).expanduser().resolve()
    work.mkdir(parents=True, exist_ok=True)
    work.chmod(0o700)
    (work / "private-failure-diagnostics.json").unlink(missing_ok=True)
    venv = ROOT / ".venv"
    venv_python = venv / "bin" / "python"
    image_mode = args.image is not None
    report_json = Path(args.report_json).expanduser().resolve() if args.report_json else work / "synthetic-report.json"
    report_md = Path(args.report_md).expanduser().resolve() if args.report_md else work / "synthetic-report.md"
    if image_mode and not args.report_json:
        report_json = work / "private-image-report.json"
    if image_mode:
        # Raw text from an actual image is always kept in the private work directory.
        report_json = work / "private-image-report.json"
    if image_mode and not args.report_md:
        report_md = work / "private-image-report.md"

    payload: dict[str, Any] = {
        "schema": REPORT_SCHEMA,
        "created_at_utc": datetime.now(timezone.utc).isoformat(),
        "status": "FAILED",
        "input": {"kind": "user-provided-image" if image_mode else "synthetic-fixture"},
        "host": {"system": platform.platform(), "machine": platform.machine()},
        "models": {},
        "python_environment": {},
        "rapidocr": {"status": "not-run"},
        "tesseract": {"status": "not-run"},
    }
    stage = "python-preflight"
    try:
        if not python.is_file():
            raise RuntimeError(f"Requested Python interpreter does not exist: {python}")
        python_version = run_checked([str(python), "--version"], timeout=20).stdout.strip()
        version_number = python_version.removeprefix("Python ")
        parsed_version = parse_version(version_number)
        if parsed_version < (3, 10, 0) or parsed_version >= (3, 14, 0):
            raise RuntimeError("Python 3.10 through 3.13 is required by the pinned ONNX Runtime wheel")
        payload["python_environment"]["bootstrap_python"] = python_version

        stage = "create-venv"
        if not venv_python.exists():
            run_checked([str(python), "-m", "venv", str(venv)], timeout=120)

        stage = "validate-pinned-wheels"
        versions: list[str] = []
        try:
            versions = installed_smoke_packages(venv_python)
        except subprocess.CalledProcessError:
            pass
        if not exact_smoke_packages(versions):
            if args.skip_dependency_install:
                raise RuntimeError("--skip-dependency-install requires exact pinned packages in the project venv")
            stage = "install-pinned-wheels"
            pip_env = os.environ.copy()
            pip_env["PIP_DISABLE_PIP_VERSION_CHECK"] = "1"
            run_checked(
                [str(venv_python), "-m", "pip", "install", "--requirement", str(REQUIREMENTS)],
                timeout=1800,
                env=pip_env,
            )
            versions = installed_smoke_packages(venv_python)
        if not exact_smoke_packages(versions):
            raise RuntimeError("The project venv does not match the required pinned OCR package versions")
        payload["python_environment"]["packages"] = versions
        payload["python_environment"]["pip_freeze"] = run_checked(
            [str(venv_python), "-m", "pip", "freeze"], timeout=30
        ).stdout.splitlines()

        stage = "download-models"
        for role, spec in MODEL_SPECS.items():
            payload["models"][role] = download_verified(spec, MODELS / spec["filename"])

        stage = "prepare-tesseract"
        payload["tesseract"]["binary"] = ensure_tesseract()
        tess_file = TESSDATA_DIR / "rus.traineddata"
        payload["models"]["tesseract_rus"] = download_verified(
            {"filename": "rus.traineddata", "url": TESSDATA_URL}, tess_file
        )
        payload["models"]["tesseract_rus"]["version_tag"] = "tessdata_fast 4.1.0"

        stage = "run-smoke"
        worker_args = [
            str(venv_python),
            str(HERE / "smoke_worker.py"),
            "--det-model",
            str(MODELS / MODEL_SPECS["det"]["filename"]),
            "--rec-model",
            str(MODELS / MODEL_SPECS["rec"]["filename"]),
            "--tesseract",
            shutil.which("tesseract") or "tesseract",
            "--tessdata-dir",
            str(TESSDATA_DIR),
            "--work-dir",
            str(work),
        ]
        if args.image:
            worker_args.extend(["--image", str(Path(args.image).expanduser().resolve())])
        completed = subprocess.run(
            worker_args, check=False, capture_output=True, text=True, timeout=300
        )
        worker_payload = json.loads(completed.stdout.splitlines()[-1])
        private_failure = worker_payload.pop("failure_private", None)
        if private_failure is not None:
            private_path = work / "private-failure-diagnostics.json"
            private_fd = os.open(private_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(private_fd, "w", encoding="utf-8") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(
                    json.dumps(
                        {"failure": worker_payload.get("failure"), "details": private_failure},
                        ensure_ascii=False,
                        indent=2,
                    )
                    + "\n"
                )
        payload.update(worker_payload)
        payload["created_at_utc"] = datetime.now(timezone.utc).isoformat()
        payload["status"] = (
            "PASS"
            if completed.returncode == 0 and worker_payload.get("status") == "PASS"
            else "FAIL"
        )
    except Exception as error:  # produce an explicit failure report for every failed stage
        payload["status"] = "FAIL"
        payload["failure"] = {"stage": stage, "type": type(error).__name__}
        if isinstance(error, subprocess.CalledProcessError):
            payload["failure"]["exit_code"] = error.returncode
        elif isinstance(error, DownloadError):
            payload["failure"].update(
                {
                    "url": error.url,
                    "reason_type": error.reason_type,
                    "reason": error.reason,
                    "attempt": error.attempt,
                }
            )
        elif isinstance(error, urllib.error.URLError):
            payload["failure"]["reason_type"] = type(error.reason).__name__
            payload["failure"]["reason"] = sanitized_reason(error)
        print(f"OCR smoke failed at stage={stage} ({type(error).__name__})", file=sys.stderr)
    finally:
        write_reports(report_json, report_md, payload, image_mode=image_mode)
        print(f"OCR smoke {payload['status']}; reports: {report_md} ; {report_json}")

    return 0 if payload["status"] == "PASS" else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, help="Absolute path to a Python 3.10+ interpreter")
    parser.add_argument("--work-dir", required=True, help="Scratch directory outside the repository")
    parser.add_argument("--image", help="Optional local input image; its original file is not copied")
    parser.add_argument("--report-md", help="Markdown report destination; defaults inside --work-dir")
    parser.add_argument("--report-json", help="JSON report destination; defaults inside --work-dir")
    parser.add_argument(
        "--skip-dependency-install",
        action="store_true",
        help="Require the exact pinned packages already exist in the project venv; do not run pip",
    )
    return run_parent(parser.parse_args())


if __name__ == "__main__":
    raise SystemExit(main())
