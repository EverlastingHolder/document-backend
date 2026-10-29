#!/usr/bin/env python3
"""Private, bounded page-registration probe with optional diagnostic OCR.

The public report contains only anonymous statuses, numeric geometry,
aggregate OCR counts/timing. Raw image
content, OCR text, source/model hashes and local paths stay under --work-root.
This is a diagnostic experiment; it does not assemble passport fields.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import importlib.resources
import json
import os
import re
import sys
import time
from pathlib import Path
from typing import Any, Callable

import cv2
import numpy as np
from PIL import Image, ImageOps

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from ocr.experiments import run_algorithm_comparison as base
from ocr.experiments.page_registration import (
    RegistrationResult,
    RegisteredPage,
    map_page_quad_to_original,
    register_spread,
    warp_registered_page,
)


SCHEMA_V2 = "passport-ocr-observations/v2"
PUBLIC_SCHEMA = "passport-page-registration-probe/v1"
PRIVATE_SCHEMA = "passport-page-registration-private/v1"
LABEL_TERMS = (
    "ФАМИЛИЯ", "ИМЯ", "ОТЧЕСТВО", "ПОЛ", "ДАТАРОЖДЕНИЯ", "МЕСТОРОЖДЕНИЯ",
    "ДАТАВЫДАЧИ", "КЕМВЫДАН", "ОРГАНВЫДАВШИЙ",
)
LABEL_LOOKALIKE = {
    "A": "А", "B": "В", "C": "С", "E": "Е", "H": "Н", "K": "К",
    "M": "М", "O": "О", "P": "Р", "T": "Т", "X": "Х", "Y": "У",
}


class ProbeError(RuntimeError):
    def __init__(self, reason: str) -> None:
        super().__init__(reason)
        self.reason = reason


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _private_dir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)


def _write_json(path: Path, payload: Any, *, private: bool) -> None:
    _private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(payload, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def _write_markdown(path: Path, text: str) -> None:
    _private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(text)


def _write_geometry_overlay(path: Path, image_rgb: np.ndarray, registration: RegistrationResult) -> None:
    """Save source-photo overlays only in the caller's private work directory."""
    quads = list(registration.candidate_quads)
    if registration.source_quad is not None:
        quads.insert(0, registration.source_quad)
    if not quads:
        return
    overlay = cv2.cvtColor(image_rgb.copy(), cv2.COLOR_RGB2BGR)
    palette = ((0, 210, 0), (0, 165, 255), (255, 120, 0), (180, 0, 220))
    for index, quad in enumerate(quads[:4]):
        points = np.rint(np.asarray(quad, dtype=np.float64)).astype(np.int32).reshape(-1, 1, 2)
        cv2.polylines(overlay, [points], True, palette[index % len(palette)], 3, lineType=cv2.LINE_AA)
    if registration.status == "accepted" and registration.pages:
        upper = next((page for page in registration.pages if page.name == "image_upper"), None)
        if upper is not None:
            seam = np.asarray(upper.source_quad[2:4], dtype=np.float64)
            points = np.rint(seam).astype(np.int32).reshape(-1, 1, 2)
            cv2.polylines(overlay, [points], False, (0, 255, 255), 3, lineType=cv2.LINE_AA)
    _private_dir(path.parent)
    if not cv2.imwrite(str(path), overlay, [cv2.IMWRITE_JPEG_QUALITY, 88]):
        raise ProbeError("private_overlay_write_failed")
    path.chmod(0o600)


def _load_rgb(path: Path) -> tuple[np.ndarray, str]:
    raw = path.read_bytes()
    try:
        with Image.open(path) as opened:
            rgb = np.asarray(ImageOps.exif_transpose(opened).convert("RGB"))
    except Exception as error:
        raise ProbeError("image_decode_failed") from error
    if rgb.dtype != np.uint8 or rgb.ndim != 3 or rgb.shape[2] != 3:
        raise ProbeError("image_format_unsupported")
    return rgb, hashlib.sha256(raw).hexdigest()


