#!/usr/bin/env python3
"""Validate and materialize the pinned public OCR source/model snapshot.

The caller fetches the source checkout and its locked model files first. This
script re-validates that checkout with the deployment-kit validator and copies
only its allowlisted runtime source and exact model-lock entries into a clean
output directory for the portable OCR image.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import shutil
import stat
import sys
from pathlib import Path

_BUILDER_PATH = Path(__file__).with_name("build-linux-deployment-kit.py")
_BUILDER_SPEC = importlib.util.spec_from_file_location("build_linux_deployment_kit", _BUILDER_PATH)
if _BUILDER_SPEC is None or _BUILDER_SPEC.loader is None:
    raise RuntimeError("asset_validator_unavailable")
kit = importlib.util.module_from_spec(_BUILDER_SPEC)
sys.modules[_BUILDER_SPEC.name] = kit
_BUILDER_SPEC.loader.exec_module(kit)


SOURCE_PREFIX = "assets/russiandocs/"
MODEL_PREFIX = "assets/models/"
MANIFEST_NAME = "portable-assets.json"


class AssetError(Exception):
    """A bounded, pathless asset packaging failure."""


def _digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _payload_bytes(item: kit.InputFile) -> bytes:
    return item.data if item.data is not None else kit._read_regular(item.source)  # type: ignore[arg-type]


def _write_new(root: Path, relative: str, data: bytes) -> None:
    try:
        rel = kit._safe_rel(relative, model_asset=relative.startswith("models/"),
                            runtime_template=relative.startswith("russiandocs/document_processing/pipeline_modules/page_registration/templates/"))
    except kit.KitError as exc:
        raise AssetError(str(exc)) from exc
    target = root.joinpath(*rel.parts)
    target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    current = root
    for part in rel.parts[:-1]:
        current = current / part
        info = current.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise AssetError("output_directory_invalid")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(target, flags, 0o644)
    except OSError as exc:
        raise AssetError("output_file_create_failed") from exc
    try:
        view = memoryview(data)
        while view:
            count = os.write(fd, view)
            view = view[count:]
        os.fsync(fd)
        os.fchmod(fd, 0o644)
    finally:
        os.close(fd)
    os.utime(target, ns=(0, 0), follow_symlinks=False)


def materialize(source_root: Path, models_root: Path, output_root: Path) -> dict:
    """Copy only validated RussianDocsOCR source files and locked model files."""
    if output_root.exists() or output_root.is_symlink():
        raise AssetError("output_root_must_be_new")
    try:
        source_root = source_root.resolve(strict=True)
        models_root = models_root.resolve(strict=True)
    except OSError as exc:
        raise AssetError("asset_root_unavailable") from exc

    try:
        commit, version, clean, source_files = kit._validate_source(source_root)
        lock_path = source_root / "document_processing" / "models.lock.json"
        lock, lock_bytes, model_files = kit._validate_models(models_root, lock_path)
    except kit.KitError as exc:
        raise AssetError(str(exc)) from exc
    if not clean or commit != kit.EXPECTED_SOURCE_COMMIT or version != kit.EXPECTED_SOURCE_VERSION:
        raise AssetError("source_pin_mismatch")
    if lock.get("models_version") != kit.EXPECTED_MODEL_LOCK_VERSION:
        raise AssetError("model_lock_version_mismatch")

    created_root = False
    try:
        output_root.mkdir(mode=0o755, parents=False)
        created_root = True
    except OSError as exc:
        raise AssetError("output_root_create_failed") from exc

    entries: list[dict[str, object]] = []
    try:
        payload = sorted((*source_files, *model_files), key=lambda item: item.archive_path)
        seen: set[str] = set()
        for item in payload:
            path = item.archive_path
            if path.startswith(SOURCE_PREFIX):
                relative = "russiandocs/" + path.removeprefix(SOURCE_PREFIX)
            elif path.startswith(MODEL_PREFIX):
                relative = "models/" + path.removeprefix(MODEL_PREFIX)
            else:
                raise AssetError("unapproved_asset_root")
            if relative in seen:
                raise AssetError("duplicate_asset_path")
            seen.add(relative)
            data = _payload_bytes(item)
            try:
                kit._content_scan("assets/" + relative, data)
            except kit.KitError as exc:
                raise AssetError(str(exc)) from exc
            _write_new(output_root, relative, data)
            entries.append({"path": relative, "size": len(data), "sha256": _digest(data)})

        manifest = {
            "schema_version": 1,
            "source": {"commit": commit, "version": version, "clean": clean},
            "models": {
                "lock_version": lock["models_version"],
                "lock_sha256": _digest(lock_bytes),
                "files_count": len(model_files),
                "total_bytes": sum(int(item["size"]) for item in entries if str(item["path"]).startswith("models/")),
            },
            "files": entries,
        }
        manifest_data = (json.dumps(manifest, ensure_ascii=True, sort_keys=True,
                                    separators=(",", ":")) + "\n").encode("ascii")
        _write_new(output_root, MANIFEST_NAME, manifest_data)
        directories = [path for path in output_root.rglob("*") if path.is_dir()]
        for directory in sorted(directories, key=lambda path: len(path.parts), reverse=True):
            os.chmod(directory, 0o755)
            os.utime(directory, ns=(0, 0), follow_symlinks=False)
        os.chmod(output_root, 0o755)
        os.utime(output_root, ns=(0, 0), follow_symlinks=False)
        dir_fd = os.open(output_root, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
        return manifest
    except Exception:
        # The directory is an ephemeral Docker build-stage output. Fail closed;
        # the stage is discarded by Docker if validation/materialization fails.
        if created_root and not output_root.is_symlink():
            shutil.rmtree(output_root, ignore_errors=True)
        raise


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-root", type=Path, required=True)
    parser.add_argument("--models-root", type=Path, required=True)
    parser.add_argument("--output-root", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        manifest = materialize(args.source_root, args.models_root, args.output_root)
        print(json.dumps({
            "status": "ready",
            "source_version": manifest["source"]["version"],
            "models_version": manifest["models"]["lock_version"],
            "model_files": manifest["models"]["files_count"],
            "model_bytes": manifest["models"]["total_bytes"],
            "payload_files": len(manifest["files"]),
        }, sort_keys=True))
        return 0
    except AssetError as exc:
        print(json.dumps({"status": "error", "category": str(exc)}, sort_keys=True), file=sys.stderr)
        return 2
    except Exception:
        print(json.dumps({"status": "error", "category": "asset_materialization_failed"},
                         sort_keys=True), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
