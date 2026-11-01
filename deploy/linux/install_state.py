#!/usr/bin/env python3
"""Create/resume the private target state for the bundled Linux installer."""

from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
import re
import secrets
import shutil
import stat
import subprocess
import sys
import uuid
from pathlib import Path, PurePosixPath
from typing import Any, Optional

EXPECTED_SOURCE_COMMIT = "8ea541f39209ef1e30e680204366041fcfdb0c84"
EXPECTED_SOURCE_VERSION = "4.10.0"
EXPECTED_MODEL_VERSION = "v11"
EXPECTED_MODEL_COUNT = 33
EXPECTED_MODEL_BYTES = 275_548_934
LAN_HTTP_PORT = 8080
RUNTIME_UID_DEFAULT = 10001
RUNTIME_GID_DEFAULT = 10001
MAX_MANIFEST_BYTES = 4 * 1024 * 1024
MARKER = "install.json"
ROOT_FILES = {
    "install.sh", ".dockerignore", "go.mod", "go.sum", "compose.yaml",
    "compose.managed-tls.yaml", "compose.production.yaml", "compose.lan.yaml",
    "README.md", "LINUX_DEPLOYMENT.md", "SHA256SUMS",
}
PUBLIC_PREFIXES = ("api/", "cmd/", "deploy/", "docs/", "internal/", "ocr/", "assets/")
FORBIDDEN_PARTS = {".git", ".local", ".tmp", "outputs", "venv", "__pycache__", "site-packages"}


def fail(message: str) -> None:
    raise ValueError(message)


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            fail("duplicate JSON key")
        out[key] = value
    return out


def read_json(path: Path, limit: int = MAX_MANIFEST_BYTES) -> Any:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_size > limit:
        fail("invalid JSON file")
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=strict_object)


def digest_file(path: Path) -> tuple[int, str]:
    h = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        while True:
            block = stream.read(1024 * 1024)
            if not block:
                break
            size += len(block)
            h.update(block)
    return size, h.hexdigest()


