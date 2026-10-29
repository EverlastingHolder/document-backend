#!/usr/bin/env python3
"""Run a synthetic smoke, then two local images when either engine passes."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


HERE = Path(__file__).resolve().parent
NATIVE_SMOKE = HERE / "run_native_smoke.py"


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def private_directory(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def private_file(path: Path) -> None:
    if path.exists():
        path.chmod(0o600)


def write_summary(json_path: Path, markdown_path: Path, report: dict[str, Any]) -> None:
    json_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    markdown = [
        "# Локальный OCR test batch",
        "",
        f"- Итог: **{report['status']}**",
        f"- Время начала: `{report['started_at_utc']}`",
        f"- Время окончания: `{report['finished_at_utc']}`",
        "",
        "| Этап | Статус | RapidOCR | Tesseract | Отчёт |",
        "|---|---|---|---|---|",
    ]
    for step in report["steps"]:
        status = step["status"]
        engines = step.get("engines", {})
        markdown.append(
            f"| {step['name']} | {status} | {engines.get('rapidocr', 'UNKNOWN')} "
            f"| {engines.get('tesseract', 'UNKNOWN')} | [{step['report']}](<{step['report']}>) |"
        )
    markdown.extend(
        [
            "",
            "Сводка содержит только статусы и ссылки на отчёты. Значения распознавания и содержимое изображений в неё не включаются.",
        ]
    )
    markdown_path.write_text("\n".join(markdown) + "\n", encoding="utf-8")
    json_path.chmod(0o600)
    markdown_path.chmod(0o600)


def run_step(
    *,
    python: Path,
    work_dir: Path,
    output_dir: Path,
    name: str,
    image: Path | None = None,
) -> tuple[int, dict[str, str]]:
    private_directory(work_dir)
    report_md = output_dir / ("ocr-smoke-report.md" if image is None else f"ocr-{name}-report.md")
    report_json = output_dir / ("ocr-smoke-report.json" if image is None else f"ocr-{name}-report.json")
    args = [
        str(python),
        str(NATIVE_SMOKE),
        "--python",
        str(python),
        "--work-dir",
        str(work_dir),
        "--report-md",
        str(report_md),
        "--report-json",
        str(report_json),
        "--skip-dependency-install",
    ]
    if image is not None:
        args.extend(["--image", str(image)])
    completed = subprocess.run(args, check=False, capture_output=True, text=True, timeout=2400)
    private_file(work_dir / "private-image-report.json")
    if image is not None:
        private_file(report_md)
        report_json = work_dir / "private-image-report.json"
    return completed.returncode, {"markdown": str(report_md), "json": str(report_json)}


def read_engine_statuses(report_path: str) -> dict[str, str] | None:
    try:
        report = json.loads(Path(report_path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    if not isinstance(report, dict):
        return None
    statuses = {}
    for engine in ("rapidocr", "tesseract"):
        section = report.get(engine)
        if not isinstance(section, dict):
            return None
        status = section.get("status")
        if not isinstance(status, str) or status not in {"PASS", "FAIL", "ERROR", "not-run"}:
            return None
        statuses[engine] = status.upper()
    return statuses


def step_status(engine_statuses: dict[str, str] | None, exit_code: int) -> str:
    if engine_statuses is None:
        return "FAIL"
    passed = sum(status == "PASS" for status in engine_statuses.values())
    if passed == 2 and exit_code == 0:
        return "PASS"
    return "PARTIAL" if passed else "FAIL"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, type=Path, help="Absolute Python 3.10–3.13 interpreter")
    parser.add_argument("--work-root", required=True, type=Path, help="Private scratch directory")
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--image", action="append", required=True, type=Path, help="Pass exactly two local image paths")
    args = parser.parse_args()

    python = args.python.expanduser().resolve()
    work_root = args.work_root.expanduser().resolve()
    output_dir = args.output_dir.expanduser().resolve()
    images = [path.expanduser().resolve() for path in args.image]
    if len(images) != 2 or images[0] == images[1]:
        parser.error("pass exactly two distinct --image paths")
    if not python.is_file():
        parser.error("--python must point to an existing Python interpreter")
    if any(not image.is_file() for image in images):
        parser.error("each --image must point to an existing local file")

    private_directory(work_root)
    output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Preserve the known synthetic-only first failure before its public report is replaced.
    previous = output_dir / "ocr-smoke-report.json"
    previous_md = output_dir / "ocr-smoke-report.md"
    if previous.exists() or previous_md.exists():
        backup_dir = work_root / "ocr-smoke-first-failure"
        is_previous_synthetic_failure = False
        if previous.is_file():
            try:
                prior_report = json.loads(previous.read_text(encoding="utf-8"))
                is_previous_synthetic_failure = (
                    prior_report.get("status") == "FAIL"
                    and prior_report.get("input", {}).get("kind") == "synthetic-fixture"
                )
            except (OSError, json.JSONDecodeError):
                pass
        if is_previous_synthetic_failure:
            private_directory(backup_dir)
            for source in (previous, previous_md):
                if source.is_file():
                    backup = backup_dir / source.name
                    shutil.copyfile(source, backup)
                    backup.chmod(0o600)

    started = utc_now()
    steps: list[dict[str, Any]] = []
    synthetic_code, synthetic_reports = run_step(
        python=python,
        work_dir=work_root / "synthetic",
        output_dir=output_dir,
        name="synthetic",
    )
    synthetic_engines = read_engine_statuses(synthetic_reports["json"])
    synthetic_status = step_status(synthetic_engines, synthetic_code)
    steps.append(
        {
            "name": "synthetic",
            "status": synthetic_status,
            "exit_code": synthetic_code,
            "engines": synthetic_engines or {"rapidocr": "UNKNOWN", "tesseract": "UNKNOWN"},
            "report": synthetic_reports["markdown"],
            "private_report": synthetic_reports["json"],
        }
    )
    if synthetic_engines is not None and any(
        status == "PASS" for status in synthetic_engines.values()
    ):
        for index, image in enumerate(images, start=1):
            name = f"image-{index:02d}"
            code, reports = run_step(
                python=python,
                work_dir=work_root / name,
                output_dir=output_dir,
                name=name,
                image=image,
            )
            engines = read_engine_statuses(reports["json"])
            steps.append(
                {
                    "name": name,
                    "status": step_status(engines, code),
                    "exit_code": code,
                    "engines": engines or {"rapidocr": "UNKNOWN", "tesseract": "UNKNOWN"},
                    "report": reports["markdown"],
                    "private_report": reports["json"],
                }
            )

    all_engines = [
        status
        for step in steps
        for status in step.get("engines", {}).values()
    ]
    report = {
        "schema": "ocr-local-batch-v1",
        "status": (
            "PASS"
            if len(steps) == 3 and all(step["status"] == "PASS" for step in steps)
            else "PARTIAL"
            if "PASS" in all_engines
            else "FAIL"
        ),
        "started_at_utc": started,
        "finished_at_utc": utc_now(),
        "steps": steps,
    }
    write_summary(output_dir / "ocr-test-batch-report.json", output_dir / "ocr-test-batch-report.md", report)
    print(f"OCR test batch {report['status']}; reports: {output_dir / 'ocr-test-batch-report.md'} ; {output_dir / 'ocr-test-batch-report.json'}")
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
