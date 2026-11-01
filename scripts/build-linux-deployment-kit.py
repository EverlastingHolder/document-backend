#!/usr/bin/env python3
"""Build a private, allowlisted, integrity-manifested Linux deployment kit."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath


EXPECTED_SOURCE_COMMIT = "8ea541f39209ef1e30e680204366041fcfdb0c84"
EXPECTED_SOURCE_VERSION = "4.10.0"
EXPECTED_MODEL_LOCK_VERSION = "v11"
SOURCE_PROVENANCE_PATH = "assets/russiandocs/SOURCE_PROVENANCE.json"
SOURCE_TEMPLATE_PREFIX = "pipeline_modules/page_registration/templates/"
ARCHIVE_TEMPLATE_PREFIX = "assets/russiandocs/document_processing/" + SOURCE_TEMPLATE_PREFIX
MANIFEST_PATH = "bundle-manifest.json"
CHECKSUMS_PATH = "SHA256SUMS"

PROJECT_EXACT_FILES = (
    ".dockerignore",
    "README.md",
    "go.mod",
    "go.sum",
    "compose.yaml",
    "compose.lan.yaml",
    "compose.production.yaml",
    "compose.managed-tls.yaml",
    "api/openapi.yaml",
    "deploy/README.md",
    "deploy/Dockerfile.api",
    "deploy/Dockerfile.migrate",
    "deploy/Dockerfile.ocr",
    "deploy/Dockerfile.ops",
    "deploy/Dockerfile.ocr.dockerignore",
    "deploy/api-entrypoint.sh",
    "deploy/ops-entrypoint.sh",
    "deploy/api.production.example.json",
    "deploy/api.queue.example.json",
    "deploy/ingress/Dockerfile",
    "deploy/ingress/Dockerfile.managed",
    "deploy/ingress/.dockerignore",
    "deploy/ingress/Caddyfile",
    "deploy/ingress/Caddyfile.lan",
    "deploy/ingress/Caddyfile.managed",
    "deploy/ingress/Dockerfile.lan",
    "deploy/ingress/entrypoint.sh",
    "deploy/ingress/entrypoint-managed.sh",
    "deploy/rabbitmq/Dockerfile",
    "deploy/rabbitmq/entrypoint.sh",
    "deploy/ocr/entrypoint.sh",
    "ocr/requirements-worker.txt",
    "ocr/README.md",
    "docs/WORKER_RUNTIME.md",
)
PROJECT_SOURCE_DIRS = ("cmd", "internal")
PROJECT_RUNTIME_PY = (
    "ocr/runtime/async_consumer.py",
    "ocr/runtime/russiandocs_worker.py",
    "ocr/runtime/secret_bootstrap.py",
)
PROJECT_OPTIONAL_ROOT_FILES = ("install.sh", "LINUX_DEPLOYMENT.md")
PROJECT_OPTIONAL_DEPLOY_DIRS = ("deploy/linux",)

FORBIDDEN_PARTS = frozenset({
    ".git", ".local", ".tmp", ".venv", ".venv-vl", "venv", "outputs",
    "datasets", "research", "experiments", "samples", "logs", "secrets",
    "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache",
})
FORBIDDEN_SUFFIXES = frozenset({
    ".jpg", ".jpeg", ".png", ".webp", ".tif", ".tiff", ".bmp", ".gif",
    ".heic", ".pdf", ".7z", ".zip", ".tar", ".gz", ".pem", ".key",
    ".p12", ".pfx", ".log", ".sqlite", ".db",
})
MODEL_ASSET_SUFFIXES = frozenset({".onnx", ".npz", ".json"})
RUNTIME_TEMPLATE_SUFFIXES = frozenset({".jpg", ".png", ".json"})
SAFE_DEPLOY_TEMPLATE_SUFFIXES = frozenset({
    ".sh", ".py", ".json", ".yaml", ".yml", ".md", ".conf", ".service",
    ".txt", ".example",
})
TOKEN_PATTERN = re.compile(rb"\b(?:pbt1|pwt1)_[A-Za-z0-9_-]{43}\b")
PRIVATE_KEY_PATTERN = re.compile(
    rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"
)
PASSWORD_DSN_PATTERN = re.compile(rb"\b(?:postgres|amqps?)://[^/@:\s]+:([^/@\s]+)@", re.I)


class KitError(Exception):
    """A pathless, safe package validation failure."""


@dataclass(frozen=True)
class InputFile:
    source: Path | None
    archive_path: str
    data: bytes | None = None
    executable: bool = False


def _safe_rel(value: str, *, model_asset: bool = False, runtime_template: bool = False) -> PurePosixPath:
    path = PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in ("", ".", "..") for part in path.parts):
        raise KitError("invalid_archive_path")
    if "\\" in value or any(any(ch.isspace() or ord(ch) < 0x20 for ch in part) for part in path.parts):
        raise KitError("invalid_archive_path")
    if any(part in FORBIDDEN_PARTS for part in path.parts):
        raise KitError("forbidden_archive_path")
    if model_asset and path.suffix.lower() not in MODEL_ASSET_SUFFIXES:
        raise KitError("unapproved_model_file_type")
    if runtime_template and path.suffix.lower() not in RUNTIME_TEMPLATE_SUFFIXES:
        raise KitError("unapproved_runtime_template_type")
    if path.suffix.lower() in FORBIDDEN_SUFFIXES and not model_asset and not runtime_template:
        raise KitError("forbidden_archive_file")
    return path


def _read_regular(path: Path) -> bytes:
    try:
        info = path.lstat()
    except OSError as exc:
        raise KitError("input_unavailable") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise KitError("input_not_regular")
    try:
        return path.read_bytes()
    except OSError as exc:
        raise KitError("input_unavailable") from exc


def _assert_no_symlink_components(root: Path, relative: PurePosixPath) -> Path:
    current = root
    for part in relative.parts:
        current = current / part
        try:
            info = current.lstat()
        except OSError as exc:
            raise KitError("input_unavailable") from exc
        if stat.S_ISLNK(info.st_mode):
            raise KitError("input_symlink_rejected")
    return current


def _hash(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _repo_files(repo: Path) -> list[InputFile]:
    selected: dict[str, InputFile] = {}

    def add_file(relative: str, *, executable: bool | None = None) -> None:
        path = _safe_rel(relative)
        disk_path = _assert_no_symlink_components(repo, path)
        data = _read_regular(disk_path)
        is_exec = bool(disk_path.stat().st_mode & 0o111) if executable is None else executable
        selected[path.as_posix()] = InputFile(disk_path, path.as_posix(), executable=is_exec)

    for relative in PROJECT_EXACT_FILES:
        disk_path = repo / relative
        if disk_path.exists():
            add_file(relative)
    required = {
        ".dockerignore", "go.mod", "go.sum", "compose.yaml", "compose.lan.yaml", "compose.production.yaml", "compose.managed-tls.yaml",
        "deploy/Dockerfile.api", "deploy/Dockerfile.migrate", "deploy/Dockerfile.ocr",
        "deploy/Dockerfile.ops", "deploy/ocr/entrypoint.sh", "deploy/api.production.example.json",
        "deploy/api.queue.example.json", "deploy/ingress/Dockerfile.managed",
        "deploy/ingress/.dockerignore", "deploy/ingress/Caddyfile.lan", "deploy/ingress/Dockerfile.lan", "deploy/ingress/Caddyfile.managed",
        "deploy/ingress/entrypoint-managed.sh",
    }
    if not required.issubset(selected):
        raise KitError("required_project_file_missing")

    for relative in PROJECT_OPTIONAL_ROOT_FILES:
        if (repo / relative).exists():
            add_file(relative, executable=True if relative == "install.sh" else None)

    for root_name in PROJECT_SOURCE_DIRS:
        directory = repo / root_name
        if directory.is_symlink() or not directory.is_dir():
            raise KitError("public_source_missing")
        for disk_path in directory.rglob("*"):
            if disk_path.is_symlink():
                raise KitError("project_symlink_rejected")
            if "__pycache__" in disk_path.parts or disk_path.suffix == ".pyc":
                continue
            if disk_path.is_dir():
                continue
            relative = disk_path.relative_to(repo).as_posix()
            _safe_rel(relative)
            if disk_path.suffix == ".go" and not disk_path.name.endswith("_test.go"):
                add_file(relative)
            elif (root_name == "internal" and relative.startswith(
                    "internal/jobstore/postgres/migrations/") and disk_path.suffix == ".sql"):
                add_file(relative)

    for relative in PROJECT_RUNTIME_PY:
        if (repo / relative).exists():
            add_file(relative)

    for deploy_dir in PROJECT_OPTIONAL_DEPLOY_DIRS:
        directory = repo / deploy_dir
        if not directory.exists():
            continue
        if directory.is_symlink():
            raise KitError("project_symlink_rejected")
        for disk_path in directory.rglob("*"):
            if disk_path.is_symlink():
                raise KitError("project_symlink_rejected")
            if "__pycache__" in disk_path.parts or disk_path.suffix == ".pyc":
                continue
            if disk_path.is_dir():
                continue
            relative = disk_path.relative_to(repo).as_posix()
            path = _safe_rel(relative)
            if path.suffix.lower() not in SAFE_DEPLOY_TEMPLATE_SUFFIXES and path.name not in {
                "install-target.sh", "install.sh"
            }:
                raise KitError("unapproved_deploy_file_type")
            add_file(relative, executable=True if path.name == "install-target.sh" else None)

    # A2's installer is a required part of a final bundle, but dry-run inventory
    # is useful while its files are still being authored.
    if "install.sh" not in selected or "deploy/linux/install-target.sh" not in selected:
        selected["__incomplete_installer__"] = InputFile(None, "__incomplete_installer__", b"")

    return [selected[key] for key in sorted(selected) if key != "__incomplete_installer__"]


def _validate_source(source: Path) -> tuple[str, str, bool, list[InputFile]]:
    if source.is_symlink() or not source.is_dir():
        raise KitError("source_root_invalid")
    try:
        commit = subprocess.run(
            ["git", "-C", str(source), "rev-parse", "HEAD"],
            check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        ).stdout.strip()
        status = subprocess.run(
            ["git", "-C", str(source), "status", "--porcelain=v1", "--untracked-files=all"],
            check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        ).stdout
    except (OSError, subprocess.CalledProcessError) as exc:
        raise KitError("source_provenance_unavailable") from exc
    clean = not status.strip()
    if commit != EXPECTED_SOURCE_COMMIT or not clean:
        raise KitError("source_revision_mismatch")
    init_path = source / "document_processing" / "__init__.py"
    init_data = _read_regular(init_path)
    match = re.search(rb"(?m)^__version__\s*=\s*['\"]([^'\"]+)['\"]", init_data)
    if match is None:
        raise KitError("source_version_unavailable")
    version = match.group(1).decode("ascii", errors="strict")
    if version != EXPECTED_SOURCE_VERSION:
        raise KitError("source_version_mismatch")
    if not (source / "LICENSE").is_file() or not (source / "NOTICE").is_file():
        raise KitError("source_license_unavailable")

    package = source / "document_processing"
    if package.is_symlink() or not package.is_dir():
        raise KitError("source_package_invalid")
    template_root = package / SOURCE_TEMPLATE_PREFIX
    if template_root.is_symlink() or not template_root.is_dir():
        raise KitError("source_templates_unavailable")
    template_rel_files: set[str] = set()
    for template_path in template_root.rglob("*"):
        if template_path.is_symlink():
            raise KitError("source_symlink_rejected")
        if template_path.is_dir():
            continue
        relative_template = template_path.relative_to(template_root).as_posix()
        _safe_rel(relative_template, runtime_template=True)
        template_rel_files.add(relative_template)
    _validate_template_references(template_root, template_rel_files)

    source_files: list[InputFile] = []
    for disk_path in package.rglob("*"):
        if disk_path.is_symlink():
            raise KitError("source_symlink_rejected")
        if disk_path.is_dir():
            continue
        relative = disk_path.relative_to(source).as_posix()
        relative_in_package = disk_path.relative_to(package).as_posix()
        if "__pycache__" in disk_path.parts or disk_path.suffix == ".pyc":
            continue
        is_template_asset = relative_in_package.startswith(SOURCE_TEMPLATE_PREFIX)
        if is_template_asset:
            _safe_rel(relative_in_package, runtime_template=True)
        elif disk_path.suffix.lower() not in {".py", ".json", ".yaml", ".yml"}:
            raise KitError("unapproved_source_file_type")
        rel = _safe_rel(
            "assets/russiandocs/" + relative,
            runtime_template=is_template_asset,
        )
        _assert_no_symlink_components(source, PurePosixPath(relative))
        _read_regular(disk_path)
        source_files.append(InputFile(disk_path, rel.as_posix(), executable=False))

    for name in ("LICENSE", "NOTICE"):
        disk_path = source / name
        _read_regular(disk_path)
        source_files.append(InputFile(disk_path, f"assets/russiandocs/{name}", executable=False))

    template_files = [item for item in source_files if item.archive_path.startswith(
        "assets/russiandocs/document_processing/" + SOURCE_TEMPLATE_PREFIX
    )]
    provenance = {
        "commit": commit,
        "version": version,
        "clean": clean,
        "license_files": ["LICENSE", "NOTICE"],
        "runtime_template_files_count": len(template_files),
        "runtime_template_bytes": sum(item.source.stat().st_size for item in template_files if item.source),
    }
    provenance_data = (json.dumps(provenance, sort_keys=True, separators=(",", ":")) + "\n").encode()
    source_files.append(InputFile(None, SOURCE_PROVENANCE_PATH, provenance_data, False))
    return commit, version, clean, source_files


def _validate_models(models_root: Path, lock_path: Path) -> tuple[dict, bytes, list[InputFile]]:
    if not models_root.is_dir() or models_root.is_symlink():
        raise KitError("models_root_invalid")
    lock_bytes = _read_regular(lock_path)
    try:
        lock = json.loads(lock_bytes)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise KitError("model_lock_invalid") from exc
    if not isinstance(lock, dict) or lock.get("models_version") != EXPECTED_MODEL_LOCK_VERSION:
        raise KitError("model_lock_version_mismatch")
    entries = lock.get("files")
    if not isinstance(entries, list) or not entries:
        raise KitError("model_lock_invalid")
    selected: dict[str, InputFile] = {}
    locked_paths: set[str] = set()
    total_bytes = 0
    for entry in entries:
        if not isinstance(entry, dict):
            raise KitError("model_lock_invalid")
        relative = entry.get("path")
        size = entry.get("size")
        digest = entry.get("sha256")
        if (not isinstance(relative, str) or not relative or isinstance(size, bool) or
                not isinstance(size, int) or size <= 0 or not isinstance(digest, str) or
                not re.fullmatch(r"[0-9a-f]{64}", digest)):
            raise KitError("model_lock_invalid")
        rel = _safe_rel(relative, model_asset=True)
        if rel.as_posix() in locked_paths:
            raise KitError("model_lock_duplicate_path")
        locked_paths.add(rel.as_posix())
        disk_path = _assert_no_symlink_components(models_root, rel)
        data = _read_regular(disk_path)
        if len(data) != size or _hash(data) != digest:
            raise KitError("model_asset_hash_mismatch")
        archive_rel = _safe_rel("assets/models/" + rel.as_posix(), model_asset=True).as_posix()
        selected[archive_rel] = InputFile(disk_path, archive_rel, executable=False)
        total_bytes += size

    # No unlocked files may be smuggled into the model payload.
    actual_paths: set[str] = set()
    for disk_path in models_root.rglob("*"):
        if disk_path.is_symlink():
            raise KitError("model_asset_symlink")
        if disk_path.is_file():
            actual_paths.add(disk_path.relative_to(models_root).as_posix())
    if actual_paths != locked_paths:
        raise KitError("unlocked_model_asset")
    return lock, lock_bytes, [selected[key] for key in sorted(selected)]


def _validate_template_references(template_root: Path, allowed_files: set[str]) -> None:
    for relative in sorted(allowed_files):
        if PurePosixPath(relative).suffix.lower() != ".json":
            continue
        path = template_root / relative
        try:
            value = json.loads(_read_regular(path))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise KitError("runtime_template_invalid") from exc

        def visit(item: object) -> None:
            if isinstance(item, dict):
                for key, child in item.items():
                    if key in {"image", "mask"} and isinstance(child, str):
                        target = PurePosixPath(child)
                        if target.is_absolute() or any(part in ("", ".", "..") for part in target.parts):
                            raise KitError("runtime_template_reference_invalid")
                        if target.as_posix() not in allowed_files:
                            raise KitError("runtime_template_reference_invalid")
                        resolved = template_root.joinpath(*target.parts)
                        _read_regular(resolved)
                    visit(child)
            elif isinstance(item, list):
                for child in item:
                    visit(child)

        visit(value)


def _content_scan(path: str, data: bytes) -> None:
    lower = path.lower()
    if any(part in FORBIDDEN_PARTS for part in PurePosixPath(lower).parts):
        raise KitError("forbidden_archive_path")
    is_model_asset = lower.startswith("assets/models/")
    is_runtime_template = lower.startswith(ARCHIVE_TEMPLATE_PREFIX.lower())
    if is_model_asset and PurePosixPath(lower).suffix not in MODEL_ASSET_SUFFIXES:
        raise KitError("unapproved_model_file_type")
    if is_runtime_template and PurePosixPath(lower).suffix not in RUNTIME_TEMPLATE_SUFFIXES:
        raise KitError("unapproved_runtime_template_type")
    if PurePosixPath(lower).suffix in FORBIDDEN_SUFFIXES and not is_model_asset and not is_runtime_template:
        raise KitError("forbidden_archive_file")
    has_password_dsn = any(
        b"{" not in match.group(1) and b"}" not in match.group(1)
        for match in PASSWORD_DSN_PATTERN.finditer(data)
    )
    if TOKEN_PATTERN.search(data) or PRIVATE_KEY_PATTERN.search(data) or has_password_dsn:
        raise KitError("credential_material_detected")
    if re.search(rb"/(?:Users|home)/[A-Za-z0-9._-]+/", data):
        raise KitError("host_path_detected")


def collect_inventory(repo: Path, source: Path, models_root: Path) -> tuple[list[InputFile], dict]:
    if source.is_symlink() or models_root.is_symlink():
        raise KitError("asset_root_symlink")
    repo = repo.resolve(strict=True)
    source = source.resolve(strict=True)
    models_root = models_root.resolve(strict=True)
    project = _repo_files(repo)
    commit, version, clean, source_files = _validate_source(source)
    source_lock_path = source / "document_processing" / "models.lock.json"
    lock, lock_bytes, model_files = _validate_models(models_root, source_lock_path)
    files = project + source_files + model_files
    archive_names = [item.archive_path for item in files]
    if len(set(archive_names)) != len(archive_names) or MANIFEST_PATH in archive_names or CHECKSUMS_PATH in archive_names:
        raise KitError("duplicate_archive_path")
    for item in files:
        _safe_rel(
            item.archive_path,
            model_asset=item.archive_path.startswith("assets/models/"),
            runtime_template=item.archive_path.startswith(ARCHIVE_TEMPLATE_PREFIX),
        )
        data = item.data if item.data is not None else _read_regular(item.source)  # type: ignore[arg-type]
        _content_scan(item.archive_path, data)
    lock_digest = _hash(lock_bytes)
    public_repo_revision = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "HEAD"], check=True,
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
    ).stdout.strip()
    manifest = {
        "schema_version": 1,
        "bundle": {
            "format": "tar.gz",
            "project_base_revision": public_repo_revision,
            "project_snapshot": "working_tree",
            "checksums": CHECKSUMS_PATH,
        },
        "source": {
            "commit": commit,
            "version": version,
            "clean": clean,
            "files_count": len(source_files),
            "package_files_count": sum(
                item.archive_path.startswith("assets/russiandocs/document_processing/") for item in source_files
            ),
            "license_files_count": 2,
        },
        "models": {
            "lock_version": lock["models_version"],
            "lock_path": "assets/russiandocs/document_processing/models.lock.json",
            "lock_sha256": lock_digest,
            "files_count": len(model_files),
            "total_bytes": sum(item.source.stat().st_size for item in model_files if item.source is not None),
        },
        "files": [],
    }
    manifest["source"]["templates_files_count"] = sum(
        item.archive_path.startswith(ARCHIVE_TEMPLATE_PREFIX) for item in source_files
    )
    manifest["source"]["templates_bytes"] = sum(
        item.source.stat().st_size for item in source_files
        if item.source is not None and item.archive_path.startswith(ARCHIVE_TEMPLATE_PREFIX)
    )
    for item in sorted(files, key=lambda entry: entry.archive_path):
        data = item.data if item.data is not None else _read_regular(item.source)  # type: ignore[arg-type]
        digest = _hash(data)
        manifest["files"].append({"path": item.archive_path, "size": len(data), "sha256": digest})
    manifest["files"].sort(key=lambda entry: entry["path"])
    manifest_data = _manifest_bytes(manifest)
    checksum_entries = [*manifest["files"], {
        "path": MANIFEST_PATH,
        "size": len(manifest_data),
        "sha256": _hash(manifest_data),
    }]
    checksums_data = "".join(
        f"{entry['sha256']}  {entry['path']}\n" for entry in sorted(checksum_entries, key=lambda entry: entry["path"])
    ).encode("ascii")
    files.append(InputFile(None, CHECKSUMS_PATH, checksums_data, False))
    return files, manifest


def _manifest_bytes(manifest: dict) -> bytes:
    return (json.dumps(manifest, ensure_ascii=True, sort_keys=True, separators=(",", ":")) + "\n").encode()


def _hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1 << 20), b""):
                digest.update(chunk)
    except OSError as exc:
        raise KitError("archive_read_failed") from exc
    return digest.hexdigest()


def _validate_output_parent(output: Path, repo: Path) -> Path:
    expected = repo / ".local" / "linux-deployment"
    local_root = repo / ".local"
    if local_root.is_symlink() or not local_root.is_dir():
        raise KitError("output_root_not_private")
    local_info = local_root.stat()
    if stat.S_IMODE(local_info.st_mode) != 0o700 or local_info.st_uid != os.geteuid():
        raise KitError("output_root_not_private")
    if not expected.exists():
        expected.mkdir(mode=0o700)
    if expected.is_symlink():
        raise KitError("output_parent_not_private")
    expected_info = expected.stat()
    if stat.S_IMODE(expected_info.st_mode) != 0o700 or expected_info.st_uid != os.geteuid():
        raise KitError("output_parent_not_private")
    if output.parent.absolute() != expected.absolute():
        raise KitError("output_parent_not_private")
    parent = output.parent.resolve(strict=True)
    info = parent.stat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.geteuid():
        raise KitError("output_parent_not_private")
    if output.exists() or output.is_symlink():
        raise KitError("output_already_exists")
    return parent


def write_bundle(output: Path, files: list[InputFile], manifest: dict, repo: Path) -> None:
    parent = _validate_output_parent(output, repo)
    payload = _manifest_bytes(manifest)
    expected = {entry["path"]: (entry["size"], entry["sha256"]) for entry in manifest["files"]}
    checksum_entries = [*manifest["files"], {
        "path": MANIFEST_PATH,
        "size": len(payload),
        "sha256": _hash(payload),
    }]
    expected_checksums = "".join(
        f"{entry['sha256']}  {entry['path']}\n" for entry in sorted(checksum_entries, key=lambda entry: entry["path"])
    ).encode("ascii")
    fd, temp_name = tempfile.mkstemp(prefix=".kit-", suffix=".tmp", dir=parent)
    temp_path = Path(temp_name)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb", closefd=True) as raw:
            with gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0, compresslevel=1) as compressed:
                with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.PAX_FORMAT) as archive:
                    for item in sorted(files, key=lambda entry: entry.archive_path):
                        data = item.data if item.data is not None else _read_regular(item.source)  # type: ignore[arg-type]
                        _content_scan(item.archive_path, data)
                        if item.archive_path == CHECKSUMS_PATH:
                            if data != expected_checksums:
                                raise KitError("checksums_changed_during_build")
                        elif (len(data), _hash(data)) != expected.get(item.archive_path):
                            raise KitError("input_changed_during_build")
                        info = tarfile.TarInfo(item.archive_path)
                        info.size = len(data)
                        info.mtime = 0
                        info.uid = 0
                        info.gid = 0
                        info.uname = ""
                        info.gname = ""
                        info.mode = 0o755 if item.executable else 0o644
                        import io
                        archive.addfile(info, io.BytesIO(data))
                    info = tarfile.TarInfo(MANIFEST_PATH)
                    info.size = len(payload)
                    info.mtime = 0
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    info.mode = 0o644
                    import io
                    archive.addfile(info, io.BytesIO(payload))
            raw.flush()
            os.fsync(raw.fileno())
        os.link(temp_path, output)
        temp_path.unlink()
        os.chmod(output, 0o600)
        dir_fd = os.open(parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except Exception:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass
        raise


def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--source-root", type=Path, required=True)
    parser.add_argument("--models-root", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--json", action="store_true", help="print aggregate-only inventory JSON")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    try:
        files, manifest = collect_inventory(args.repo_root, args.source_root, args.models_root)
        has_installer = any(item.archive_path == "install.sh" for item in files) and any(
            item.archive_path == "deploy/linux/install-target.sh" for item in files
        )
        has_checksums = any(item.archive_path == CHECKSUMS_PATH for item in files)
        if not args.dry_run and not has_installer:
            raise KitError("installer_files_missing")
        manifest_bytes = _manifest_bytes(manifest)
        checksum_item = next((item for item in files if item.archive_path == CHECKSUMS_PATH), None)
        if checksum_item is None or checksum_item.data is None:
            raise KitError("checksums_unavailable")
        total_bytes = sum(entry["size"] for entry in manifest["files"]) + len(checksum_item.data) + len(manifest_bytes)
        inventory = {
            "status": "ready" if has_installer and has_checksums else "waiting_for_installer_files",
            "file_count": len(files) + 1,
            "uncompressed_bytes": total_bytes,
            "source_version": manifest["source"]["version"],
            "source_commit": manifest["source"]["commit"],
            "model_lock_version": manifest["models"]["lock_version"],
            "model_file_count": manifest["models"]["files_count"],
            "model_bytes": manifest["models"]["total_bytes"],
        }
        if args.dry_run:
            print(json.dumps(inventory if args.json else {"status": inventory["status"], **{
                key: value for key, value in inventory.items() if key not in ("source_commit",)
            }}, sort_keys=True))
            return 0
        if args.output is None:
            raise KitError("output_required")
        write_bundle(args.output, files, manifest, args.repo_root.resolve(strict=True))
        inventory["archive_bytes"] = args.output.stat().st_size
        inventory["archive_sha256"] = _hash_file(args.output)
        print(json.dumps(inventory, sort_keys=True))
        return 0
    except KitError as exc:
        print(json.dumps({"status": "error", "category": str(exc)}, sort_keys=True), file=sys.stderr)
        return 2
    except Exception:
        print(json.dumps({"status": "error", "category": "bundle_failed"}, sort_keys=True), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