def _validate_baseline(path: Path, sample_id: str, expected_sha: str) -> dict[str, Any]:
    try:
        observation = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ProbeError("baseline_observations_missing_or_invalid") from error
    image = observation.get("image") if isinstance(observation, dict) else None
    runtime = observation.get("runtime") if isinstance(observation, dict) else None
    passes = observation.get("passes") if isinstance(observation, dict) else None
    expected_models = {
        "detector": base.MODEL_HASHES["det"][1],
        "recognizer": base.MODEL_HASHES["rec"][1],
    }
    if (
        observation.get("schema_version") != SCHEMA_V2
        or not isinstance(image, dict)
        or image.get("id") != sample_id
        or image.get("sha256") != expected_sha
        or image.get("coordinate_frame") != "exif_normalized_pixels"
        or not isinstance(runtime, dict)
        or runtime.get("model_sha256") != expected_models
        or not isinstance(passes, list)
        or not any(isinstance(item, dict) and item.get("kind") == "mrz" for item in passes)
        or not isinstance(observation.get("lines"), list)
    ):
        raise ProbeError("baseline_observations_sha_id_or_model_pin_mismatch")
    pass_kinds = {str(item.get("id")): item.get("kind") for item in passes if isinstance(item, dict)}
    text_quads = []
    for line in observation["lines"]:
        if not isinstance(line, dict) or pass_kinds.get(str(line.get("pass_id"))) in {"mrz", "right_margin_cw", "right_margin_ccw"}:
            continue
        try:
            quad = np.asarray(line.get("quad"), dtype=np.float64).reshape(4, 2)
        except (TypeError, ValueError):
            continue
        if np.isfinite(quad).all():
            text_quads.append(quad)
    if len(text_quads) < 4:
        raise ProbeError("baseline_text_geometry_insufficient")
    return {"observation": observation, "text_quads": text_quads}


def _validate_models(model_dir: Path) -> dict[str, Path]:
    if not model_dir.is_dir():
        raise ProbeError("local_model_directory_missing")
    models: dict[str, Path] = {}
    for role in ("det", "rec"):
        filename, expected_hash = base.MODEL_HASHES[role]
        path = model_dir / filename
        if not path.is_file() or _sha256(path) != expected_hash:
            raise ProbeError(f"local_model_pin_mismatch_{role}")
        models["detector" if role == "det" else "recognizer"] = path
    return models


def _canonical_label(text: str) -> str:
    mapped = "".join(LABEL_LOOKALIKE.get(char, char) for char in text.upper())
    return "".join(char for char in mapped if ("А" <= char <= "Я") or char == "Ё" or ("A" <= char <= "Z") or char.isdigit())


def _label_anchor_count(lines: list[dict[str, Any]]) -> int:
    """Count lines containing a strict existing label term; never emit text."""
    count = 0
    for line in lines:
        canonical = _canonical_label(str(line.get("text_raw", "")))
        if any(term in canonical for term in LABEL_TERMS):
            count += 1
    return count


def _map_engine_result(result: Any, page: RegisteredPage) -> list[dict[str, Any]]:
    texts = getattr(result, "txts", None) or []
    boxes = getattr(result, "boxes", None)
    scores = getattr(result, "scores", None)
    mapped: list[dict[str, Any]] = []
    if boxes is None:
        return mapped
    for index, text in enumerate(texts):
        if index >= len(boxes):
            continue
        local_quad = np.asarray(boxes[index], dtype=np.float64)
        source_quad = map_page_quad_to_original(local_quad, page)
        if source_quad is None:
            continue
        try:
            score = float(scores[index]) if scores is not None and index < len(scores) else 0.0
        except (TypeError, ValueError):
            score = 0.0
        if not np.isfinite(score):
            score = 0.0
        mapped.append({
            "text_raw": str(text),
            "score": max(0.0, min(1.0, score)),
            "quad_page": local_quad.reshape(4, 2).tolist(),
            "quad_original": source_quad,
        })
    return mapped


