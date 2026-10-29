#!/usr/bin/env python3
"""Run a private, CPU-only PaddleOCR-VL 1.6 pilot on two local images.

The launcher owns installation, runtime checks, bounded workers, post-hoc
ground-truth scoring, and sanitized aggregate reporting. It intentionally uses
only the standard library so it can run from the existing project interpreter
while keeping that environment unchanged.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import signal
import subprocess
import sys
import time
import unicodedata
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments.local_paths import ARCHIVE_OUTPUT_ROOT, ARCHIVE_WORK_ROOT  # noqa: E402


WORK_DEFAULT = ARCHIVE_WORK_ROOT / "ocr-passport-tests"
OUTPUT_DEFAULT = ARCHIVE_OUTPUT_ROOT / "ocr-vl-comparison"
GOLD_DEFAULT = WORK_DEFAULT / "ground-truth.json"
INPUT_DEFAULTS = (
    WORK_DEFAULT / "inputs/image-01.jpg",
    WORK_DEFAULT / "inputs/image-02.jpg",
)
PRIVATE_WORK_DEFAULT = WORK_DEFAULT.parent / "ocr-vl-work"
VERIFICATION_DEFAULT = WORK_DEFAULT.parent / "ocr-vl-verification"
SCHEMA = "passport-paddleocr-vl-pilot/v1"
VENV_MANIFEST = ".passport-vl-env.json"
PADDLEOCR_VERSION = "3.7.0"
PADDLEPADDLE_VERSION = "3.2.1"
PADDLE_CPU_INDEX = "https://www.paddlepaddle.org.cn/packages/stable/cpu/"
FIELDS = (
    "surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
    "series", "number", "issue_date", "issuer", "department_code",
)
REQUIRED_FIELDS = tuple(field for field in FIELDS if field != "patronymic")
REQUIRED_BASELINE_REFERENCE = {"correct": 15, "wrong": 3, "abstain": 2, "required_fields": 20}
_ACTIVE_CHILDREN: set[subprocess.Popen[Any]] = set()


class TrialError(RuntimeError):
    """A coded failure whose message is safe for the public report."""

    def __init__(self, code: str, phase: str | None = None):
        self.code = code
        self.phase = phase
        super().__init__(code)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def ensure_private_dir(path: Path) -> None:
    missing: list[Path] = []
    cursor = path
    while not cursor.exists():
        missing.append(cursor)
        cursor = cursor.parent
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    for created in missing:
        created.chmod(0o700)
    path.chmod(0o700)


def host_resource_info(work_root: Path) -> dict[str, Any]:
    stat = os.statvfs(work_root)
    free_bytes = stat.f_bavail * stat.f_frsize
    total_memory = os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE")
    try:
        chip = subprocess.run(
            ["/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string"],
            capture_output=True, text=True, timeout=2, check=False,
        ).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        chip = "unavailable"
    return {
        "architecture": platform.machine(),
        "platform": platform.system(),
        "chip": chip or "unavailable",
        "total_memory_bytes": total_memory,
        "free_work_volume_bytes": free_bytes,
    }


def resource_preflight(work_root: Path, venv_dir: Path, minimum_free_bytes: int = 8 * 1024**3) -> dict[str, Any]:
    info = host_resource_info(work_root)
    venv_stat = os.statvfs(venv_dir.parent)
    venv_free_bytes = venv_stat.f_bavail * venv_stat.f_frsize
    info["free_venv_volume_bytes"] = venv_free_bytes
    info["minimum_free_bytes"] = minimum_free_bytes
    if min(info["free_work_volume_bytes"], venv_free_bytes) < minimum_free_bytes:
        raise TrialError("insufficient_free_disk_space", "resource_preflight")
    return info


def write_private_json(path: Path, payload: Any) -> None:
    ensure_private_dir(path.parent)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def write_public(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o644)
        stream.write(text)


def validate_inputs(inputs: list[Path]) -> list[Path]:
    if len(inputs) != 2:
        raise TrialError("exactly_two_inputs_required", "preflight")
    resolved = [path.expanduser().resolve(strict=True) for path in inputs]
    if any(not path.is_file() for path in resolved):
        raise TrialError("input_not_regular_file", "preflight")
    if len(set(resolved)) != 2:
        raise TrialError("duplicate_input_path", "preflight")
    if len({path.name.casefold() for path in resolved}) != 2:
        raise TrialError("input_basename_mapping_not_unique", "preflight")
    return resolved


def validate_deadlines(install: float, per_image: float, total: float) -> None:
    if not (1 <= install <= 1200):
        raise TrialError("install_timeout_out_of_bounds", "preflight")
    if not (1 <= per_image <= 900):
        raise TrialError("per_image_timeout_out_of_bounds", "preflight")
    if not (1 <= total <= 3600):
        raise TrialError("total_timeout_out_of_bounds", "preflight")


def worker_argv(
    python: Path,
    worker_script: Path,
    image_path: Path,
    output_path: Path,
    cache_home: Path,
) -> list[str]:
    """Construct the worker argv; no gold path or field values can enter it."""
    return [
        str(python), str(worker_script),
        "--input", str(image_path), "--output", str(output_path), "--cache-root", str(cache_home),
    ]


def worker_environment(source_env: dict[str, str], cache_home: Path) -> dict[str, str]:
    """Return a small runtime environment without gold paths or field targets."""
    allowed = ("PATH", "HOME", "CODEX_HOME", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY")
    env = {key: source_env[key] for key in allowed if key in source_env}
    env["PADDLE_PDX_CACHE_HOME"] = str(cache_home / "paddlex")
    env["XDG_CACHE_HOME"] = str(cache_home / ".cache")
    env["HF_HOME"] = str(cache_home / ".cache" / "huggingface")
    env["HUGGINGFACE_HUB_CACHE"] = str(cache_home / ".cache" / "huggingface" / "hub")
    return env


def _kill_process_group(process: subprocess.Popen[str], grace_seconds: float = 2.0) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=grace_seconds)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def _handle_termination(signum: int, _frame: Any) -> None:
    for process in list(_ACTIVE_CHILDREN):
        try:
            _kill_process_group(process, grace_seconds=1.0)
        finally:
            _ACTIVE_CHILDREN.discard(process)
    name = signal.Signals(signum).name.lower()
    raise TrialError(f"interrupted_by_{name}", "signal")


def run_bounded(
    argv: list[str], *, cwd: Path, env: dict[str, str], timeout: float,
    stdout_path: Path, stderr_path: Path, sample_rss: bool = False,
) -> dict[str, Any]:
    """Run one foreground child with private logs and optional sampled RSS."""
    ensure_private_dir(stdout_path.parent)
    ensure_private_dir(stderr_path.parent)
    rss_samples: list[int] = []
    started = time.monotonic()
    try:
        with stdout_path.open("wb") as stdout_file, stderr_path.open("wb") as stderr_file:
            os.chmod(stdout_path, 0o600)
            os.chmod(stderr_path, 0o600)
            process = subprocess.Popen(
                argv, cwd=cwd, env=env, stdout=stdout_file, stderr=stderr_file,
                start_new_session=True,
            )
            _ACTIVE_CHILDREN.add(process)
            try:
                while process.poll() is None:
                    if time.monotonic() - started >= timeout:
                        _kill_process_group(process)
                        raise TrialError("child_timeout", "worker" if sample_rss else "subprocess")
                    if sample_rss:
                        rss = sampled_rss_kib(process.pid)
                        if rss is not None:
                            rss_samples.append(rss)
                    time.sleep(0.5)
                returncode = process.returncode
            finally:
                _ACTIVE_CHILDREN.discard(process)
    except OSError as error:
        raise TrialError("child_start_failed", "subprocess") from error
    elapsed = time.monotonic() - started
    if returncode != 0:
        raise TrialError(f"child_exit_{returncode}", "worker" if sample_rss else "subprocess")
    return {
        "returncode": returncode,
        "wall_seconds": round(elapsed, 3),
        "rss_sample_peak_kib": max(rss_samples) if rss_samples else None,
        "rss_sample_count": len(rss_samples),
        "rss_sampling_interval_seconds": 0.5 if sample_rss else None,
    }


def sampled_rss_kib(pid: int) -> int | None:
    """Read process RSS from macOS ps; returned values are KiB."""
    try:
        result = subprocess.run(
            ["/bin/ps", "-o", "rss=", "-p", str(pid)],
            capture_output=True, text=True, timeout=2, check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    try:
        return int(result.stdout.strip())
    except ValueError:
        return None


def read_worker_output(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise TrialError("worker_json_missing_or_invalid", "output_validation") from error
    if not isinstance(value, dict):
        raise TrialError("worker_json_root_invalid", "output_validation")
    if set(value) == {"res"} and isinstance(value.get("res"), dict):
        value = value["res"]
    if not isinstance(value.get("parsing_res_list"), list):
        raise TrialError("worker_result_shape_unsupported", "output_validation")
    if not value["parsing_res_list"]:
        raise TrialError("worker_result_blocks_empty", "output_validation")
    for item in value["parsing_res_list"]:
        if not isinstance(item, dict):
            raise TrialError("worker_result_block_invalid", "output_validation")
        if not isinstance(item.get("block_content"), str):
            raise TrialError("worker_result_block_text_missing", "output_validation")
        bbox = item.get("block_bbox")
        if not isinstance(bbox, list) or not bbox or not all(_is_number(x) for x in _flatten_numbers(bbox)):
            raise TrialError("worker_result_block_bbox_invalid", "output_validation")
        if not isinstance(item.get("block_label"), str):
            raise TrialError("worker_result_block_label_missing", "output_validation")
    return value


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _flatten_numbers(value: Any) -> list[Any]:
    if isinstance(value, list):
        flattened: list[Any] = []
        for item in value:
            flattened.extend(_flatten_numbers(item))
        return flattened
    return [value]


def normalized_tokens(value: str) -> list[str]:
    normalized = unicodedata.normalize("NFKC", value).casefold()
    return re.findall(r"[^\W_]+", normalized, flags=re.UNICODE)


def token_sequence_in_one_block(expected: str, block_texts: list[str]) -> bool:
    expected_tokens = normalized_tokens(expected)
    if not expected_tokens:
        return False
    for block_text in block_texts:
        actual = normalized_tokens(block_text)
        size = len(expected_tokens)
        if any(actual[i:i + size] == expected_tokens for i in range(max(0, len(actual) - size + 1))):
            return True
    return False


def _load_json(path: Path, code: str) -> dict[str, Any]:
    try:
        result = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise TrialError(code, "evaluation") from error
    if not isinstance(result, dict):
        raise TrialError(code, "evaluation")
    return result


def map_gold_records(gold_path: Path, input_hashes: dict[str, str]) -> dict[str, tuple[str, dict[str, Any]]]:
    """Match gold records by source-image SHA only after inference has completed."""
    gold_root = _load_json(gold_path, "ground_truth_invalid")
    images = gold_root.get("images")
    if not isinstance(images, dict):
        raise TrialError("ground_truth_schema_invalid", "evaluation")
    matches_by_sha: dict[str, list[tuple[str, dict[str, Any]]]] = {}
    for key, entry in images.items():
        if not isinstance(key, str) or not isinstance(entry, dict):
            continue
        source = entry.get("source_image_path")
        if not isinstance(source, str) or not source:
            continue
        source_path = Path(source).expanduser().resolve()
        if not source_path.is_file():
            continue
        digest = sha256_file(source_path)
        matches_by_sha.setdefault(digest, []).append((key, entry))
    mapped: dict[str, tuple[str, dict[str, Any]]] = {}
    for sample_id, digest in input_hashes.items():
        matches = matches_by_sha.get(digest, [])
        if len(matches) != 1:
            raise TrialError("ground_truth_source_mapping_not_unique", "evaluation")
        mapped[sample_id] = matches[0]
    if len(mapped) != 2 or len({key for key, _ in mapped.values()}) != 2:
        raise TrialError("ground_truth_sample_mapping_not_unique", "evaluation")
    return mapped


def _field_literal(fields: dict[str, Any], field_id: str, *, required: bool) -> str | None:
    item = fields.get(field_id)
    if item is None and not required:
        return None
    if not isinstance(item, dict):
        raise TrialError("ground_truth_field_invalid", "evaluation")
    literal = item.get("literal")
    if literal is None and not required:
        return None
    if not isinstance(literal, str):
        raise TrialError("ground_truth_field_invalid", "evaluation")
    return literal


def score_sample(sample_id: str, blocks: list[dict[str, Any]], gold_entry: dict[str, Any]) -> dict[str, Any]:
    gold_fields = gold_entry.get("fields")
    if not isinstance(gold_fields, dict):
        raise TrialError("ground_truth_fields_invalid", "evaluation")
    block_texts = [item["block_content"] for item in blocks]
    field_rows: list[dict[str, Any]] = []
    for field_id in FIELDS:
        expected = _field_literal(gold_fields, field_id, required=field_id in REQUIRED_FIELDS)
        if expected is None:
            field_rows.append({"field_id": field_id, "gold_status": "not_provided", "ocr_token_presence": None})
            continue
        presence = token_sequence_in_one_block(expected, block_texts)
        field_rows.append({
            "field_id": field_id,
            "gold_status": "provided",
            "ocr_token_presence": presence,
        })
    required_rows = [row for row in field_rows if row["field_id"] in REQUIRED_FIELDS]
    optional_rows = [row for row in field_rows if row["field_id"] == "patronymic"]
    return {
        "sample_id": sample_id,
        "field_results": field_rows,
        "ocr_required_field_text_presence": sum(row["ocr_token_presence"] is True for row in required_rows),
        "ocr_required_fields_total": len(required_rows),
        "ocr_optional_patronymic_text_presence": optional_rows[0]["ocr_token_presence"],
    }


def public_summary(
    *, samples: list[dict[str, Any]], versions: dict[str, Any], total_seconds: float,
    host: dict[str, Any] | None = None, status: str = "COMPLETED", failure_code: str | None = None,
) -> dict[str, Any]:
    """Build an explicit allowlist-only summary; callers pass no raw text or paths."""
    summary: dict[str, Any] = {
        "schema_version": SCHEMA,
        "status": status,
        "model": "PaddleOCR-VL-1.6",
        "host": host or _public_host_info(),
        "runtime_versions": versions,
        "configured_options": {
            "pipeline_version": "v1.6",
            "device": "cpu",
            "model_backend": "local_paddlepaddle",
            "max_new_tokens": None,
            "use_layout_detection": "vendor_default_true",
            "use_doc_orientation_classify": "vendor_default_false",
            "use_doc_unwarping": "vendor_default_false",
        },
        "timing_boundary": "worker wall includes import, model init and predict; init and predict are separately measured where available",
        "rss_boundary": "sampled worker process RSS only; ps KiB at 0.5 second intervals, not total system memory",
        "total_wall_seconds": round(total_seconds, 3),
        "samples": samples,
        "evaluation_boundary": {
            "ocr_metric": "literal NFKC/casefold token sequence within a single layout block; no block concatenation or confusable correction",
            "incumbent_reference": "historical selected-field aggregate only; different metric from OCR block presence",
            "field_accuracy_claimed": False,
            "go_adapter_claimed": False,
        },
        "incumbent_reference": {
            "correct": REQUIRED_BASELINE_REFERENCE["correct"],
            "wrong": REQUIRED_BASELINE_REFERENCE["wrong"],
            "abstain": REQUIRED_BASELINE_REFERENCE["abstain"],
            "required_fields": REQUIRED_BASELINE_REFERENCE["required_fields"],
            "source": "existing two-image Go extraction reference; not directly comparable to OCR block text presence",
            "different_metric": True,
        },
    }
    if failure_code:
        summary["failure_code"] = failure_code
    return summary


def _public_host_info() -> dict[str, Any]:
    total_memory = os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE")
    try:
        chip = subprocess.run(
            ["/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string"],
            capture_output=True, text=True, timeout=2, check=False,
        ).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        chip = "unavailable"
    return {
        "architecture": platform.machine(),
        "chip": chip or "unavailable",
        "total_memory_gib": round(total_memory / 1024**3, 2),
    }


def markdown_summary(summary: dict[str, Any]) -> str:
    lines = [
        "# PaddleOCR-VL 1.6 local pilot",
        "",
        f"- Status: **{summary['status']}**",
        f"- Model: `{summary['model']}`",
        f"- Pipeline: `{summary['configured_options']['pipeline_version']}`; device: `{summary['configured_options']['device']}`",
        f"- Total wall time: {summary['total_wall_seconds']:.3f} s",
    ]
    if summary.get("failure_code"):
        lines.append(f"- Failure code: `{summary['failure_code']}`")
    lines += [
        "",
        "| Sample | OCR block token presence | Init (s) | Predict (s) | Worker wall (s) | Sampled worker RSS peak (KiB) |",
        "|---|---:|---:|---:|---:|---:|",
    ]
    for sample in summary.get("samples", []):
        lines.append(
            f"| {sample['sample_id']} | {sample['ocr_required_field_text_presence']}/{sample['ocr_required_fields_total']} | "
            f"{sample.get('model_init_seconds') if sample.get('model_init_seconds') is not None else 'n/a'} | "
            f"{sample.get('predict_seconds') if sample.get('predict_seconds') is not None else 'n/a'} | "
            f"{sample.get('worker_wall_seconds', 0):.3f} | {sample.get('rss_sample_peak_kib') if sample.get('rss_sample_peak_kib') is not None else 'n/a'} |"
        )
    lines += [
        "",
        "OCR presence means a literal normalized token sequence appeared within one returned layout block. It is not field extraction accuracy and does not imply a Go field adapter.",
        "The incumbent Go pilot reference is 15/20 correct required-field selections, with 3 wrong and 2 abstentions; it is a separate historical metric and is not directly comparable to OCR text presence. This two-image result is a development pilot, not held-out or production acceptance.",
        "",
        f"Timing boundary: {summary['timing_boundary']}.",
        f"Memory boundary: {summary['rss_boundary']}.",
        "",
    ]
    return "\n".join(lines)


def _base_python_gate() -> None:
    if sys.version_info < (3, 9) or sys.version_info >= (3, 14):
        raise TrialError("base_python_outside_vendor_verified_range", "preflight")
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise TrialError("unexpected_host_platform", "preflight")


def _venv_manifest(venv_dir: Path) -> Path:
    return venv_dir / VENV_MANIFEST


def ensure_venv(venv_dir: Path, work_root: Path, deadline: float, install_timeout: float) -> tuple[Path, dict[str, Any], bool]:
    resolved_venv = venv_dir.resolve()
    manifest_path = _venv_manifest(resolved_venv)
    if resolved_venv.exists():
        if not manifest_path.is_file():
            raise TrialError("existing_venv_identity_unknown", "environment")
        manifest = _load_json(manifest_path, "existing_venv_manifest_invalid")
        if (
            manifest.get("schema_version") != "passport-paddleocr-vl-env/v1"
            or manifest.get("venv_path") != str(resolved_venv)
            or manifest.get("environment_status") != "ready"
            or not isinstance(manifest.get("pip_freeze_sha256"), str)
        ):
            raise TrialError("existing_venv_identity_mismatch", "environment")
        python = resolved_venv / "bin/python"
        if not python.is_file():
            raise TrialError("existing_venv_python_missing", "environment")
        probe = [str(python), "-c", "import importlib.metadata as m, json; print(json.dumps({k:m.version(k) for k in ('paddleocr','paddlepaddle')}))"]
        result = _run_capture(probe, timeout=min(15.0, _remaining(deadline)), phase="environment", log_dir=work_root / "private" / "launcher-logs")
        try:
            versions = json.loads(result.stdout)
        except json.JSONDecodeError as error:
            raise TrialError("existing_venv_package_probe_invalid", "environment") from error
        if versions != {"paddleocr": PADDLEOCR_VERSION, "paddlepaddle": PADDLEPADDLE_VERSION}:
            raise TrialError("existing_venv_package_pins_mismatch", "environment")
        freeze = _run_capture(
            [str(python), "-m", "pip", "freeze", "--all"],
            timeout=min(30.0, _remaining(deadline)), phase="existing_environment_freeze",
            log_dir=work_root / "private" / "launcher-logs",
        )
        if hashlib.sha256(freeze.stdout.encode("utf-8")).hexdigest() != manifest["pip_freeze_sha256"]:
            raise TrialError("existing_venv_dependency_identity_mismatch", "environment")
        return python, manifest, False

    resolved_venv.parent.mkdir(parents=True, exist_ok=True)
    argv = [sys.executable, "-m", "venv", str(resolved_venv)]
    _run_capture(argv, timeout=min(install_timeout, _remaining(deadline)), phase="environment", log_dir=work_root / "private" / "launcher-logs")
    python = resolved_venv / "bin/python"
    if not python.is_file():
        raise TrialError("venv_creation_failed", "environment")
    manifest = {
        "schema_version": "passport-paddleocr-vl-env/v1",
        "venv_path": str(resolved_venv),
        "base_python": str(Path(sys.executable).resolve()),
        "base_python_version": platform.python_version(),
        "host_architecture": platform.machine(),
        "pinned": {"paddlepaddle": PADDLEPADDLE_VERSION, "paddleocr": PADDLEOCR_VERSION},
        "environment_status": "created_incomplete",
        "created_unix_seconds": int(time.time()),
    }
    # Written only after the venv identity has been created by this launcher.
    write_private_json(manifest_path, manifest)
    return python, manifest, True


def _remaining(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TrialError("total_timeout", "total")
    return remaining


def _run_capture(
    argv: list[str], *, timeout: float, phase: str, env: dict[str, str] | None = None,
    log_dir: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    if timeout <= 0:
        raise TrialError("total_timeout", "total")
    try:
        process = subprocess.Popen(
            argv, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, start_new_session=True,
        )
    except OSError as error:
        raise TrialError(f"{phase}_start_failed", phase) from error
    _ACTIVE_CHILDREN.add(process)
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as error:
        _kill_process_group(process)
        stdout, stderr = process.communicate()
        if log_dir is not None:
            _write_stage_logs(log_dir.parent.parent, phase, subprocess.CompletedProcess(argv, process.returncode, stdout, stderr))
        raise TrialError(f"{phase}_timeout", phase) from error
    finally:
        _ACTIVE_CHILDREN.discard(process)
    result = subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)
    if log_dir is not None:
        _write_stage_logs(log_dir.parent.parent, phase, result)
    if result.returncode != 0:
        raise TrialError(f"{phase}_exit_{result.returncode}", phase)
    return result


def _write_stage_logs(work_root: Path, stage: str, result: subprocess.CompletedProcess[str]) -> None:
    private = work_root / "private" / "launcher-logs"
    ensure_private_dir(private)
    for suffix, data in (("stdout.log", result.stdout), ("stderr.log", result.stderr)):
        path = private / f"{stage}-{suffix}"
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)


def _check_runtime(python: Path, work_root: Path, deadline: float) -> dict[str, Any]:
    code = (
        "import importlib.metadata as m, json, paddle, platform, sys; "
        "from paddleocr import PaddleOCRVL; "
        "paddle.set_device('cpu'); "
        "assert not paddle.is_compiled_with_cuda(); "
        "assert paddle.device.get_device().startswith('cpu'); "
        "print(json.dumps({'python':sys.version.split()[0],'machine':platform.machine(),'paddle':paddle.__version__,'paddleocr':m.version('paddleocr'),'paddlepaddle':m.version('paddlepaddle'),'paddlex':m.version('paddlex'),'device':paddle.device.get_device(),'cuda_compiled':paddle.is_compiled_with_cuda(),'api':'PaddleOCRVL'}))"
    )
    result = _run_capture([str(python), "-c", code], timeout=min(90.0, _remaining(deadline)), phase="runtime_probe", log_dir=work_root / "private" / "launcher-logs")
    _write_stage_logs(work_root, "runtime-probe", result)
    try:
        versions = json.loads(result.stdout.strip().splitlines()[-1])
    except (json.JSONDecodeError, IndexError) as error:
        raise TrialError("runtime_probe_output_invalid", "runtime_probe") from error
    expected = {"paddleocr": PADDLEOCR_VERSION, "paddlepaddle": PADDLEPADDLE_VERSION, "device": "cpu", "cuda_compiled": False}
    if any(versions.get(key) != value for key, value in expected.items()):
        raise TrialError("runtime_probe_contract_failed", "runtime_probe")
    freeze = _run_capture([str(python), "-m", "pip", "freeze", "--all"], timeout=min(90.0, _remaining(deadline)), phase="pip_freeze", log_dir=work_root / "private" / "launcher-logs")
    _write_stage_logs(work_root, "pip-freeze", freeze)
    freeze_path = work_root / "private" / "pip-freeze.txt"
    fd = os.open(freeze_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(freeze.stdout)
    versions["freeze_sha256"] = hashlib.sha256(freeze.stdout.encode("utf-8")).hexdigest()
    versions["freeze_file"] = "private/pip-freeze.txt"
    return versions


def _load_meta(path: Path) -> dict[str, Any]:
    return _load_json(path, "worker_metadata_missing_or_invalid")


def _validate_worker_metadata(metadata: dict[str, Any]) -> None:
    for name in ("model_init_seconds", "predict_seconds", "worker_wall_seconds"):
        value = metadata.get(name)
        if not _is_number(value) or value < 0:
            raise TrialError("worker_timing_metadata_invalid", "evaluation")
    rss_peak = metadata.get("rss_sample_peak_kib")
    if rss_peak is not None and (not isinstance(rss_peak, int) or rss_peak < 0):
        raise TrialError("worker_rss_metadata_invalid", "evaluation")
    sample_count = metadata.get("rss_sample_count")
    if not isinstance(sample_count, int) or sample_count < 0:
        raise TrialError("worker_rss_metadata_invalid", "evaluation")


def evaluate_outputs(
    *, input_hashes: dict[str, str], result_paths: dict[str, Path], metadata_paths: dict[str, Path],
    gold_path: Path, private_root: Path,
) -> list[dict[str, Any]]:
    """Validate completed inference artifacts before opening the gold file."""
    if set(input_hashes) != {"sample-01", "sample-02"} or set(result_paths) != set(input_hashes):
        raise TrialError("sample_mapping_invalid", "evaluation")
    # Validate all result and timing files first. Gold is not opened before here.
    parsed = {sample_id: read_worker_output(path) for sample_id, path in result_paths.items()}
    metadata = {sample_id: _load_meta(path) for sample_id, path in metadata_paths.items()}
    if set(metadata) != set(input_hashes):
        raise TrialError("worker_metadata_mapping_invalid", "evaluation")
    for item in metadata.values():
        _validate_worker_metadata(item)
    gold_records = map_gold_records(gold_path, input_hashes)
    scored: list[dict[str, Any]] = []
    for sample_id in ("sample-01", "sample-02"):
        parsed_data = parsed[sample_id]
        blocks = parsed_data["parsing_res_list"]
        gold_key, gold_entry = gold_records[sample_id]
        row = score_sample(sample_id, blocks, gold_entry)
        row.update({
            "worker_wall_seconds": metadata[sample_id].get("worker_wall_seconds"),
            "model_init_seconds": metadata[sample_id].get("model_init_seconds"),
            "predict_seconds": metadata[sample_id].get("predict_seconds"),
            "rss_sample_peak_kib": metadata[sample_id].get("rss_sample_peak_kib"),
            "rss_sample_count": metadata[sample_id].get("rss_sample_count"),
        })
        scored.append(row)
        # Gold key and literal OCR blocks remain in private files, never public.
        write_private_json(private_root / sample_id / "posthoc-evaluation.json", {
            "sample_id": sample_id, "gold_key": gold_key, "field_results": row["field_results"],
            "raw_block_texts": [block["block_content"] for block in blocks],
        })
    return scored


def _public_samples(samples: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [
        {
            "sample_id": sample["sample_id"],
            "field_results": sample["field_results"],
            "ocr_required_field_text_presence": sample["ocr_required_field_text_presence"],
            "ocr_required_fields_total": sample["ocr_required_fields_total"],
            "ocr_optional_patronymic_text_presence": sample["ocr_optional_patronymic_text_presence"],
            "worker_wall_seconds": sample.get("worker_wall_seconds"),
            "model_init_seconds": sample.get("model_init_seconds"),
            "predict_seconds": sample.get("predict_seconds"),
            "rss_sample_peak_kib": sample.get("rss_sample_peak_kib"),
            "rss_sample_count": sample.get("rss_sample_count"),
        }
        for sample in samples
    ]


def _report_paths(output_dir: Path) -> tuple[Path, Path]:
    return output_dir / "paddleocr-vl-comparison.json", output_dir / "paddleocr-vl-comparison.md"


def write_reports(output_dir: Path, summary: dict[str, Any]) -> None:
    json_path, md_path = _report_paths(output_dir)
    write_public(json_path, json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
    write_public(md_path, markdown_summary(summary))


def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", action="append", type=Path, default=None)
    parser.add_argument("--gold", type=Path, default=GOLD_DEFAULT)
    parser.add_argument("--work-root", type=Path, default=PRIVATE_WORK_DEFAULT)
    parser.add_argument("--output-dir", type=Path, default=OUTPUT_DEFAULT)
    parser.add_argument("--venv", type=Path, default=ROOT / ".venv-vl")
    parser.add_argument("--install-timeout-seconds", type=float, default=1200)
    parser.add_argument("--per-image-timeout-seconds", type=float, default=900)
    parser.add_argument("--total-timeout-seconds", type=float, default=3600)
    parser.add_argument("--test-only", action="store_true", help="Run model-free tests only; never install or import OCR packages")
    return parser.parse_args(argv)


def _run_unit_tests(work_root: Path, timeout: float = 25.0) -> dict[str, Any]:
    verification = VERIFICATION_DEFAULT
    ensure_private_dir(verification)
    stdout_path, stderr_path = verification / "unit-tests.stdout.log", verification / "unit-tests.stderr.log"
    command = [sys.executable, "-m", "unittest", "ocr.experiments.test_paddleocr_vl_trial", "-v"]
    try:
        completed = run_bounded(
            command, cwd=ROOT, env=os.environ.copy(), timeout=timeout,
            stdout_path=stdout_path, stderr_path=stderr_path,
        )
    except TrialError as error:
        write_private_json(verification / "unit-tests-result.json", {
            "command": command, "status": "FAIL", "failure_code": error.code,
            "stdout_log": str(stdout_path), "stderr_log": str(stderr_path),
        })
        raise
    write_private_json(verification / "unit-tests-result.json", {
        "command": command,
        **completed,
    })
    return completed


def _install_packages(python: Path, work_root: Path, deadline: float, install_timeout: float) -> dict[str, Any]:
    install_deadline = min(deadline, time.monotonic() + install_timeout)
    commands = [
        ("install-paddlepaddle", [str(python), "-m", "pip", "install", f"paddlepaddle=={PADDLEPADDLE_VERSION}", "-i", PADDLE_CPU_INDEX]),
        ("install-paddleocr", [str(python), "-m", "pip", "install", f"paddleocr[doc-parser]=={PADDLEOCR_VERSION}"]),
    ]
    installation = []
    for name, argv in commands:
        started = time.monotonic()
        result = _run_capture(argv, timeout=min(_remaining(install_deadline), _remaining(deadline)), phase=name, log_dir=work_root / "private" / "launcher-logs")
        _write_stage_logs(work_root, name, result)
        elapsed = time.monotonic() - started
        installation.append({
            "stage": name, "argv": argv,
            "elapsed_seconds": round(elapsed, 3), "exit_code": result.returncode,
        })
    return {"stages": installation}


def _check_package_pins(python: Path, work_root: Path, deadline: float) -> None:
    result = _run_capture([
        str(python), "-c",
        "import importlib.metadata as m, json; print(json.dumps({k:m.version(k) for k in ('paddleocr','paddlepaddle')}))",
    ], timeout=min(30.0, _remaining(deadline)), phase="package_version_probe", log_dir=work_root / "private" / "launcher-logs")
    _write_stage_logs(work_root, "package-version-probe", result)
    try:
        versions = json.loads(result.stdout.strip().splitlines()[-1])
    except (json.JSONDecodeError, IndexError) as error:
        raise TrialError("package_version_probe_invalid", "package_version_probe") from error
    if versions != {"paddleocr": PADDLEOCR_VERSION, "paddlepaddle": PADDLEPADDLE_VERSION}:
        raise TrialError("package_pin_verification_failed", "package_version_probe")


def _run_workers(
    *, python: Path, inputs: list[Path], work_root: Path, deadline: float, per_image_timeout: float,
) -> tuple[dict[str, str], dict[str, Path], dict[str, Path], dict[str, Any]]:
    private_root = work_root / "private"
    ensure_private_dir(private_root)
    cache_home = private_root / "model-cache"
    ensure_private_dir(cache_home)
    ensure_private_dir(cache_home / "paddlex")
    worker_script = Path(__file__).with_name("paddleocr_vl_worker.py").resolve()
    if not worker_script.is_file():
        raise TrialError("worker_script_missing", "preflight")
    input_hashes = {f"sample-{index:02d}": sha256_file(path) for index, path in enumerate(inputs, start=1)}
    result_paths: dict[str, Path] = {}
    metadata_paths: dict[str, Path] = {}
    run_details: dict[str, Any] = {}
    env = worker_environment(os.environ, cache_home)
    ensure_private_dir(cache_home / ".cache")
    ensure_private_dir(cache_home / ".cache" / "huggingface")
    for index, image_path in enumerate(inputs, start=1):
        sample_id = f"sample-{index:02d}"
        sample_dir = private_root / sample_id
        ensure_private_dir(sample_dir)
        result_path = sample_dir / f"{sample_id}_res.json"
        metadata_path = sample_dir / "worker-metadata.json"
        stdout_path = sample_dir / "stdout.log"
        stderr_path = sample_dir / "stderr.log"
        worker_env = dict(env)
        argv = worker_argv(python, worker_script, image_path, result_path, cache_home)
        bounded = run_bounded(
            argv, cwd=ROOT, env=worker_env,
            timeout=min(per_image_timeout, _remaining(deadline)),
            stdout_path=stdout_path, stderr_path=stderr_path, sample_rss=True,
        )
        # Worker-generated JSON and metadata are private, mode 0600.
        read_worker_output(result_path)
        meta = _load_meta(metadata_path)
        meta.update(bounded)
        meta["worker_wall_seconds"] = bounded["wall_seconds"]
        write_private_json(metadata_path, meta)
        result_paths[sample_id] = result_path
        metadata_paths[sample_id] = metadata_path
        run_details[sample_id] = {"argv": argv, **bounded}
    after_hashes = {f"sample-{index:02d}": sha256_file(path) for index, path in enumerate(inputs, start=1)}
    if input_hashes != after_hashes:
        raise TrialError("input_changed_during_inference", "postflight")
    write_private_json(private_root / "input-manifest.json", {
        "samples": [
            {"sample_id": sample_id, "source_path": str(inputs[index]), "sha256_pre": input_hashes[sample_id], "sha256_post": after_hashes[sample_id]}
            for index, sample_id in enumerate(("sample-01", "sample-02"))
        ],
    })
    return input_hashes, result_paths, metadata_paths, run_details


def _write_private_run_manifest(
    path: Path, *, args: argparse.Namespace, stage_records: list[dict[str, Any]], input_hashes: dict[str, str] | None,
) -> None:
    write_private_json(path, {
        "schema_version": "passport-paddleocr-vl-run-manifest/v1",
        "source_docs": [
            "https://www.paddleocr.ai/latest/en/version3.x/pipeline_usage/PaddleOCR-VL.html",
            "https://www.paddleocr.ai/v3.7.0/en/version3.x/pipeline_usage/PaddleOCR-VL-Apple-Silicon.html",
            "https://github.com/PaddlePaddle/PaddleOCR/releases/tag/v3.7.0",
            "https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.6",
            "https://github.com/PaddlePaddle/PaddleOCR/discussions/16842",
        ],
        "pins": {"paddlepaddle": PADDLEPADDLE_VERSION, "paddleocr": PADDLEOCR_VERSION, "pipeline_version": "v1.6"},
        "input_paths": [str(path) for path in (args.input or list(INPUT_DEFAULTS))],
        "gold_path": str(args.gold),
        "input_hashes_private": input_hashes,
        "test_command": [sys.executable, "-m", "unittest", "ocr.experiments.test_paddleocr_vl_trial", "-v"],
        "stages": stage_records,
    })


def run_trial(args: argparse.Namespace) -> int:
    start = time.monotonic()
    work_root = args.work_root.expanduser().resolve()
    output_dir = args.output_dir.expanduser().resolve()
    venv_dir = args.venv.expanduser().resolve()
    stage_records: list[dict[str, Any]] = []
    input_hashes: dict[str, str] | None = None
    host_info: dict[str, Any] | None = None
    total_deadline = start + args.total_timeout_seconds
    ensure_private_dir(work_root)
    ensure_private_dir(work_root / "private")
    os.umask(0o077)
    try:
        validate_deadlines(args.install_timeout_seconds, args.per_image_timeout_seconds, args.total_timeout_seconds)
        _base_python_gate()
        inputs = validate_inputs(args.input or list(INPUT_DEFAULTS))
        host_info = resource_preflight(work_root, venv_dir)
        stage_records.append({
            "stage": "resource_preflight",
            "status": "PASS",
            "host": host_info,
            "minimum_free_disk_gib": round(host_info["minimum_free_bytes"] / 1024**3, 2),
        })
        if args.test_only:
            _run_unit_tests(work_root, min(25.0, _remaining(total_deadline)))
            return 0
        if not args.gold.expanduser().resolve().is_file():
            raise TrialError("ground_truth_file_missing", "preflight")
        # Input digests are private provenance and are computed before model use.
        # Gold itself is not opened until all inference JSON and metadata validate.
        python, venv_manifest, needs_install = ensure_venv(venv_dir, work_root, total_deadline, args.install_timeout_seconds)
        stage_records.append({"stage": "venv", "status": "PASS", "python": str(python), "manifest": venv_manifest})
        test_result = _run_unit_tests(work_root, min(25.0, _remaining(total_deadline)))
        stage_records.append({"stage": "model_free_tests", "status": "PASS", **test_result})
        if needs_install:
            installation = _install_packages(python, work_root, total_deadline, args.install_timeout_seconds)
            stage_records.extend(installation["stages"])
            _check_package_pins(python, work_root, total_deadline)
        else:
            stage_records.append({"stage": "installation", "status": "REUSED_VERIFIED_ENVIRONMENT"})
        versions = _check_runtime(python, work_root, total_deadline)
        if needs_install:
            venv_manifest.update({"environment_status": "ready", "pip_freeze_sha256": versions["freeze_sha256"]})
            write_private_json(_venv_manifest(venv_dir), venv_manifest)
        stage_records.append({"stage": "runtime_probe", "status": "PASS", "versions": versions})
        image_hashes, result_paths, metadata_paths, worker_details = _run_workers(
            python=python, inputs=inputs, work_root=work_root, deadline=total_deadline,
            per_image_timeout=args.per_image_timeout_seconds,
        )
        input_hashes = image_hashes
        stage_records.append({"stage": "inference", "status": "PASS", "workers": worker_details})
        _remaining(total_deadline)
        # Explicit postflight hashes verify the source inputs remained immutable.
        for index, path in enumerate(inputs, start=1):
            if sha256_file(path) != image_hashes[f"sample-{index:02d}"]:
                raise TrialError("input_changed_during_inference", "postflight")
        samples = evaluate_outputs(
            input_hashes=image_hashes, result_paths=result_paths, metadata_paths=metadata_paths,
            gold_path=args.gold.expanduser().resolve(), private_root=work_root / "private",
        )
        _remaining(total_deadline)
        public = public_summary(
            samples=_public_samples(samples), versions={key: value for key, value in versions.items() if key not in {"freeze_file", "freeze_sha256"}},
            total_seconds=time.monotonic() - start, host=_public_host_info(),
        )
        write_reports(output_dir, public)
        stage_records.append({"stage": "evaluation_and_reports", "status": "PASS"})
        _write_private_run_manifest(work_root / "private" / "run-manifest.json", args=args, stage_records=stage_records, input_hashes=input_hashes)
        return 0
    except TrialError as error:
        phase = error.phase or "unknown"
        code = error.code
        stage_records.append({"stage": phase, "status": "FAIL", "code": code})
        write_private_json(work_root / "private" / "failure.json", {
            "phase": phase, "code": code, "created_unix_seconds": int(time.time()),
            "traceback": "".join(__import__("traceback").format_exception(error)),
        })
        elapsed = time.monotonic() - start
        failure_public = public_summary(samples=[], versions={}, total_seconds=elapsed, host=_public_host_info(), status="FAILED", failure_code=code)
        write_reports(output_dir, failure_public)
        _write_private_run_manifest(work_root / "private" / "run-manifest.json", args=args, stage_records=stage_records, input_hashes=input_hashes)
        print(f"PaddleOCR-VL pilot failed: {code}", file=sys.stderr)
        return 2
    except Exception as error:
        code = f"unexpected_{type(error).__name__.lower()}"
        stage_records.append({"stage": "unexpected", "status": "FAIL", "code": code})
        write_private_json(work_root / "private" / "failure.json", {
            "phase": "unexpected", "code": code,
            "traceback": "".join(__import__("traceback").format_exception(error)),
        })
        failure_public = public_summary(
            samples=[], versions={}, total_seconds=time.monotonic() - start,
            host=_public_host_info(), status="FAILED", failure_code=code,
        )
        write_reports(output_dir, failure_public)
        _write_private_run_manifest(work_root / "private" / "run-manifest.json", args=args, stage_records=stage_records, input_hashes=input_hashes)
        print(f"PaddleOCR-VL pilot failed: {code}", file=sys.stderr)
        return 2


def main(argv: list[str] | None = None) -> int:
    signal.signal(signal.SIGTERM, _handle_termination)
    signal.signal(signal.SIGINT, _handle_termination)
    args = _parse_args(argv)
    return run_trial(args)


if __name__ == "__main__":
    raise SystemExit(main())
