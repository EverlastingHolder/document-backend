#!/usr/bin/env python3
"""Copy mounted secrets to private tmpfs, then drop privileges before OCR."""

from __future__ import annotations

import json
import os
import secrets
import shutil
import stat
import sys
from pathlib import Path
from typing import Any

MAX_CONFIG_BYTES = 64 * 1024
MAX_SECRET_BYTES = 8192
PATH_FIELDS = (
    "rabbit_password_file", "worker_token_file", "image_root", "russiandocs_repo",
    "models_root", "work_root",
)
SECRET_SOURCES = {
    "rabbit_password_file": Path("/run/secrets/rabbitmq_password"),
    "worker_token_file": Path("/run/secrets/ocr_worker_token"),
}


def _no_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate field")
        result[key] = value
    return result


def _read_regular_private(path: Path, max_bytes: int, require_private: bool) -> bytes:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or (require_private and info.st_mode & 0o077):
            raise ValueError("file mode")
        data = bytearray()
        while len(data) <= max_bytes:
            chunk = os.read(fd, min(4096, max_bytes + 1 - len(data)))
            if not chunk:
                break
            data.extend(chunk)
        if len(data) > max_bytes:
            raise ValueError("file size")
        return bytes(data)
    finally:
        os.close(fd)


def _write_private_file(directory: Path, name: str, data: bytes, uid: int, gid: int) -> Path:
    target = directory / name
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(target, flags, 0o600)
    try:
        os.fchown(fd, uid, gid)
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(data):
            offset += os.write(fd, data[offset:])
        os.fsync(fd)
    finally:
        os.close(fd)
    return target


def _replace_private_file(directory: Path, name: str, data: bytes, uid: int, gid: int) -> Path:
    target = directory / name
    try:
        info = target.lstat()
    except FileNotFoundError:
        info = None
    if info is not None and (
        not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or
        info.st_mode & 0o077 or info.st_uid != uid
    ):
        raise ValueError("existing runtime file")
    temporary_name = "." + name + "." + secrets.token_hex(12)
    temporary = _write_private_file(directory, temporary_name, data, uid, gid)
    try:
        os.replace(temporary, target)
        directory_fd = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
    return target


def prepare(config_path: Path, uid: int, gid: int, run_root: Path = Path("/run/passport-ocr")) -> Path:
    if os.geteuid() != 0 or uid <= 0 or gid <= 0:
        raise ValueError("bootstrap privilege configuration")
    config_data = _read_regular_private(config_path, MAX_CONFIG_BYTES, require_private=False)
    values = json.loads(config_data, object_pairs_hook=_no_duplicate_pairs)
    if not isinstance(values, dict):
        raise ValueError("config object")
    for key in PATH_FIELDS:
        value = values.get(key)
        if not isinstance(value, str) or not value:
            raise ValueError("config path")
        path = Path(value)
        if not path.is_absolute():
            path = config_path.parent / path
        if key in SECRET_SOURCES and path != SECRET_SOURCES[key]:
            raise ValueError("secret source is not mounted secret")
        if stat.S_ISLNK(path.lstat().st_mode):
            raise ValueError("symlink path")
        values[key] = str(path.resolve(strict=True))

    if stat.S_ISLNK(run_root.lstat().st_mode):
        raise ValueError("tmpfs path")
    root = run_root.resolve(strict=True)
    root_info = root.lstat()
    if not stat.S_ISDIR(root_info.st_mode) or root_info.st_mode & 0o077:
        raise ValueError("tmpfs mode")
    os.chown(root, uid, gid)
    os.chmod(root, 0o700)
    workdir = root / ("passport-ocr-" + secrets.token_hex(12))
    workdir.mkdir(mode=0o700)
    os.chown(workdir, uid, gid)
    os.chmod(workdir, 0o700)

    try:
        for field, filename in (("rabbit_password_file", "rabbit-password"), ("worker_token_file", "worker-token")):
            source = Path(values[field])
            data = _read_regular_private(source, MAX_SECRET_BYTES, require_private=True)
            copied = _write_private_file(workdir, filename, data, uid, gid)
            values[field] = str(copied)

        if not isinstance(values.get("readiness_marker"), str):
            raise ValueError("readiness marker config")
        values["readiness_marker"] = str(root / "ready")
        copied_config = _replace_private_file(
            root, "worker-config.json",
            json.dumps(values, ensure_ascii=False, separators=(",", ":")).encode("utf-8"), uid, gid,
        )
        return copied_config
    except Exception:
        shutil.rmtree(workdir, ignore_errors=True)
        raise


def healthcheck(config_path: Path, uid: int, gid: int, run_root: Path = Path("/run/passport-ocr")) -> int:
    """Drop to the worker identity before running the silent marker probe."""
    try:
        if os.geteuid() != 0 or uid <= 0 or gid <= 0 or stat.S_ISLNK(run_root.lstat().st_mode):
            return 1
        root = run_root.resolve(strict=True)
        if config_path != root / "worker-config.json":
            return 1
        root_info = root.lstat()
        if (not stat.S_ISDIR(root_info.st_mode) or root_info.st_mode & 0o077 or
                root_info.st_uid != uid or root_info.st_gid != gid):
            return 1
        info = config_path.lstat()
        if (not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or
                info.st_mode & 0o077 or info.st_uid != uid or info.st_gid != gid or info.st_size > MAX_CONFIG_BYTES):
            return 1
        os.setgroups([])
        os.setgid(gid)
        os.setuid(uid)
        os.execv(sys.executable, [sys.executable, "-m", "ocr.runtime.async_consumer",
                                  "--config", str(config_path), "healthcheck"])
    except Exception:
        return 1
    return 1


def main() -> int:
    if len(sys.argv) == 3 and sys.argv[1] == "healthcheck":
        try:
            uid = int(os.environ.get("PASSPORT_WORKER_UID", "10001"))
            gid = int(os.environ.get("PASSPORT_WORKER_GID", "10001"))
            return healthcheck(Path(sys.argv[2]), uid, gid)
        except Exception:
            return 1
    try:
        if len(sys.argv) not in (2, 3) or (len(sys.argv) == 3 and sys.argv[2] != "--check-config"):
            raise ValueError("config argument")
        check_config = len(sys.argv) == 3
        uid = int(os.environ.get("PASSPORT_WORKER_UID", "10001"))
        gid = int(os.environ.get("PASSPORT_WORKER_GID", "10001"))
        private_config = prepare(Path(sys.argv[1]), uid, gid)
        os.setgroups([])
        os.setgid(gid)
        os.setuid(uid)
        command = [sys.executable, "-m", "ocr.runtime.async_consumer", "--config", str(private_config)]
        if check_config:
            command.append("--check-config")
        os.execv(sys.executable, command)
    except Exception:
        print("worker_bootstrap_failed", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