def _ocr_registered_pages(
    image_rgb: np.ndarray,
    registration: RegistrationResult,
    engine: Any,
    *,
    provider_validator: Callable[[Any], dict[str, Any]] | None = None,
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    """Run at most one CPU OCR call per registered page; useful with fake engines."""
    if registration.status != "accepted" or len(registration.pages) != 2:
        return [], {"status": "skipped", "reason": "registration_not_accepted", "elapsed_seconds": 0.0}
    if provider_validator is None:
        from ocr.experiments.region_pipeline import _assert_cpu_only
        provider_validator = _assert_cpu_only
    outputs: list[dict[str, Any]] = []
    total = 0.0
    providers: dict[str, Any] | None = None
    for page in registration.pages:
        page_rgb = warp_registered_page(image_rgb, page)
        page_bgr = cv2.cvtColor(page_rgb, cv2.COLOR_RGB2BGR)
        started = time.perf_counter()
        try:
            result = engine(page_bgr)
            elapsed = time.perf_counter() - started
            if providers is None:
                providers = provider_validator(engine)
            lines = _map_engine_result(result, page)
            status, issue = "ok", None
        except Exception:
            elapsed = time.perf_counter() - started
            lines, status, issue = [], "error", "page_ocr_or_provider_validation_failed"
        total += elapsed
        outputs.append({
            "name": page.name,
            "status": status,
            "issue_code": issue,
            "elapsed_seconds": elapsed,
            "output_size": list(page.output_size),
            "source_quad": page.source_quad,
            "original_to_page": page.original_to_page,
            "page_to_original": page.page_to_original,
            "lines": lines,
        })
        if providers is None:
            # Do not make another inference attempt unless actual nested
            # detector and recognizer sessions were verified as CPU-only.
            break
    return outputs, {"status": "ok" if all(item["status"] == "ok" for item in outputs) else "partial_error", "elapsed_seconds": total, "providers": providers}


def probe_image(
    *,
    sample_id: str,
    image_rgb: np.ndarray,
    image_sha256: str,
    baseline: dict[str, Any],
    engine: Any | None,
    det_sha256: str,
    rec_sha256: str,
    provider_validator: Callable[[Any], dict[str, Any]] | None = None,
    registration: RegistrationResult | None = None,
    geometry_only: bool = False,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Create one public-safe summary and one private diagnostic payload."""
    if registration is None:
        registration = register_spread(image_rgb, text_quads=baseline.get("text_quads", []))
    page_outputs: list[dict[str, Any]] = []
    ocr_info: dict[str, Any] = {"status": "not_run", "reason": "registration_rejected", "elapsed_seconds": 0.0, "providers": None}
    if registration.status == "accepted":
        if engine is None:
            reason = "geometry_only_requested" if geometry_only else "engine_unavailable"
            ocr_info = {"status": "not_run", "reason": reason, "elapsed_seconds": 0.0, "providers": None}
        else:
            page_outputs, ocr_info = _ocr_registered_pages(image_rgb, registration, engine, provider_validator=provider_validator)
    label_counts = {page["name"]: _label_anchor_count(page.get("lines", [])) for page in page_outputs}
    resolution_config = None if geometry_only else _rapidocr_resolution_config()
    public = {
        "sample_id": sample_id,
        "registration_status": registration.status,
        "registration_reason": registration.reason,
        "candidate_count": registration.candidate_count,
        "orientation_status": registration.orientation_status,
        "geometry": {
            "source_quad": registration.source_quad,
            "area_fraction": registration.area_fraction,
            "edge_support": list(registration.edge_support) if registration.edge_support is not None else None,
            "contrast_support": list(registration.contrast_support) if registration.contrast_support is not None else None,
            "texture_asymmetry": list(registration.texture_asymmetry) if registration.texture_asymmetry is not None else None,
            "seam_ratio": registration.seam_ratio,
            "seam_support": registration.seam_support,
            "candidate_score": registration.candidate_score,
            "stage_counts": registration.stage_counts,
        },
        "page_count": len(page_outputs),
        "ocr_status": ocr_info["status"],
        "ocr_seconds": round(float(ocr_info.get("elapsed_seconds", 0.0)), 6),
        "label_anchor_counts": label_counts,
        "line_count": sum(len(page.get("lines", [])) for page in page_outputs),
        "page_input_sizes": {page["name"]: page["output_size"] for page in page_outputs},
    }
    if not geometry_only:
        public["rapidocr_configured_resolution"] = resolution_config
        public["resolution_note"] = "Configured preprocessing only; tensor shapes and character pixel sizes were not measured."
    private = {
        "sample_id": sample_id,
        "source_sha256": image_sha256,
        "source_coordinate_frame": "exif_normalized_pixels",
        "runtime": {
            "opencv_version": cv2.__version__,
            "numpy_version": np.__version__,
        },
        "registration": registration.summary(),
        "ocr": ocr_info,
        "pages": page_outputs,
    }
    if not geometry_only:
        private["model_sha256"] = {"detector": det_sha256, "recognizer": rec_sha256}
        private["runtime"].update({
            "rapidocr_version": _package_version("rapidocr"),
            "onnxruntime_version": _package_version("onnxruntime"),
            "rapidocr_configured_resolution": resolution_config,
        })
    return public, private


def _package_version(name: str) -> str:
    try:
        return importlib.metadata.version(name)
    except importlib.metadata.PackageNotFoundError:
        return "unknown"


def _rapidocr_resolution_config() -> dict[str, Any]:
    """Read RapidOCR defaults without constructing an engine or loading a model."""
    try:
        import yaml
        config_text = importlib.resources.files("rapidocr").joinpath("config.yaml").read_text(encoding="utf-8")
        config = yaml.safe_load(config_text)
        global_cfg = config.get("Global", {})
        det_cfg = config.get("Det", {})
        return {
            "global_min_side_len": int(global_cfg["min_side_len"]),
            "global_max_side_len": int(global_cfg["max_side_len"]),
            "global_preprocess_enabled": bool(global_cfg["use_preprocess_img"]),
            "detector_limit_side_len": int(det_cfg["limit_side_len"]),
            "detector_limit_type": str(det_cfg["limit_type"]),
        }
    except Exception as error:
        raise ProbeError("rapidocr_resolution_config_unavailable") from error


def _public_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Page registration probe",
        "",
        f"- Status: **{report['status']}**",
        "- Scope: provisional physical-quad and seam diagnostics; no field assembly or accuracy claim.",
        "- Page roles are spatial (`image_upper`/`image_lower`); orientation is unverified.",
        "- Raw OCR text, image paths, hashes and transforms remain in the private work directory.",
        "",
        "| Sample | Registration | Reason | Candidates | Seam ratio | OCR | Lines | Label anchors |",
        "|---|---|---|---:|---:|---|---:|---|",
    ]
    for row in report.get("results", []):
        geometry = row.get("geometry", {})
        labels = row.get("label_anchor_counts", {})
        lines.append(
            f"| {row['sample_id']} | {row['registration_status']} | `{row['registration_reason']}` | "
            f"{row['candidate_count']} | {geometry.get('seam_ratio')} | {row['ocr_status']} | {row['line_count']} | "
            f"image_upper={labels.get('image_upper', 0)}, image_lower={labels.get('image_lower', 0)} |"
        )
    config = report.get("rapidocr_configured_resolution", {})
    if config:
        lines.append("")
        lines.append("RapidOCR values below are configured defaults; model tensor shapes and character pixel sizes were not measured.")
        lines.append(
            f"- Configured resolution: Global min_side_len={config.get('global_min_side_len')}, "
            f"max_side_len={config.get('global_max_side_len')}; "
            f"Det limit_side_len={config.get('detector_limit_side_len')}, "
            f"limit_type=`{config.get('detector_limit_type')}`."
        )
    return "\n".join(lines) + "\n"


def run_probe(
    *,
    images: list[Path],
    baseline_work_root: Path | None,
    model_dir: Path | None,
    work_root: Path,
    output_dir: Path,
    engine_factory: Callable[[Path, Path], Any] | None = None,
    provider_validator: Callable[[Any], dict[str, Any]] | None = None,
    geometry_only: bool = False,
) -> dict[str, Any]:
    if len(images) != 2 or len(set(path.resolve() for path in images)) != 2:
        raise ProbeError("exactly_two_distinct_images_required")
    for path in images:
        if not path.is_file():
            raise ProbeError("input_image_missing")
    if geometry_only and engine_factory is not None:
        raise ProbeError("geometry_only_cannot_initialize_engine")
    if not geometry_only and (baseline_work_root is None or model_dir is None):
        raise ProbeError("ocr_probe_inputs_required")
    models = {} if geometry_only else _validate_models(model_dir)  # type: ignore[arg-type]
    _private_dir(work_root)
    _private_dir(output_dir)
    prepared: list[dict[str, Any]] = []
    for index, image_path in enumerate(images, start=1):
        sample_id = f"sample-{index:02d}"
        rgb, image_hash = _load_rgb(image_path)
        if geometry_only:
            baseline = {"text_quads": []}
        else:
            assert baseline_work_root is not None
            baseline_path = baseline_work_root / sample_id / "observations.json"
            baseline = _validate_baseline(baseline_path, sample_id, image_hash)
        registration = register_spread(rgb, text_quads=baseline["text_quads"])
        prepared.append({
            "sample_id": sample_id,
            "image_path": image_path,
            "rgb": rgb,
            "image_sha256": image_hash,
            "baseline": baseline,
            "registration": registration,
        })

    # All source hashes, observation IDs and model pins are checked before any
    # model is initialized or OCR inference is attempted.
    results: list[dict[str, Any]] = []
    private_results: list[dict[str, Any]] = []
    engine: Any | None = None
    for item in prepared:
        if item["registration"].status == "accepted" and engine is None and engine_factory is not None:
            engine = engine_factory(models["detector"], models["recognizer"])
        selected_engine = engine if item["registration"].status == "accepted" else None
        result, private = probe_image(
            sample_id=item["sample_id"], image_rgb=item["rgb"], image_sha256=item["image_sha256"],
            baseline=item["baseline"], engine=selected_engine,
            det_sha256="" if geometry_only else _sha256(models["detector"]),
            rec_sha256="" if geometry_only else _sha256(models["recognizer"]),
            provider_validator=provider_validator, registration=item["registration"],
            geometry_only=geometry_only,
        )
        sample_work = work_root / item["sample_id"]
        _private_dir(sample_work)
        _write_geometry_overlay(sample_work / "registration-overlay.jpg", item["rgb"], item["registration"])
        for page_data in private["registration"].get("pages", []):
            page_output = sample_work / f"{page_data['name']}-page.png"
            if page_data is not None:
                registered_page = RegisteredPage(
                    name=page_data["name"], source_quad=page_data["source_quad"], output_size=tuple(page_data["output_size"]),
                    source_size=tuple(page_data["source_size"]),
                    original_to_page=page_data["original_to_page"], page_to_original=page_data["page_to_original"],
                )
                page_rgb = warp_registered_page(item["rgb"], registered_page)
                Image.fromarray(page_rgb).save(page_output, format="PNG")
                page_output.chmod(0o600)
        _write_json(sample_work / "registration-private.json", private, private=True)
        results.append(result)
        private_results.append(private)

    report = {
        "schema_version": PUBLIC_SCHEMA,
        "status": "PASS" if all(row["registration_status"] == "accepted" for row in results) else "PARTIAL",
        "scope": "two development images; provisional geometry only",
        "results": results,
    }
    if not geometry_only:
        report["rapidocr_configured_resolution"] = _rapidocr_resolution_config()
        report["resolution_note"] = "Configured preprocessing only; tensor shapes and character pixel sizes were not measured."
    private_report = {
        "schema_version": PRIVATE_SCHEMA,
        "status": report["status"],
        "source_paths": [str(path.resolve()) for path in images],
        "results": private_results,
    }
    if not geometry_only:
        assert model_dir is not None
        private_report["model_dir"] = str(model_dir.resolve())
        private_report["model_sha256"] = {"detector": _sha256(models["detector"]), "recognizer": _sha256(models["recognizer"])}
    _write_json(work_root / "registration-probe-private.json", private_report, private=True)
    _write_json(output_dir / "registration-probe.json", report, private=False)
    _write_markdown(output_dir / "registration-probe.md", _public_markdown(report))
    return report


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", action="append", type=Path, required=True, help="Source image; pass exactly twice")
    parser.add_argument("--baseline-work-root", type=Path, help="Private root containing sample-01/02 observations.json")
    parser.add_argument("--model-dir", type=Path, help="Already-installed pinned local model directory")
    parser.add_argument("--work-root", type=Path, required=True, help="Private diagnostic outputs (0700 directory)")
    parser.add_argument("--output-dir", type=Path, required=True, help="Aggregate-only reports (files mode 0600)")
    parser.add_argument("--geometry-only", action="store_true", help="Run registration only; do not initialize OCR/model engine")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        engine_factory = None
        if not args.geometry_only:
            from ocr.experiments.region_pipeline import create_cpu_engine
            engine_factory = create_cpu_engine
        report = run_probe(
            images=args.image,
            baseline_work_root=args.baseline_work_root,
            model_dir=args.model_dir,
            work_root=args.work_root,
            output_dir=args.output_dir,
            engine_factory=engine_factory,
            geometry_only=args.geometry_only,
        )
    except ProbeError as error:
        print(f"registration probe failed reason={error.reason}", file=sys.stderr)
        return 2
    except Exception:
        print("registration probe failed reason=internal_error", file=sys.stderr)
        return 2
    print(f"completed samples={len(report['results'])} status={report['status']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