def read_regular_nofollow(path: Path, maximum: int) -> bytes:
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > maximum:
            fail("invalid private input file")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining:
            chunk = os.read(fd, min(1024 * 1024, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        if len(data) > maximum:
            fail("private input file is too large")
        return data
    finally:
        os.close(fd)


def validate_relative(value: Any) -> PurePosixPath:
    if not isinstance(value, str) or not value or "\\" in value:
        fail("invalid manifest path")
    p = PurePosixPath(value)
    if p.is_absolute() or any(part in ("", ".", "..") for part in p.parts):
        fail("invalid manifest path")
    return p


def no_symlink_path(root: Path, rel: PurePosixPath) -> Path:
    current = root
    if current.is_symlink():
        fail("symlink in bundle")
    for component in rel.parts:
        current = current / component
        info = current.lstat()
        if stat.S_ISLNK(info.st_mode):
            fail("symlink in bundle")
    return current


def bundle_manifest(kit: Path) -> tuple[dict[str, Any], list[tuple[PurePosixPath, int, str]], str]:
    manifest_path = kit / "bundle-manifest.json"
    manifest = read_json(manifest_path)
    if not isinstance(manifest, dict) or manifest.get("schema_version") != 1:
        fail("unsupported bundle manifest")
    source = manifest.get("source")
    models = manifest.get("models")
    entries = manifest.get("files")
    if not isinstance(source, dict) or not isinstance(models, dict) or not isinstance(entries, list):
        fail("incomplete bundle manifest")
    if (source.get("commit") != EXPECTED_SOURCE_COMMIT or source.get("version") != EXPECTED_SOURCE_VERSION or
            source.get("clean") is not True or models.get("lock_version") != EXPECTED_MODEL_VERSION or
            models.get("files_count") != EXPECTED_MODEL_COUNT or models.get("total_bytes") != EXPECTED_MODEL_BYTES):
        fail("bundle does not match the approved OCR snapshot")
    lock_rel = validate_relative(models.get("lock_path"))
    if lock_rel != PurePosixPath("assets/russiandocs/document_processing/models.lock.json"):
        fail("model lock path mismatch")
    lock_path = no_symlink_path(kit, lock_rel)
    lock_size, lock_hash = digest_file(lock_path)
    del lock_size
    if lock_hash != models.get("lock_sha256"):
        fail("model lock hash mismatch")
    lock = read_json(lock_path, 2 * 1024 * 1024)
    if not isinstance(lock, dict) or lock.get("models_version") != EXPECTED_MODEL_VERSION or not isinstance(lock.get("files"), list):
        fail("model lock schema mismatch")
    model_entries: list[tuple[str, int, str]] = []
    for item in lock["files"]:
        if not isinstance(item, dict):
            fail("invalid model lock entry")
        rel = validate_relative(item.get("path"))
        size = item.get("size")
        checksum = item.get("sha256")
        if isinstance(size, bool) or not isinstance(size, int) or size <= 0 or not isinstance(checksum, str) or not re.fullmatch(r"[0-9a-f]{64}", checksum):
            fail("invalid model lock entry")
        model_file = no_symlink_path(kit / "assets/models", rel)
        actual_size, actual_hash = digest_file(model_file)
        if actual_size != size or actual_hash != checksum:
            fail("locked model content mismatch")
        model_entries.append((rel.as_posix(), size, checksum))
    if (len(model_entries) != EXPECTED_MODEL_COUNT or sum(e[1] for e in model_entries) != EXPECTED_MODEL_BYTES or
            len({e[0] for e in model_entries}) != len(model_entries)):
        fail("model lock inventory mismatch")

    parsed: list[tuple[PurePosixPath, int, str]] = []
    listed: set[str] = set()
    for item in entries:
        if not isinstance(item, dict):
            fail("invalid file manifest entry")
        rel = validate_relative(item.get("path"))
        size = item.get("size")
        checksum = item.get("sha256")
        if isinstance(size, bool) or not isinstance(size, int) or size < 0 or not isinstance(checksum, str) or len(checksum) != 64:
            fail("invalid file manifest entry")
        name = rel.as_posix()
        if name in ("bundle-manifest.json", "SHA256SUMS"):
            fail("bundle metadata cannot be payload")
        if name in listed or any(part in FORBIDDEN_PARTS for part in rel.parts):
            fail("unexpected bundle file")
        if name not in ROOT_FILES and not name.startswith(PUBLIC_PREFIXES):
            fail("unexpected bundle file")
        if name.startswith("assets/") and not (name.startswith("assets/russiandocs/") or name.startswith("assets/models/")):
            fail("unexpected asset bundle path")
        file_path = no_symlink_path(kit, rel)
        if not file_path.is_file():
            fail("bundle file missing")
        actual_size, actual_hash = digest_file(file_path)
        if actual_size != size or actual_hash != checksum:
            fail("bundle file integrity mismatch")
        listed.add(name)
        parsed.append((rel, size, checksum))
    expected_models = {f"assets/models/{name}" for name, _, _ in model_entries}
    if not expected_models.issubset(listed):
        fail("locked model files missing from bundle")
    source_files = [name for name in listed if name.startswith("assets/russiandocs/")]
    if not source_files or not any(name.endswith("/SOURCE_PROVENANCE.json") for name in source_files):
        fail("source provenance missing")
    package_files = [name for name in source_files if name.startswith("assets/russiandocs/document_processing/")]
    license_files = [name for name in source_files if name in ("assets/russiandocs/LICENSE", "assets/russiandocs/NOTICE")]
    if (source.get("files_count") != len(source_files) or source.get("files_count") != 106 or
            source.get("package_files_count") != len(package_files) or source.get("package_files_count") != 103 or
            source.get("license_files_count") != len(license_files) or source.get("license_files_count") != 2 or
            set(license_files) != {"assets/russiandocs/LICENSE", "assets/russiandocs/NOTICE"}):
        fail("source inventory count mismatch")
    actual_models = {"assets/models/" + path for path in walk_payload(kit / "assets/models")}
    if actual_models != expected_models:
        fail("model bundle contains missing or unlocked files")
    actual_source = {"assets/russiandocs/" + path for path in walk_payload(kit / "assets/russiandocs")}
    if actual_source != {name for name in listed if name.startswith("assets/russiandocs/")}:
        fail("source bundle contains unlisted files")
    template_prefix = "assets/russiandocs/document_processing/pipeline_modules/page_registration/templates/"
    template_entries = [(name, size) for name, (size, _) in ((p.as_posix(), (s, h)) for p, s, h in parsed) if name.startswith(template_prefix)]
    if (source.get("templates_files_count") != len(template_entries) or source.get("templates_files_count") != 29 or
            source.get("templates_bytes") != sum(size for _, size in template_entries)):
        fail("runtime template inventory mismatch")
    version_file = no_symlink_path(kit / "assets/russiandocs", PurePosixPath("document_processing/__init__.py"))
    source_text = version_file.read_text(encoding="utf-8")
    if not re.search(r"(?m)^__version__\s*=\s*['\"]4\.10\.0['\"]\s*$", source_text):
        fail("source version label mismatch")
    actual_payload = walk_payload(kit)
    actual_payload.discard("bundle-manifest.json")
    actual_payload.discard("SHA256SUMS")
    if actual_payload != listed:
        fail("bundle payload inventory differs from manifest")
    bundle = manifest.get("bundle")
    checksums_name = bundle.get("checksums") if isinstance(bundle, dict) else None
    if checksums_name != "SHA256SUMS":
        fail("checksum list is missing")
    checksums_path = kit / checksums_name
    if checksums_path.is_symlink() or not checksums_path.is_file() or checksums_path.stat().st_size > MAX_MANIFEST_BYTES:
        fail("checksum list is invalid")
    checksum_lines = checksums_path.read_text(encoding="ascii").splitlines()
    expected = {p.as_posix(): digest for p, _, digest in parsed}
    expected["bundle-manifest.json"] = hashlib.sha256(manifest_path.read_bytes()).hexdigest()
    observed: dict[str, str] = {}
    for line in checksum_lines:
        if len(line) < 67 or line[64:66] != "  ":
            fail("invalid checksum list")
        checksum, name = line[:64], line[66:]
        rel = validate_relative(name)
        if not re.fullmatch(r"[0-9a-f]{64}", checksum) or name in observed or name == "SHA256SUMS":
            fail("invalid checksum list")
        observed[rel.as_posix()] = checksum
    if observed != expected:
        fail("checksum list does not match bundle manifest")
    return manifest, parsed, hashlib.sha256(manifest_path.read_bytes()).hexdigest()


def walk_payload(root: Path) -> set[str]:
    names: set[str] = set()
    for current, dirs, files in os.walk(root, followlinks=False):
        base = Path(current)
        for name in list(dirs):
            p = base / name
            if p.is_symlink() or not p.is_dir():
                fail("unexpected asset directory")
        for name in files:
            p = base / name
            if p.is_symlink() or not p.is_file():
                fail("unexpected asset file")
            names.add(p.relative_to(root).as_posix())
    return names


def existing_root(path: Path) -> bool:
    try:
        info = path.lstat()
    except FileNotFoundError:
        return False
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700:
        fail("runtime root must be a real root-owned 0700 directory")
    return True


def mkdir_owned(path: Path, mode: int, uid: int, gid: int) -> None:
    try:
        os.mkdir(path, mode)
        os.chown(path, uid, gid)
        os.chmod(path, mode)
    except FileExistsError:
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            fail("runtime directory type mismatch")
        if stat.S_IMODE(info.st_mode) != mode or info.st_uid != uid or info.st_gid != gid:
            fail("runtime directory permissions mismatch")


def write_new(path: Path, data: bytes, mode: int = 0o600, uid: int = 0, gid: int = 0) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags, mode)
    try:
        os.fchown(fd, uid, gid)
        os.fchmod(fd, mode)
        offset = 0
        while offset < len(data):
            offset += os.write(fd, data[offset:])
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_json(path: Path, value: Any, mode: int = 0o600) -> None:
    data = (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
    try:
        info = path.lstat()
    except FileNotFoundError:
        info = None
    if info is not None:
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != mode:
            fail("runtime file permissions mismatch")
        current = read_regular_nofollow(path, 4 * 1024 * 1024)
        if current != data:
            fail("existing runtime config differs; refusing overwrite")
        return
    tmp = path.with_name("." + path.name + ".new-" + secrets.token_hex(8))
    write_new(tmp, data, mode)
    try:
        os.link(tmp, path, follow_symlinks=False)
        tmp.unlink()
        dirfd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(dirfd)
        finally:
            os.close(dirfd)
    except FileExistsError:
        tmp.unlink(missing_ok=True)
        atomic_json(path, value, mode)
    finally:
        tmp.unlink(missing_ok=True)


def replace_marker(path: Path, value: Any) -> None:
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600:
        fail("installer marker permissions mismatch")
    data = (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
    tmp = path.with_name("." + path.name + ".new-" + secrets.token_hex(8))
    write_new(tmp, data)
    os.replace(tmp, path)
    dirfd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(dirfd)
    finally:
        os.close(dirfd)


def read_marker(root: Path) -> dict[str, Any] | None:
    path = root / MARKER
    try:
        value = read_json(path, 64 * 1024)
    except FileNotFoundError:
        return None
    info = path.lstat()
    if info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or not isinstance(value, dict):
        fail("installer marker is invalid")
    return value


def host_networks() -> list[ipaddress.IPv4Network]:
    result: list[ipaddress.IPv4Network] = []
    try:
        routes = json.loads(subprocess.check_output(["ip", "-j", "route"], stderr=subprocess.DEVNULL))
    except Exception:
        fail("host route inventory unavailable")
    for route in routes:
        dst = route.get("dst")
        if isinstance(dst, str) and dst != "default":
            try:
                result.append(ipaddress.ip_network(dst, strict=False))
            except ValueError:
                pass
    try:
        ids = subprocess.check_output(["docker", "network", "ls", "-q"], stderr=subprocess.DEVNULL, text=True).split()
        if ids:
            data = json.loads(subprocess.check_output(["docker", "network", "inspect", *ids], stderr=subprocess.DEVNULL))
    except Exception:
        fail("Docker network inventory unavailable")
    for network in data if ids else []:
        ipam = network.get("IPAM") or {}
        configs = ipam.get("Config") or []
        for item in configs:
            subnet = item.get("Subnet")
            if subnet:
                try:
                    result.append(ipaddress.ip_network(subnet, strict=False))
                except ValueError:
                    pass
    return result


def select_subnet() -> str:
    occupied = host_networks()
    for octet in (30, 31, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16):
        candidate = ipaddress.ip_network(f"172.{octet}.0.0/24")
        if not any(candidate.overlaps(existing) for existing in occupied):
            return str(candidate)
    fail("could not select a private Docker subnet without a route overlap")


def is_lan_bind_address(value: str) -> bool:
    try:
        address = ipaddress.IPv4Address(value)
    except ValueError:
        return False
    allowed = (ipaddress.ip_network("10.0.0.0/8"), ipaddress.ip_network("172.16.0.0/12"),
               ipaddress.ip_network("192.168.0.0/16"), ipaddress.ip_network("127.0.0.0/8"))
    return any(address in network for network in allowed)


def ingress_identity_matches(marker: dict[str, Any], domain: str, public_bind: str, tls_mode: str,
                             certificate_sha256: Optional[str], http_port: Optional[int]) -> bool:
    return (marker.get("domain") == domain and marker.get("public_bind") == public_bind and
            marker.get("tls_mode") == tls_mode and marker.get("certificate_sha256") == certificate_sha256 and
            marker.get("http_port") == http_port)


def install_env(runtime: Path, subnet: str, domain: str, public_bind: str, uid: int, gid: int,
                tls_mode: str, http_port: int) -> dict[str, str]:
    network = ipaddress.ip_network(subnet)
    ip = lambda n: str(network.network_address + n)
    env = {
        "PASSPORT_POSTGRES_DB": "passport",
        "PASSPORT_POSTGRES_USER": "passport",
        "PASSPORT_RABBITMQ_USER": "passport",
        "PASSPORT_RABBITMQ_HOSTNAME": "passport-rabbitmq",
        "PASSPORT_APP_UID": str(uid), "PASSPORT_APP_GID": str(gid),
        "PASSPORT_API_IP": ip(10), "PASSPORT_WORKER_IP": ip(11), "PASSPORT_INGRESS_IP": ip(20),
        "PASSPORT_PRIVATE_SUBNET": subnet,
        "PASSPORT_PRIVATE_DYNAMIC_POOL": f"{network.network_address + 128}/25",
        "PASSPORT_TLS_DOMAIN": domain,
        "PASSPORT_PUBLIC_BIND": public_bind,
        "PASSPORT_POSTGRES_PASSWORD_FILE": str(runtime / "secrets/postgres_password"),
        "PASSPORT_RABBITMQ_PASSWORD_FILE": str(runtime / "secrets/rabbitmq_password"),
        "PASSPORT_POSTGRES_DSN_FILE": str(runtime / "secrets/postgres_dsn"),
        "PASSPORT_RABBITMQ_DSN_FILE": str(runtime / "secrets/rabbitmq_dsn"),
        "PASSPORT_CLIENT_CREDENTIALS_FILE": "/dev/null",
        "PASSPORT_WORKER_CREDENTIALS_FILE": str(runtime / "secrets/worker_credentials.json"),
        "PASSPORT_OCR_WORKER_TOKEN_FILE": str(runtime / "secrets/worker_token"),
        "PASSPORT_API_CONFIG_FILE": str(runtime / "config/api.json"),
        "PASSPORT_OCR_CONFIG_FILE": str(runtime / "config/ocr-worker.json"),
        "PASSPORT_IMAGE_ROOT": str(runtime / "images"),
        "PASSPORT_API_STATE_ROOT": str(runtime / "state"),
        "PASSPORT_RUSSIANDOCS_ROOT": str(runtime / "russiandocs"),
        "PASSPORT_MODELS_ROOT": str(runtime / "models"),
        "PASSPORT_OCR_IMAGE": "passport-backend-ocr:linux-install-v11",
    }
    if tls_mode == "static":
        env["PASSPORT_TLS_CERTIFICATE_FILE"] = str(runtime / "secrets/tls.crt")
        env["PASSPORT_TLS_PRIVATE_KEY_FILE"] = str(runtime / "secrets/tls.key")
    if tls_mode == "lan":
        env["PASSPORT_HTTP_PORT"] = str(http_port)
    name = {"static": "compose.static.env", "managed": "compose.env", "lan": "compose.lan.env"}[tls_mode]
    path = runtime / name
    data = "".join(f"{key}={value}\n" for key, value in env.items()).encode()
    if path.exists():
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != 0 or path.read_bytes() != data:
            fail("existing Compose environment differs; refusing overwrite")
    else:
        write_new(path, data)
    return env


def api_config(api_ip: str, ingress_ip: str, runtime: Path, manifest: dict[str, Any]) -> dict[str, Any]:
    source = manifest["source"]["version"]
    models = manifest["models"]["lock_version"]
    return {
        "listen_address": f"{api_ip}:8080",
        "allowed_client_networks": [f"{ingress_ip}/32"],
        "max_upload_bytes": 8 * 1024 * 1024,
        "max_concurrent": 1,
        "pipeline_version": f"russiandocs-{source}-models-{models}",
        "ocr_timeout": "360s", "read_header_timeout": "5s", "read_timeout": "30s",
        "write_timeout": "420s", "idle_timeout": "30s", "shutdown_timeout": "30s",
        "max_header_bytes": 16384, "process_stop_grace": "30s",
        "max_output_bytes": 2 * 1024 * 1024, "max_image_pixels": 40_000_000,
        "logging": {"path": "/var/lib/passport/state/logs/api.jsonl", "level": "info", "max_size_bytes": 5_242_880, "max_backups": 3},
        "metrics": {"state_path": "/var/lib/passport/state/metrics/state.json"},
        "sync_ocr_enabled": False,
        "async_jobs": {
            "database_dsn_file": "/run/passport-api/secrets/postgres_dsn",
            "connect_timeout": "5s", "migration_lock_timeout": "30s",
            "image_root": "/var/lib/passport/images", "max_stored_image_bytes": 10 * 1024 * 1024 * 1024,
            "max_concurrent_uploads": 8,
            "client_enrollment": {"max_clients": 10000, "max_new_clients_per_minute": 30},
            "retention": {"claim_lease": "30s", "interval": "1s", "operation_timeout": "10s", "batch_size": 100},
        },
        "queue_processing": {
            "worker_credentials_file": "/run/passport-api/secrets/worker_credentials",
            "private_listener_address": f"{api_ip}:8081",
            "broker_dsn_file": "/run/passport-api/secrets/broker_dsn",
            "connect_timeout": "5s", "poll_interval": "1s", "publisher_lease": "30s",
            "publish_timeout": "10s", "publish_backoff": "2s", "reconnect_min": "1s", "reconnect_max": "30s",
            "consumer_ack_timeout": "35m", "max_attempts": 5, "retry_initial_delay": "2s", "retry_max_delay": "1m",
            "hard_processing_timeout": "25m", "completion_budget": "2m", "attempt_lease": "30m", "recovery_interval": "15s",
        },
    }


def worker_config(api_ip: str, runtime: Path, manifest: dict[str, Any]) -> dict[str, Any]:
    return {
        "rabbit_host": "passport-rabbitmq", "rabbit_port": 5672, "rabbit_user": "passport", "rabbit_password_file": "/run/secrets/rabbitmq_password", "rabbit_vhost": "/",
        "queue": "passport.recognition.work", "exchange": "passport.jobs", "routing_key": "recognition.requested",
        "dead_letter_exchange": "passport.jobs.dlx", "dead_letter_queue": "passport.recognition.dead",
        "go_worker_url": f"http://{api_ip}:8081", "worker_token_file": "/run/secrets/ocr_worker_token",
        "readiness_marker": "/run/passport-ocr/ready", "image_root": "/var/lib/passport/images",
        "russiandocs_repo": "/opt/russiandocs", "models_root": "/opt/passport-models", "work_root": "/var/lib/passport/ocr-work",
        "max_input_bytes": 8 * 1024 * 1024, "max_image_pixels": 40_000_000,
        "expected_upstream_version": manifest["source"]["version"], "expected_model_lock_version": manifest["models"]["lock_version"],
        "startup_timeout_seconds": 600, "processing_stop_grace_seconds": 30,
        "http_timeout_ms": 10_000, "boundary_attempts": 3, "reconnect_backoff_seconds": 2,
        "wait_poll_max_seconds": 2, "rabbit_heartbeat_seconds": 60,
        "rabbit_blocked_connection_timeout_seconds": 30, "consumer_ack_timeout_ms": 2_100_000,
    }


def copy_payload(kit: Path, runtime: Path, entries: list[tuple[PurePosixPath, int, str]], uid: int, gid: int) -> None:
    desired: dict[str, tuple[int, str]] = {}
    for root_name, expected_gid, _, expected_file_mode in (
        ("russiandocs", gid, 0o755, 0o644), ("models", gid, 0o750, 0o640),
    ):
        asset_root = runtime / root_name
        for current, dirs, files in os.walk(asset_root, followlinks=False):
            base = Path(current)
            for directory in dirs:
                path = base / directory
                info = path.lstat()
                if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                    fail("asset destination directory mismatch")
            for filename in files:
                if not re.fullmatch(r"\..+\.install-[0-9a-f]{12}", filename):
                    continue
                path = base / filename
                info = path.lstat()
                if (stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or
                        info.st_uid != 0 or info.st_gid != expected_gid or stat.S_IMODE(info.st_mode) != expected_file_mode):
                    fail("unrecognized interrupted asset copy")
                path.unlink()
    for rel, size, checksum in entries:
        text = rel.as_posix()
        if not text.startswith("assets/"):
            continue
        if text.startswith("assets/russiandocs/"):
            target_rel = text.removeprefix("assets/russiandocs/")
            dest_root = runtime / "russiandocs"
            fmode, fuid, fgid = 0o644, 0, gid
            dmode, duid, dgid = 0o755, 0, gid
        elif text.startswith("assets/models/"):
            target_rel = text.removeprefix("assets/models/")
            dest_root = runtime / "models"
            fmode, fuid, fgid = 0o640, 0, gid
            dmode, duid, dgid = 0o750, 0, gid
        else:
            fail("unexpected payload root")
        desired[f"{dest_root.name}/{target_rel}"] = (size, checksum)
        src = no_symlink_path(kit, rel)
        dest = dest_root / target_rel
        parent = dest.parent
        chain: list[Path] = []
        while parent != dest_root.parent and parent != dest_root:
            chain.append(parent)
            parent = parent.parent
        for directory in reversed(chain):
            try:
                os.mkdir(directory, dmode)
                os.chown(directory, duid, dgid)
                os.chmod(directory, dmode)
            except FileExistsError:
                info = directory.lstat()
                if (stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or
                        info.st_uid != duid or info.st_gid != dgid or stat.S_IMODE(info.st_mode) != dmode):
                    fail("asset destination directory mismatch")
        if dest.exists():
            info = dest.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != fuid or info.st_gid != fgid or stat.S_IMODE(info.st_mode) != fmode:
                fail("existing asset file permissions mismatch")
            actual_size, actual_hash = digest_file(dest)
            if actual_size != size or actual_hash != checksum:
                fail("existing installed asset does not match bundle")
            continue
        data_tmp = dest.with_name("." + dest.name + ".install-" + secrets.token_hex(6))
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(data_tmp, flags, fmode)
        try:
            os.fchown(fd, fuid, fgid)
            os.fchmod(fd, fmode)
            with src.open("rb") as stream:
                while True:
                    block = stream.read(1024 * 1024)
                    if not block:
                        break
                    view = memoryview(block)
                    while view:
                        written = os.write(fd, view)
                        view = view[written:]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.link(data_tmp, dest, follow_symlinks=False)
        data_tmp.unlink()
    for root_name in ("russiandocs", "models"):
        root = runtime / root_name
        actual = walk_payload(root) if root.exists() else set()
        want = {path.split("/", 1)[1] for path in desired if path.startswith(root_name + "/")}
        if actual != want:
            fail("installed asset inventory differs from bundle")


def prepare(args: argparse.Namespace) -> None:
    kit = Path(args.kit_root).resolve(strict=True)
    runtime = Path(args.runtime_root)
    parent = runtime.parent
    parent_info = parent.lstat()
    if stat.S_ISLNK(parent_info.st_mode) or not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != 0:
        fail("runtime parent must be a root-owned directory")
    manifest, entries, manifest_hash = bundle_manifest(kit)
    root_exists = existing_root(runtime)
    if not root_exists:
        if runtime.exists() or runtime.is_symlink():
            fail("runtime path is not a safe new directory")
        os.mkdir(runtime, 0o700)
        os.chown(runtime, 0, 0)
        os.chmod(runtime, 0o700)
    marker = read_marker(runtime)
    tls_mode = "lan" if args.lan else ("static" if args.tls_cert else "managed")
    if args.lan:
        if (args.domain or args.tls_cert or args.tls_key or args.http_port != LAN_HTTP_PORT or
                not is_lan_bind_address(args.public_bind)):
            fail("invalid LAN install identity")
    elif not args.domain:
        fail("domain is required outside LAN mode")
    if not args.docker_context or args.docker_context.startswith("-") or len(args.docker_context) > 128:
        fail("Docker context identity is invalid")
    static_cert_hash = None
    if args.tls_cert:
        static_cert_hash = hashlib.sha256(read_regular_nofollow(Path(args.tls_cert), 1024 * 1024)).hexdigest()
    if marker is None:
        content = list(runtime.iterdir())
        if content:
            fail("refusing a nonempty unrecognized runtime directory")
        marker = {
            "schema_version": 1, "status": "initializing", "domain": args.domain,
            "public_bind": args.public_bind,
            "tls_mode": tls_mode, "http_port": LAN_HTTP_PORT if args.lan else None,
            "certificate_sha256": static_cert_hash,
            "bundle_manifest_sha256": manifest_hash,
            "source_commit": manifest["source"]["commit"], "model_lock_version": manifest["models"]["lock_version"],
            "runtime_uid": args.runtime_uid, "runtime_gid": args.runtime_gid,
            "docker_context": args.docker_context,
            "project_name": "passport-backend", "rabbitmq_hostname": "passport-rabbitmq",
            "private_subnet": select_subnet(),
        }
        atomic_json(runtime / MARKER, marker)
    else:
        expected = (marker.get("schema_version") == 1 and
                    ingress_identity_matches(marker, args.domain, args.public_bind, tls_mode,
                                             static_cert_hash, LAN_HTTP_PORT if args.lan else None) and
                    marker.get("bundle_manifest_sha256") == manifest_hash and marker.get("source_commit") == manifest["source"]["commit"] and
                    marker.get("model_lock_version") == manifest["models"]["lock_version"] and
                    marker.get("runtime_uid") == args.runtime_uid and marker.get("runtime_gid") == args.runtime_gid and
                    marker.get("docker_context") == args.docker_context and
                    marker.get("project_name") == "passport-backend" and marker.get("rabbitmq_hostname") == "passport-rabbitmq" and
                    isinstance(marker.get("private_subnet"), str) and marker.get("status") in ("initializing", "complete"))
        if not expected:
            fail("runtime installation identity differs; refusing overwrite")

    uid, gid = args.runtime_uid, args.runtime_gid
    for name in ("config", "secrets"):
        mkdir_owned(runtime / name, 0o700, 0, 0)
    for name in ("images", "state", "state/logs", "state/metrics", "backup", "backup/keys", "backup/archives"):
        mkdir_owned(runtime / name, 0o700, uid, gid)
    mkdir_owned(runtime / "russiandocs", 0o755, 0, gid)
    mkdir_owned(runtime / "models", 0o750, 0, gid)

    copy_payload(kit, runtime, entries, uid, gid)
    env = install_env(runtime, marker["private_subnet"], args.domain, args.public_bind, uid, gid, tls_mode, LAN_HTTP_PORT)
    secret_values: dict[str, bytes] = {}
    secret_values["postgres_password"] = secrets.token_hex(32).encode() + b"\n"
    secret_values["rabbitmq_password"] = secrets.token_hex(32).encode() + b"\n"
    for name, value in secret_values.items():
        path = runtime / "secrets" / name
        if not path.exists():
            if marker["status"] == "complete":
                fail("required secret is missing from completed installation")
            write_new(path, value)
        else:
            info = path.lstat()
            if (stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
                    stat.S_IMODE(info.st_mode) != 0o600):
                fail("existing secret is invalid; refusing replacement")
            saved = read_regular_nofollow(path, 128).strip()
            if len(saved) != 64 or not re.fullmatch(rb"[0-9a-f]{64}", saved):
                fail("existing secret is invalid; refusing replacement")
        secret_values[name] = path.read_bytes().strip()
    for name, value in (("postgres_dsn", f"postgres://passport:{secret_values['postgres_password'].decode()}@postgres:5432/passport?sslmode=disable\n".encode()),
                        ("rabbitmq_dsn", f"amqp://passport:{secret_values['rabbitmq_password'].decode()}@passport-rabbitmq:5672/%2f\n".encode())):
        path = runtime / "secrets" / name
        if not path.exists():
            if marker["status"] == "complete":
                fail("required DSN is missing from completed installation")
            write_new(path, value)
        else:
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or path.read_bytes() != value:
                fail("existing DSN permissions are invalid")

    raw_token = "pwt1_" + __import__("base64").urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")
    token_path = runtime / "secrets/worker_token"
    registry_path = runtime / "secrets/worker_credentials.json"
    if not token_path.exists() and not registry_path.exists():
        if marker["status"] == "complete":
            fail("worker credential missing from completed installation")
        worker_id = str(uuid.uuid4())
        token_hash = hashlib.sha256(raw_token.encode("ascii")).hexdigest()
        write_new(token_path, (raw_token + "\n").encode())
        registry = {"schema_version": "passport-worker-credentials/v1", "credentials": [{"worker_id": worker_id, "token_sha256": token_hash}]}
        write_new(registry_path, (json.dumps(registry, sort_keys=True, separators=(",", ":")) + "\n").encode())
    elif token_path.exists() and registry_path.exists():
        token_info, reg_info = token_path.lstat(), registry_path.lstat()
        if any((stat.S_ISLNK(item.st_mode) or not stat.S_ISREG(item.st_mode) or item.st_uid != 0 or stat.S_IMODE(item.st_mode) != 0o600) for item in (token_info, reg_info)):
            fail("existing worker credentials have invalid permissions")
        token_value = read_regular_nofollow(token_path, 256).decode("ascii").strip()
        registry = read_json(registry_path, 64 * 1024)
        expected_hash = hashlib.sha256(token_value.encode("ascii")).hexdigest()
        if (not re.fullmatch(r"pwt1_[A-Za-z0-9_-]{43}", token_value) or not isinstance(registry, dict) or registry.get("schema_version") != "passport-worker-credentials/v1" or
                not isinstance(registry.get("credentials"), list) or len(registry["credentials"]) != 1 or
                not isinstance(registry["credentials"][0], dict) or registry["credentials"][0].get("token_sha256") != expected_hash):
            fail("existing worker credentials do not match their registry")
    else:
        fail("partial worker credential pair; refusing to rotate it")

    if tls_mode == "static":
        for source_arg, dest_name in ((args.tls_cert, "tls.crt"), (args.tls_key, "tls.key")):
            source = Path(source_arg)
            dest = runtime / "secrets" / dest_name
            data = read_regular_nofollow(source, 2 * 1024 * 1024)
            if dest.exists():
                info = dest.lstat()
                if (stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
                        stat.S_IMODE(info.st_mode) != 0o600 or digest_file(dest)[1] != hashlib.sha256(data).hexdigest()):
                    fail("existing TLS files differ from supplied pair")
            else:
                write_new(dest, data)
    api = api_config(env["PASSPORT_API_IP"], env["PASSPORT_INGRESS_IP"], runtime, manifest)
    worker = worker_config(env["PASSPORT_API_IP"], runtime, manifest)
    atomic_json(runtime / "config/api.json", api)
    atomic_json(runtime / "config/ocr-worker.json", worker)


def mark_complete(runtime: Path) -> None:
    marker = read_marker(runtime)
    if marker is None or marker.get("status") not in ("initializing", "complete"):
        fail("installer marker unavailable")
    marker["status"] = "complete"
    replace_marker(runtime / MARKER, marker)


def verify_bundle(args: argparse.Namespace) -> None:
    bundle_manifest(Path(args.kit_root).resolve(strict=True))


def main() -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    verify = sub.add_parser("verify-bundle")
    verify.add_argument("--kit-root", required=True)
    prepare_parser = sub.add_parser("prepare")
    prepare_parser.add_argument("--kit-root", required=True)
    prepare_parser.add_argument("--runtime-root", required=True)
    prepare_parser.add_argument("--domain", default="")
    prepare_parser.add_argument("--public-bind", required=True)
    prepare_parser.add_argument("--lan", action="store_true")
    prepare_parser.add_argument("--http-port", type=int, default=8080)
    prepare_parser.add_argument("--runtime-uid", required=True, type=int)
    prepare_parser.add_argument("--runtime-gid", required=True, type=int)
    prepare_parser.add_argument("--docker-context", required=True)
    prepare_parser.add_argument("--tls-cert")
    prepare_parser.add_argument("--tls-key")
    complete = sub.add_parser("mark-complete")
    complete.add_argument("--runtime-root", required=True)
    args = parser.parse_args()
    try:
        if args.command == "verify-bundle":
            verify_bundle(args)
        elif args.command == "prepare":
            prepare(args)
        else:
            mark_complete(Path(args.runtime_root))
        return 0
    except Exception:
        print("installer state validation failed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
