#!/usr/bin/env python3
"""Run a bounded two-image registration-first OCR/Go comparison.

Uses only the existing pinned local RapidOCR CPU models and private local
inputs. OCR observations, source lineage, and Go outputs stay in the 0700 work
directory; the report contains aggregate field metrics without OCR text.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments import run_algorithm_comparison as incumbent
from ocr.experiments.registration_first_pipeline import build_registered_observations


class RunnerError(RuntimeError):
    """A sanitized failure in the finite comparison runner."""


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def _private_json(path: Path, payload: Any) -> None:
    _private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def _gold_index_by_source_sha(path: Path, source_shas: set[str]) -> dict[str, tuple[str, dict[str, Any]]]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise RunnerError("ground_truth_unreadable") from error
    entries = data.get("images") if isinstance(data, dict) else None
    if not isinstance(entries, dict):
        raise RunnerError("ground_truth_schema_invalid")
    matches: dict[str, list[tuple[str, dict[str, Any]]]] = {sha: [] for sha in source_shas}
    for key, entry in entries.items():
        if not isinstance(entry, dict):
            continue
        source_path = entry.get("source_image_path")
        if not isinstance(source_path, str) or not source_path:
            continue
        resolved = Path(source_path).expanduser()
        if not resolved.is_absolute():
            resolved = path.parent / resolved
        resolved = resolved.resolve()
        if not resolved.is_file():
            continue
        source_sha = incumbent.sha256_file(resolved)
        if source_sha in matches:
            matches[source_sha].append((str(key), entry))
    output: dict[str, tuple[str, dict[str, Any]]] = {}
    for source_sha, rows in matches.items():
        if len(rows) != 1:
            raise RunnerError("ground_truth_source_path_sha_missing_or_ambiguous")
        if not isinstance(rows[0][1].get("fields"), dict):
            raise RunnerError("ground_truth_field_map_invalid")
        output[source_sha] = rows[0]
    return output


def _posthoc_score(
    gold_path: Path,
    completed: list[dict[str, Any]],
    *,
    event_log: list[str] | None = None,
) -> list[dict[str, Any]]:
    """Resolve labels by source file SHA after both arms' Go outputs exist."""
    source_shas = {str(row.get("source_sha256", "")) for row in completed}
    if len(completed) != 4 or len(source_shas) != 2 or any("extraction" not in row for row in completed):
        raise RunnerError("posthoc_scoring_requires_four_completed_go_outputs")
    if event_log is not None:
        event_log.append("gold_read")
    gold_by_sha = _gold_index_by_source_sha(gold_path, source_shas)
    scored: list[dict[str, Any]] = []
    for row in completed:
        gold_key, gold_entry = gold_by_sha[str(row["source_sha256"])]
        scored.append({
            "sample_id": row["sample_id"],
            "arm": row["arm"],
            "route": row["route"],
            "registration_reason": row.get("registration_reason"),
            "source_sha256_verified": True,
            "canonical_frame": row["route"] != "fallback",
            "gold_mapping_verified": bool(gold_key),
            "ocr_seconds": row["ocr_seconds"],
            "go_seconds": row["go_seconds"],
            **incumbent.score_extraction(row["extraction"], gold_entry["fields"]),
        })
    return scored


