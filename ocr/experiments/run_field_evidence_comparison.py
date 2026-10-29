#!/usr/bin/env python3
"""Compare cached Go extraction with and without supported-name ROI evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
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
from ocr.experiments.field_evidence_adapter import FieldEvidenceAdapterError, build_sidecar, write_private_json


SCHEMA = "passport-field-evidence-comparison/v1"
DEFAULT_CACHE = ARCHIVE_WORK_ROOT / "ocr-field-roi-work"
DEFAULT_WORK = ARCHIVE_WORK_ROOT / "ocr-field-evidence-comparison"
DEFAULT_OUTPUT = ARCHIVE_OUTPUT_ROOT / "ocr-field-evidence-comparison"
DEFAULT_GOLD = ARCHIVE_WORK_ROOT / "ocr-passport-tests" / "ground-truth.json"


class ComparisonError(RuntimeError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _read_json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ComparisonError(code) from error


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def _normalize(value: Any) -> str:
    return " ".join(unicodedata.normalize("NFC", str(value or "")).strip().split()).casefold()


def _canonical_json(value: Any, parent_key: str = "") -> Any:
    if isinstance(value, dict):
        return {key: _canonical_json(item, key) for key, item in sorted(value.items())}
    if isinstance(value, list):
        items = [_canonical_json(item) for item in value]
        if parent_key in {"line_ids", "pass_ids", "raw_variants", "rules", "issues"}:
            return sorted(items, key=lambda item: json.dumps(item, sort_keys=True, ensure_ascii=False))
        if parent_key == "candidates":
            return sorted(items, key=lambda item: (str(item.get("value", "")) if isinstance(item, dict) else "", json.dumps(item, sort_keys=True, ensure_ascii=False)))
        return items
    return value


def _field_metrics(extraction: dict[str, Any], gold: str) -> dict[str, Any]:
    fields = extraction.get("fields")
    field = fields.get("given_name") if isinstance(fields, dict) else None
    if not isinstance(field, dict):
        raise ComparisonError("go_extraction_field_missing")
    candidates = field.get("candidates")
    values = [item.get("value") for item in candidates if isinstance(item, dict) and isinstance(item.get("value"), str)] if isinstance(candidates, list) else []
    return {
        "state": field.get("state"),
        "candidate_count": len(candidates) if isinstance(candidates, list) else 0,
        "value_present": isinstance(field.get("value"), str),
        "exact_gold_candidate_present": any(_normalize(value) == _normalize(gold) for value in values),
        "exact_gold_value_selected": isinstance(field.get("value"), str) and _normalize(field.get("value")) == _normalize(gold),
        "conflicting_candidates": "conflicting_field_candidates" in (field.get("issues") or []),
    }


def _markdown(summary: dict[str, Any]) -> str:
    rows = [
        "# Supported-name ROI candidate comparison", "",
        f"- Status: **{summary['status']}**",
        f"- Samples: {summary['sample_count']}",
        "- Go received one `supported_row` RapidOCR `rec_only` candidate per accepted sidecar.",
        "- No OCR or model runtime was invoked; sidecars and Go extractions are private.",
        "- Gold was read only after all cached artifacts had been adapted and Go runs completed.",
        "- A candidate match is post-hoc evidence, not production accuracy or recognition confidence.", "",
        "| Sample | Adapter | Baseline state | Baseline exact candidate | Sidecar state | Sidecar candidates | Exact candidate after merge | Selected exact value | Conflict |",
        "|---|---|---|---:|---|---:|---:|---:|---:|",
    ]
    for row in summary["samples"]:
        base = row.get("baseline", {})
        merged = row.get("with_evidence", {})
        rows.append(
            f"| `{row['sample_id']}` | `{row['adapter_status']}` | `{base.get('state', 'n/a')}` | "
            f"{str(base.get('exact_gold_candidate_present', False)).lower()} | `{merged.get('state', 'n/a')}` | "
            f"{merged.get('candidate_count', 0)} | {str(merged.get('exact_gold_candidate_present', False)).lower()} | "
            f"{str(merged.get('exact_gold_value_selected', False)).lower()} | {str(merged.get('conflicting_candidates', False)).lower()} |"
        )
    return "\n".join(rows) + "\n"


def run_comparison(
    *, cache_root: Path = DEFAULT_CACHE, work_root: Path = DEFAULT_WORK, output_dir: Path = DEFAULT_OUTPUT,
    gold_path: Path = DEFAULT_GOLD, go_command: str = "go", go_runner: Callable[..., Any] = subprocess.run,
    timeout_seconds: float = 25.0,
) -> dict[str, Any]:
    started = time.monotonic()
    _private_dir(work_root)
    work_root = work_root.resolve()
    output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    output_dir.chmod(0o700)
    run_metadata = _read_json(cache_root / "run.json", "cached_run_metadata_invalid")
    manifest_samples = run_metadata.get("samples") if isinstance(run_metadata, dict) else None
    if not isinstance(manifest_samples, list):
        raise ComparisonError("cached_sample_manifest_invalid")
    manifest_hashes: dict[str, str] = {}
    for item in manifest_samples:
        if not isinstance(item, dict) or not isinstance(item.get("sample_id"), str) or not isinstance(item.get("image_sha256"), str):
            raise ComparisonError("cached_sample_manifest_invalid")
        sample_key = item["sample_id"]
        if sample_key in manifest_hashes:
            raise ComparisonError("cached_sample_manifest_ambiguous")
        manifest_hashes[sample_key] = item["image_sha256"]
    samples: list[dict[str, Any]] = []
    private_extractions: dict[str, dict[str, dict[str, Any]]] = {}
    for sample_id in ("sample-01", "sample-02"):
        sample_root = cache_root / sample_id
        observations = _read_json(sample_root / "baseline/observations.json", "cached_observations_invalid")
        extraction = _read_json(sample_root / "baseline/extraction.json", "cached_extraction_invalid")
        plan = _read_json(sample_root / "field-roi-plan.json", "cached_plan_invalid")
        readers = _read_json(sample_root / "reader-results.json", "cached_readers_invalid")
        image = observations.get("image") if isinstance(observations, dict) else None
        if not isinstance(image, dict) or manifest_hashes.get(sample_id) != image.get("sha256"):
            raise ComparisonError("cached_observation_manifest_sha_mismatch")
        try:
            sidecar = build_sidecar(observations, extraction, plan, readers, run_metadata)
            adapter_status, reason_code = "created", None
        except FieldEvidenceAdapterError as error:
            sidecar = None
            adapter_status, reason_code = "skipped", error.code
        sample_work = work_root / sample_id
        _private_dir(sample_work)
        observations_path = sample_root / "baseline/observations.json"
        baseline_path = sample_work / "baseline-extraction.json"
        command = [go_command, "run", "./cmd/ocr-eval", "--input", str(observations_path), "--output", str(baseline_path), "--passes", "all", "--normalize-names", "--mrz-policy", "augment"]
        remaining = timeout_seconds - (time.monotonic() - started)
        if remaining <= 0:
            raise ComparisonError("comparison_timeout")
        try:
            result = go_runner(command, cwd=ROOT, timeout=min(remaining, 15.0), capture_output=True, text=True, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise ComparisonError("go_baseline_run_failed_or_timed_out") from error
        if result.returncode != 0:
            raise ComparisonError("go_baseline_run_failed")
        baseline_result = _read_json(baseline_path, "go_baseline_output_invalid")
        if _canonical_json(baseline_result) != _canonical_json(extraction):
            raise ComparisonError("cached_go_baseline_semantic_parity_failed")
        with_result = None
        if sidecar is not None:
            sidecar_path = sample_work / "field-evidence.json"
            write_private_json(sidecar_path, sidecar)
            with_path = sample_work / "with-evidence-extraction.json"
            command = [go_command, "run", "./cmd/ocr-eval", "--input", str(observations_path), "--output", str(with_path), "--field-evidence", str(sidecar_path), "--passes", "all", "--normalize-names", "--mrz-policy", "augment"]
            remaining = timeout_seconds - (time.monotonic() - started)
            if remaining <= 0:
                raise ComparisonError("comparison_timeout")
            try:
                result = go_runner(command, cwd=ROOT, timeout=min(remaining, 15.0), capture_output=True, text=True, check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                raise ComparisonError("go_evidence_run_failed_or_timed_out") from error
            if result.returncode != 0:
                raise ComparisonError("go_evidence_run_failed")
            with_result = _read_json(with_path, "go_evidence_output_invalid")
        private_extractions[sample_id] = {"baseline": baseline_result, "with_evidence": with_result}
        samples.append({"sample_id": sample_id, "adapter_status": adapter_status, "adapter_reason_code": reason_code})

    # No ground-truth read occurs until all adapter and Go candidate processing is complete.
    gold_root = _read_json(gold_path, "ground_truth_invalid")
    images = gold_root.get("images") if isinstance(gold_root, dict) else None
    if not isinstance(images, dict):
        raise ComparisonError("ground_truth_schema_invalid")
    expected_hashes = {item.get("sample_id"): item.get("image_sha256") for item in run_metadata.get("samples", []) if isinstance(item, dict)}
    gold_key_by_sample: dict[str, str] = {}
    gold_keys_by_sha: dict[str, str] = {}
    for key, entry in images.items():
        source_path = Path(entry.get("source_image_path", "")).resolve() if isinstance(entry, dict) and entry.get("source_image_path") else None
        if source_path is None:
            continue
        try:
            source_path.relative_to(gold_path.parent.resolve())
        except ValueError:
            continue
        if not source_path.is_file():
            continue
        digest = _sha256_file(source_path)
        if digest in gold_keys_by_sha:
            raise ComparisonError("ground_truth_source_sha_ambiguous")
        gold_keys_by_sha[digest] = key
        matches = [sample_id for sample_id, expected in expected_hashes.items() if expected == digest]
        if len(matches) > 1:
            raise ComparisonError("cached_sample_sha_ambiguous")
        if len(matches) == 1:
            if matches[0] in gold_key_by_sample:
                raise ComparisonError("ground_truth_sample_mapping_ambiguous")
            gold_key_by_sample[matches[0]] = key
    for row in samples:
        key = gold_key_by_sample.get(row["sample_id"])
        if key is None:
            raise ComparisonError("ground_truth_mapping_invalid")
        fields = images[key].get("fields") if isinstance(images[key], dict) else None
        given_name = fields.get("given_name") if isinstance(fields, dict) else None
        literal = given_name.get("literal") if isinstance(given_name, dict) else None
        if not isinstance(literal, str):
            raise ComparisonError("ground_truth_field_invalid")
        raw_results = private_extractions[row["sample_id"]]
        row["baseline"] = _field_metrics(raw_results["baseline"], literal)
        if raw_results["with_evidence"] is not None:
            row["with_evidence"] = _field_metrics(raw_results["with_evidence"], literal)
        else:
            row["with_evidence"] = {}
    summary = {
        "schema_version": SCHEMA, "status": "COMPLETED", "sample_count": len(samples),
        "comparison_mode": "cached_roi_reader_results_and_go_cli_only",
        "gold_loaded_after_adapter_and_go_runs": True,
        "samples": samples,
    }
    public_path = output_dir / "field-evidence-comparison.json"
    public_path.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    markdown_path = output_dir / "field-evidence-comparison.md"
    markdown_path.write_text(_markdown(summary), encoding="utf-8")
    public_path.chmod(0o600)
    markdown_path.chmod(0o600)
    return summary


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache-root", type=Path, default=DEFAULT_CACHE)
    parser.add_argument("--work-root", type=Path, default=DEFAULT_WORK)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--gold", type=Path, default=DEFAULT_GOLD)
    parser.add_argument("--go", default="go")
    parser.add_argument("--timeout", type=float, default=25.0)
    args = parser.parse_args()
    try:
        summary = run_comparison(cache_root=args.cache_root, work_root=args.work_root, output_dir=args.output_dir, gold_path=args.gold, go_command=args.go, timeout_seconds=args.timeout)
    except ComparisonError as error:
        print(f"field-evidence comparison failed: {error.code}", file=sys.stderr)
        return 2
    print(f"completed samples={summary['sample_count']} reports=2")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
