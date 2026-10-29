#!/usr/bin/env python3
"""One-image PaddleOCR-VL worker; receives no gold or field targets."""

from __future__ import annotations

import argparse
import json
import os
import platform
import time
from pathlib import Path
from typing import Any


def _private_dir(path: Path) -> None:
    missing: list[Path] = []
    cursor = path
    while not cursor.exists():
        missing.append(cursor)
        cursor = cursor.parent
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    for created in missing:
        created.chmod(0o700)
    path.chmod(0o700)


def _private_json(path: Path, value: Any) -> None:
    _private_dir(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def run(input_path: Path, output_path: Path, cache_root: Path) -> int:
    input_path = input_path.resolve(strict=True)
    output_path = output_path.resolve()
    cache_root = cache_root.resolve()
    if not input_path.is_file() or not output_path.parent.is_dir() or not cache_root.is_dir():
        raise RuntimeError("worker_path_preflight_failed")
    expected_model_cache = str(cache_root / "paddlex")
    if os.environ.get("PADDLE_PDX_CACHE_HOME") != expected_model_cache:
        raise RuntimeError("worker_private_model_cache_mismatch")

    import paddle
    from paddleocr import PaddleOCRVL

    if paddle.is_compiled_with_cuda():
        raise RuntimeError("cuda_build_not_allowed")
    paddle.set_device("cpu")
    if not paddle.device.get_device().startswith("cpu"):
        raise RuntimeError("cpu_device_not_selected")

    # Keep vendor layout, orientation and unwarping defaults. The full pipeline
    # uses local PaddlePaddle CPU inference and receives no prompt or field data.
    init_started = time.perf_counter()
    pipeline = PaddleOCRVL(pipeline_version="v1.6", device="cpu")
    init_seconds = time.perf_counter() - init_started

    predict_started = time.perf_counter()
    results = list(pipeline.predict(str(input_path)))
    predict_seconds = time.perf_counter() - predict_started
    if len(results) != 1:
        raise RuntimeError("worker_expected_one_result")
    result = results[0]
    if not hasattr(result, "save_to_json"):
        raise RuntimeError("worker_result_serializer_missing")
    result.save_to_json(save_path=str(output_path))
    if not output_path.is_file():
        raise RuntimeError("worker_result_file_missing")
    output_path.chmod(0o600)

    # Record vendor-reported effective options when present; retain configured
    # defaults separately so absence is not presented as an observed value.
    try:
        result_data = result.json
        if isinstance(result_data, dict) and isinstance(result_data.get("res"), dict):
            result_data = result_data["res"]
    except Exception:
        result_data = None
    observed_options = {}
    if isinstance(result_data, dict):
        for name in (
            "model_settings", "use_doc_preprocessor", "use_layout_detection",
            "use_doc_orientation_classify", "use_doc_unwarping", "format_block_content",
        ):
            if name in result_data:
                observed_options[name] = result_data[name]
    metadata = {
        "schema_version": "passport-paddleocr-vl-worker/v1",
        "configured": {
            "pipeline_version": "v1.6",
            "device": "cpu",
            "inference_backend": "local_paddlepaddle",
            "max_new_tokens": None,
            "use_layout_detection": "vendor_default_true",
            "use_doc_orientation_classify": "vendor_default_false",
            "use_doc_unwarping": "vendor_default_false",
        },
        "observed_options": observed_options,
        "versions": {
            "python": platform.python_version(),
            "paddle": paddle.__version__,
            "device": paddle.device.get_device(),
            "cuda_compiled": paddle.is_compiled_with_cuda(),
        },
        "model_init_seconds": round(init_seconds, 3),
        "predict_seconds": round(predict_seconds, 3),
    }
    _private_json(output_path.with_name("worker-metadata.json"), metadata)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cache-root", type=Path, required=True)
    args = parser.parse_args(argv)
    return run(args.input, args.output, args.cache_root)


if __name__ == "__main__":
    raise SystemExit(main())