def _run(command: list[str], *, cwd: Path, env: dict[str, str], timeout: int, phase: str) -> None:
    try:
        completed = subprocess.run(command, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout, check=False)
    except subprocess.TimeoutExpired as error:
        raise RunnerError(f"{phase}_timeout") from error
    except OSError as error:
        raise RunnerError(f"{phase}_could_not_start") from error
    if completed.returncode:
        raise RunnerError(f"{phase}_failed_exit_{completed.returncode}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", required=True, type=Path, help="The exact existing project venv Python")
    parser.add_argument("--go", required=True, type=Path, help="The selected Go executable")
    parser.add_argument("--image", action="append", required=True, type=Path, help="Exactly two explicit local source photos")
    parser.add_argument("--gold", required=True, type=Path, help="Private labels with source_image_path references, matched by SHA-256 after OCR and Go outputs")
    parser.add_argument("--model-dir", required=True, type=Path, help="Existing cached pinned RapidOCR ONNX models")
    parser.add_argument("--work-root", required=True, type=Path, help="Private OCR, lineage, and extraction output")
    parser.add_argument("--output-dir", required=True, type=Path, help="Private aggregate comparison report")
    parser.add_argument("--timeout-seconds", type=int, default=900, help="Whole-run wall-clock limit (30-1800 seconds)")
    parser.add_argument("--_worker", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()

    python_executable = args.python.expanduser().absolute()
    python = python_executable.resolve()
    go = args.go.expanduser().resolve()
    images = [item.expanduser().resolve() for item in args.image]
    gold_path = args.gold.expanduser().resolve()
    model_dir = args.model_dir.expanduser().resolve()
    work_root = args.work_root.expanduser().resolve()
    output_dir = args.output_dir.expanduser().resolve()
    if not 30 <= args.timeout_seconds <= 1800:
        print("registration-first comparison failed reason=timeout_out_of_bounds", file=sys.stderr)
        return 2
    if not args._worker:
        worker_argv = [
            str(python_executable), str(Path(__file__).resolve()), *sys.argv[1:], "--_worker",
        ]
        try:
            worker = subprocess.Popen(
                worker_argv, cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                start_new_session=True,
            )
            stdout, stderr = worker.communicate(timeout=args.timeout_seconds)
        except subprocess.TimeoutExpired:
            worker_pid = worker.pid
            try:
                os.killpg(worker_pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                worker.wait(timeout=3)
            except subprocess.TimeoutExpired:
                pass
            # The leader may exit on SIGTERM while a grandchild remains alive.
            # Always signal the original process group after the grace period.
            try:
                os.killpg(worker_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            worker.wait()
            _private_dir(work_root)
            _private_json(work_root / "registration-first-runner-exit.json", {
                "status": "timeout", "timeout_seconds": args.timeout_seconds,
            })
            print("registration-first comparison failed reason=whole_run_timeout", file=sys.stderr)
            return 124
        if stdout:
            print(stdout, end="")
        if stderr:
            print(stderr, end="", file=sys.stderr)
        _private_dir(work_root)
        _private_json(work_root / "registration-first-runner-exit.json", {
            "status": "completed" if worker.returncode == 0 else "failed",
            "exit_code": worker.returncode,
            "timeout_seconds": args.timeout_seconds,
        })
        return worker.returncode
    try:
        if len(images) != 2 or len(set(images)) != 2 or any(not item.is_file() for item in images):
            raise RunnerError("exactly_two_distinct_existing_images_required")
        if Path(sys.executable).resolve() != python:
            raise RunnerError("runner_python_mismatch")
        if not gold_path.is_file():
            raise RunnerError("ground_truth_file_missing")
        go_version, python_versions = incumbent.check_toolchain(python, go, model_dir)
        det_model = model_dir / incumbent.MODEL_HASHES["det"][0]
        rec_model = model_dir / incumbent.MODEL_HASHES["rec"][0]
        _private_dir(work_root)
        _private_dir(output_dir)
        _private_dir(work_root / "go-cache")
        env = os.environ.copy()
        env.update({
            "GOCACHE": str(work_root / "go-cache"),
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
        })
        evaluator = work_root / "ocr-eval"
        _run([str(go), "build", "-o", str(evaluator), "./cmd/ocr-eval"], cwd=ROOT, env=env, timeout=300, phase="go_build")
        if not evaluator.is_file():
            raise RunnerError("go_evaluator_not_created")
        evaluator.chmod(0o700)

        source_records: list[dict[str, Any]] = []
        completed_rows: list[dict[str, Any]] = []
        input_source_shas = [incumbent.sha256_file(path) for path in images]
        if len(set(input_source_shas)) != 2:
            raise RunnerError("duplicate_source_image_sha")
        from ocr.experiments.region_pipeline import build_observations, create_cpu_engine

        for index, image_path in enumerate(images, start=1):
            sample_id = f"sample-{index:02d}"
            source_sha = input_source_shas[index - 1]
            sample_work = work_root / sample_id
            _private_dir(sample_work)
            engine_state: dict[str, Any] = {}

            def engine_factory(det: str | Path, rec: str | Path) -> Any:
                selected = create_cpu_engine(det, rec)
                engine_state["engine"] = selected
                return selected

            observation_start = time.perf_counter()
            result = build_registered_observations(
                image_path, sample_id, det_model, rec_model,
                private_output_dir=sample_work,
                engine_factory=engine_factory,
            )
            observation_elapsed = time.perf_counter() - observation_start
            observation = result["observations"]
            observation_path = Path(result.get("observations_path") or sample_work / "observations.json")
            if not result.get("observations_path"):
                _private_json(observation_path, observation)
            image_meta = observation.get("image") if isinstance(observation, dict) else None
            if not isinstance(image_meta, dict) or image_meta.get("id") != (
                f"{sample_id}:registered-spread" if result["status"] != "fallback" else sample_id
            ):
                raise RunnerError("observation_image_id_mismatch")
            if result["status"] == "fallback":
                if image_meta.get("sha256") != source_sha:
                    raise RunnerError("fallback_source_sha_mismatch")
            else:
                lineage_path = Path(result["lineage_path"])
                lineage = json.loads(lineage_path.read_text(encoding="utf-8"))
                canonical_path = Path(result["canonical_image_path"])
                if (
                    lineage.get("source_image", {}).get("sha256") != source_sha
                    or lineage.get("canonical_image", {}).get("sha256") != image_meta.get("sha256")
                    or incumbent.sha256_file(canonical_path) != image_meta.get("sha256")
                ):
                    raise RunnerError("registered_source_or_canonical_sha_mismatch")
            if incumbent.sha256_file(image_path) != source_sha:
                raise RunnerError("source_image_changed_during_run")

            if result["status"] == "fallback":
                baseline_observation = observation
                baseline_observation_path = observation_path
                baseline_ocr_seconds = observation_elapsed
                reused_for_fallback = True
            else:
                if "engine" not in engine_state:
                    raise RunnerError("registered_route_engine_missing")
                baseline_start = time.perf_counter()
                baseline_observation = build_observations(
                    image_path=image_path,
                    image_id=sample_id,
                    det_model=det_model,
                    rec_model=rec_model,
                    engine=engine_state["engine"],
                    enhanced=True,
                    include_mrz=True,
                )
                baseline_ocr_seconds = time.perf_counter() - baseline_start
                baseline_observation_path = sample_work / "baseline-observations.json"
                _private_json(baseline_observation_path, baseline_observation)
                reused_for_fallback = False
            baseline_image_meta = baseline_observation.get("image", {})
            if (
                baseline_image_meta.get("id") != sample_id
                or baseline_image_meta.get("sha256") != source_sha
                or baseline_image_meta.get("coordinate_frame") != "exif_normalized_pixels"
            ):
                raise RunnerError("baseline_observation_source_binding_failed")

            arms = [
                ("baseline", baseline_observation, baseline_observation_path, baseline_ocr_seconds),
                ("registration_first", observation, observation_path, observation_elapsed),
            ]
            sample_outputs: dict[str, dict[str, Any]] = {}
            baseline_extraction: dict[str, Any] | None = None
            baseline_go_seconds = 0.0
            for arm, arm_observation, arm_path, arm_ocr_seconds in arms:
                extraction_path = sample_work / f"{arm}-go-extraction.json"
                if arm == "registration_first" and reused_for_fallback:
                    assert baseline_extraction is not None
                    extraction = baseline_extraction
                    extraction_path = sample_work / "baseline-go-extraction.json"
                    go_seconds = baseline_go_seconds
                else:
                    arm_image_meta = arm_observation.get("image", {})
                    started = time.perf_counter()
                    _run([
                        str(evaluator), "--input", str(arm_path), "--output", str(extraction_path),
                        "--passes=all", "--normalize-names=true", "--mrz-policy=augment",
                    ], cwd=ROOT, env=env, timeout=60, phase="go_extract")
                    go_seconds = time.perf_counter() - started
                    extraction = json.loads(extraction_path.read_text(encoding="utf-8"))
                    if extraction.get("schema_version") != "passport-extraction/v1" or extraction.get("image_id") != arm_image_meta.get("id"):
                        raise RunnerError("go_extraction_schema_or_image_mismatch")
                    if arm == "baseline":
                        baseline_extraction = extraction
                        baseline_go_seconds = go_seconds
                sample_outputs[arm] = {
                    "sample_id": sample_id,
                    "arm": arm,
                    "route": result["status"],
                    "registration_reason": result.get("reason"),
                    "source_sha256": source_sha,
                    "ocr_seconds": round(arm_ocr_seconds, 6),
                    "go_seconds": round(go_seconds, 6),
                    "extraction": extraction,
                    "observation_sha256": arm_observation.get("image", {}).get("sha256"),
                    "reused_for_fallback": reused_for_fallback and arm == "registration_first",
                }
            completed_rows.extend(sample_outputs.values())
            source_records.append({
                "sample_id": sample_id,
                "source_path": str(image_path),
                "source_sha256": source_sha,
                "observation_path": str(observation_path),
                "observation_sha256": image_meta["sha256"],
                "baseline_observation_sha256": baseline_image_meta["sha256"],
                "route": result["status"],
                "registered_arm_reused_for_fallback": reused_for_fallback,
                "reason": result.get("reason"),
            })

        # Ground truth is opened only after both OCR routes and Go extractions
        # have completed and their source/canonical hashes have been checked.
        scored_rows = _posthoc_score(gold_path, completed_rows)
        scored_by_sample: dict[str, dict[str, dict[str, Any]]] = {}
        for row in scored_rows:
            scored_by_sample.setdefault(row["sample_id"], {})[row["arm"]] = row
        paired_rows = []
        for sample_id in ("sample-01", "sample-02"):
            arms = scored_by_sample.get(sample_id, {})
            if set(arms) != {"baseline", "registration_first"}:
                raise RunnerError("paired_comparison_arm_missing")
            baseline_row = arms["baseline"]
            registered_row = arms["registration_first"]
            baseline_totals = baseline_row["totals"]
            registered_totals = registered_row["totals"]
            paired_rows.append({
                "sample_id": sample_id,
                "route": registered_row["route"],
                "source_sha256_verified": True,
                "gold_mapping_verified": baseline_row["gold_mapping_verified"] and registered_row["gold_mapping_verified"],
                "registered_arm_reused_for_fallback": next(item["registered_arm_reused_for_fallback"] for item in source_records if item["sample_id"] == sample_id),
                "baseline": baseline_row,
                "registration_first": registered_row,
                "delta": {
                    "required_suggested_correct": registered_totals["required_suggested_correct"] - baseline_totals["required_suggested_correct"],
                    "required_wrong_suggestion": registered_totals["required_wrong_suggestion"] - baseline_totals["required_wrong_suggestion"],
                    "required_false_fill": registered_totals["required_false_fill"] - baseline_totals["required_false_fill"],
                    "required_missing_or_abstained": registered_totals["required_missing_or_abstained"] - baseline_totals["required_missing_or_abstained"],
                },
            })
        report = {
            "schema_version": "passport-registration-first-comparison/v1",
            "status": "PASS",
            "scope": "two development images; not a held-out accuracy estimate",
            "route_counts": {
                "accepted_registered": sum(row["route"] != "fallback" for row in paired_rows),
                "source_fallback": sum(row["route"] == "fallback" for row in paired_rows),
            },
            "toolchain": {"go": go_version, "python_packages": python_versions},
            "models": {role: values[1] for role, values in incumbent.MODEL_HASHES.items()},
            "go_options": {"passes": "all", "normalize_names": True, "mrz_policy": "augment"},
            "results": paired_rows,
        }
        _private_json(work_root / "registration-first-private-manifest.json", {"inputs": source_records})
        _private_json(work_root / "registration-first-private-report.json", report)
        _private_json(output_dir / "registration-first-comparison.json", report)
        rows = [
            "# Registration-first OCR comparison", "",
            "Two development images; this is not a held-out accuracy estimate.",
            "Raw OCR, source hashes, and extracted values remain in the private work directory.", "",
            "| Sample | Route | Baseline exact / 10 | Registered exact / 10 | Delta | Wrong suggestions delta | False fills delta | Abstentions delta |",
            "|---|---|---:|---:|---:|---:|---:|---:|",
        ]
        for row in paired_rows:
            base_totals = row["baseline"]["totals"]
            reg_totals = row["registration_first"]["totals"]
            rows.append(
                f"| {row['sample_id']} | {row['route']} | {base_totals['required_suggested_correct']}/10 | "
                f"{reg_totals['required_suggested_correct']}/10 | {row['delta']['required_suggested_correct']:+d} | "
                f"{row['delta']['required_wrong_suggestion']:+d} | {row['delta']['required_false_fill']:+d} | "
                f"{row['delta']['required_missing_or_abstained']:+d} |"
            )
        report_path = output_dir / "registration-first-comparison.md"
        report_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
        report_path.chmod(0o600)
        print(f"completed samples=2 registered={report['route_counts']['accepted_registered']} fallback={report['route_counts']['source_fallback']}")
        return 0
    except RunnerError as error:
        print(f"registration-first comparison failed reason={error}", file=sys.stderr)
        return 2
    except Exception:
        print("registration-first comparison failed reason=internal_error", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
