#!/usr/bin/env python3
"""Private RabbitMQ consumer for durable passport OCR jobs.

RabbitMQ carries identifiers only. PostgreSQL remains behind the Go worker
boundary, and a delivery is acknowledged only after that boundary reports a
durable ACK decision.
"""

from __future__ import annotations

import argparse
import base64
import functools
import json
import logging
import math
import os
import re
import signal
import secrets
import stat
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .russiandocs_worker import FIELDS, SCHEMA as PIPELINE_SCHEMA, validate_pipeline_snapshot

ENVELOPE_KEYS = frozenset({"schema_version", "event_id", "job_id", "attempt_no"})
MAX_DELIVERY_BYTES = 16 * 1024
MAX_HTTP_RESPONSE_BYTES = 1 << 20
MAX_WORKER_REQUEST_BYTES = 2 << 20
MAX_PROTOCOL_LINE_BYTES = 2 << 20
WORKER_TOKEN_RE = re.compile(r"^pwt1_[A-Za-z0-9_-]{43}$")
SAFE_ERRORS = frozenset({
    "image_unprocessable", "processing_failed", "ocr_unavailable", "processing_timeout",
})


class ConsumerError(Exception):
    """An intentionally non-sensitive worker error category."""


class TemporaryBoundaryError(ConsumerError):
    pass


class PermanentDeliveryError(ConsumerError):
    pass


def _pairs_no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate_json_key")
        result[key] = value
    return result


def strict_json_object(data: bytes, *, max_bytes: int = MAX_DELIVERY_BYTES) -> dict[str, Any]:
    if len(data) > max_bytes:
        raise ValueError("oversized_json")
    try:
        value = json.loads(
            data,
            object_pairs_hook=_pairs_no_duplicates,
            parse_constant=lambda _: (_ for _ in ()).throw(ValueError("invalid_constant")),
        )
    except Exception as error:
        raise ValueError("invalid_json") from error
    if not isinstance(value, dict):
        raise ValueError("invalid_json")
    return value


def _uuid_v4(value: object) -> bool:
    if not isinstance(value, str):
        return False
    try:
        parsed = uuid.UUID(value)
    except (ValueError, AttributeError):
        return False
    return parsed.version == 4 and str(parsed) == value


@dataclass(frozen=True)
class Envelope:
    schema_version: int
    event_id: str
    job_id: str
    attempt_no: int

    def as_json(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "event_id": self.event_id,
            "job_id": self.job_id,
            "attempt_no": self.attempt_no,
        }


def parse_envelope(body: bytes) -> tuple[Envelope | None, str | None]:
    """Return a trusted-ID envelope or a closed reject category.

    A valid v4 event/job ID plus positive integer attempt is enough to ask Go
    to durably reject an unsupported version. Bool is rejected explicitly
    because Python treats it as an int subclass.
    """
    try:
        value = strict_json_object(body)
    except ValueError:
        return None, "malformed_envelope"
    if set(value) != ENVELOPE_KEYS:
        return None, "malformed_envelope"
    event_id, job_id = value.get("event_id"), value.get("job_id")
    attempt_no, schema_version = value.get("attempt_no"), value.get("schema_version")
    if not _uuid_v4(event_id) or not _uuid_v4(job_id):
        return None, "malformed_envelope"
    if isinstance(attempt_no, bool) or not isinstance(attempt_no, int) or attempt_no <= 0:
        return None, "malformed_envelope"
    if isinstance(schema_version, bool) or not isinstance(schema_version, int):
        return None, "malformed_envelope"
    envelope = Envelope(schema_version, event_id, job_id, attempt_no)
    if schema_version != 1:
        return envelope, "unsupported_envelope"
    return envelope, None


@dataclass(frozen=True)
class Config:
    rabbit_host: str
    rabbit_port: int
    rabbit_user: str
    rabbit_password_file: Path
    rabbit_vhost: str
    queue: str
    exchange: str
    routing_key: str
    dead_letter_exchange: str
    dead_letter_queue: str
    go_worker_url: str
    worker_token_file: Path
    readiness_marker: Path
    image_root: Path
    russiandocs_repo: Path
    models_root: Path
    work_root: Path
    max_input_bytes: int
    max_image_pixels: int
    expected_upstream_version: str
    expected_model_lock_version: str
    startup_timeout_seconds: float
    processing_stop_grace_seconds: float
    http_timeout_ms: int
    boundary_attempts: int
    reconnect_backoff_seconds: float
    wait_poll_max_seconds: float
    rabbit_heartbeat_seconds: int
    rabbit_blocked_connection_timeout_seconds: float
    consumer_ack_timeout_ms: int

    @classmethod
    def load(cls, path: Path) -> "Config":
        try:
            info = path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
                raise ValueError("config_permissions")
            raw = path.read_bytes()
            values = strict_json_object(raw, max_bytes=64 * 1024)
            expected = set(cls.__dataclass_fields__)
            if set(values) != expected:
                raise ValueError("config_fields")
            for key in ("rabbit_password_file", "worker_token_file", "image_root", "russiandocs_repo", "models_root", "work_root"):
                if not isinstance(values[key], str) or not values[key]:
                    raise ValueError("config_path")
                candidate = Path(values[key])
                if not candidate.is_absolute():
                    candidate = path.parent / candidate
                if stat.S_ISLNK(candidate.lstat().st_mode):
                    raise ValueError("config_symlink_path")
                values[key] = candidate.resolve(strict=True)
            readiness = values.get("readiness_marker")
            if not isinstance(readiness, str) or not readiness:
                raise ValueError("readiness_marker")
            readiness_path = Path(readiness)
            if not readiness_path.is_absolute():
                readiness_path = path.parent / readiness_path
            if stat.S_ISLNK(readiness_path.parent.lstat().st_mode):
                raise ValueError("readiness_parent_symlink")
            values["readiness_marker"] = readiness_path.parent.resolve(strict=True) / readiness_path.name
            cfg = cls(**values)
            cfg.validate()
            return cfg
        except Exception as error:
            # Never expose paths, secret contents or parser details in logs.
            raise ConsumerError("invalid_worker_config") from error

    def validate(self) -> None:
        if not isinstance(self.rabbit_host, str) or not self.rabbit_host:
            raise ValueError("rabbit_host")
        if not 1 <= self.rabbit_port <= 65535 or not self.rabbit_user or not self.rabbit_vhost:
            raise ValueError("rabbit_connection")
        if self.queue != "passport.recognition.work" or self.exchange != "passport.jobs" or self.routing_key != "recognition.requested":
            raise ValueError("rabbit_topology")
        if self.dead_letter_exchange != "passport.jobs.dlx" or self.dead_letter_queue != "passport.recognition.dead":
            raise ValueError("rabbit_dlq_topology")
        if isinstance(self.consumer_ack_timeout_ms, bool) or self.consumer_ack_timeout_ms < 300_000:
            raise ValueError("rabbit_ack_timeout")
        parsed = urllib.parse.urlsplit(self.go_worker_url)
        if parsed.scheme != "http" or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ValueError("go_worker_url")
        if parsed.path not in ("", "/"):
            raise ValueError("go_worker_url")
        positive = (
            self.max_input_bytes, self.max_image_pixels, self.startup_timeout_seconds,
            self.processing_stop_grace_seconds, self.http_timeout_ms,
            self.boundary_attempts, self.reconnect_backoff_seconds,
            self.wait_poll_max_seconds, self.rabbit_heartbeat_seconds,
            self.rabbit_blocked_connection_timeout_seconds, self.consumer_ack_timeout_ms,
        )
        if any(value <= 0 for value in positive):
            raise ValueError("worker_limits")
        _require_private_directory(self.readiness_marker.parent)
        if self.readiness_marker.name in ("", ".", ".."):
            raise ValueError("readiness_marker")
        for secret in (self.rabbit_password_file, self.worker_token_file):
            stat_info = secret.lstat()
            if not stat.S_ISREG(stat_info.st_mode) or stat_info.st_mode & 0o077:
                raise ValueError("secret_permissions")
        int_fields = (
            self.rabbit_port, self.max_input_bytes, self.max_image_pixels, self.http_timeout_ms,
            self.boundary_attempts, self.rabbit_heartbeat_seconds, self.consumer_ack_timeout_ms,
        )
        if any(isinstance(value, bool) or not isinstance(value, int) for value in int_fields):
            raise ValueError("worker_integer_config")
        number_fields = (
            self.startup_timeout_seconds, self.processing_stop_grace_seconds,
            self.reconnect_backoff_seconds, self.wait_poll_max_seconds,
            self.rabbit_blocked_connection_timeout_seconds,
        )
        if any(isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) for value in number_fields):
            raise ValueError("worker_number_config")
        version_labels = (self.expected_upstream_version, self.expected_model_lock_version)
        if any(not isinstance(value, str) or not value or value != value.strip() or len(value) > 128 or "\x00" in value
               for value in version_labels):
            raise ValueError("source_snapshot_config")

    def read_secret(self, path: Path) -> str:
        try:
            value = path.read_text(encoding="ascii").strip()
        except Exception as error:
            raise ConsumerError("worker_secret_unavailable") from error
        if path == self.worker_token_file and not WORKER_TOKEN_RE.fullmatch(value):
            raise ConsumerError("worker_secret_invalid")
        if path == self.worker_token_file:
            try:
                decoded = base64.urlsafe_b64decode(value.removeprefix("pwt1_") + "=")
            except Exception as error:
                raise ConsumerError("worker_secret_invalid") from error
            if len(decoded) != 32 or base64.urlsafe_b64encode(decoded).decode("ascii").rstrip("=") != value.removeprefix("pwt1_"):
                raise ConsumerError("worker_secret_invalid")
        if path == self.rabbit_password_file and (not value or "\n" in value):
            raise ConsumerError("rabbit_secret_invalid")
        return value


READINESS_FIELDS = frozenset({
    "schema_version", "status", "boot_nonce", "updated_monotonic_ns",
    "consumer_pid", "consumer_start_ticks", "ocr_pid", "ocr_start_ticks",
})
BOOT_NONCE_RE = re.compile(r"^[0-9a-f]{32}$")


def _require_private_directory(path: Path) -> None:
    try:
        info = path.lstat()
        if (not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_mode & 0o077 or
                info.st_uid != os.geteuid()):
            raise ValueError("private_directory")
    except Exception as error:
        raise ValueError("private_directory") from error


def _proc_start_ticks(pid: int, proc_root: Path = Path("/proc")) -> int:
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        raise ValueError("process_identity")
    raw = (proc_root / str(pid) / "stat").read_text(encoding="ascii")
    closing = raw.rfind(")")
    if closing < 0:
        raise ValueError("process_identity")
    fields = raw[closing + 1:].split()
    # The first post-comm field is field 3; starttime is field 22.
    if len(fields) <= 19:
        raise ValueError("process_identity")
    value = int(fields[19])
    if value <= 0:
        raise ValueError("process_identity")
    return value


def _readiness_max_age_ns(config: Config) -> int:
    # Pika heartbeat detection is bounded by the negotiated heartbeat and
    # blocked-connection timeout; include reconnect backoff for transient loss.
    seconds = (3 * config.rabbit_heartbeat_seconds +
               config.rabbit_blocked_connection_timeout_seconds +
               config.reconnect_backoff_seconds)
    return int(seconds * 1_000_000_000)


class ReadinessMarker:
    """Private tmpfs marker written only after OCR and Rabbit setup both pass."""

    def __init__(self, config: Config):
        self.path = config.readiness_marker
        self.boot_nonce = secrets.token_hex(16)
        _require_private_directory(self.path.parent)

    def publish(self, child_identity: tuple[int, int]) -> None:
        consumer_pid = os.getpid()
        consumer_start = _proc_start_ticks(consumer_pid)
        child_pid, child_start = child_identity
        payload = {
            "schema_version": 1,
            "status": "ready",
            "boot_nonce": self.boot_nonce,
            "updated_monotonic_ns": time.monotonic_ns(),
            "consumer_pid": consumer_pid,
            "consumer_start_ticks": consumer_start,
            "ocr_pid": child_pid,
            "ocr_start_ticks": child_start,
        }
        _write_readiness_atomically(self.path, payload)

    def clear(self) -> None:
        try:
            _require_private_directory(self.path.parent)
            info = self.path.lstat()
            if stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode) and info.st_uid == os.geteuid():
                self.path.unlink()
                directory_fd = os.open(self.path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
        except FileNotFoundError:
            return
        except Exception:
            # An unreadable/undeletable marker is rejected by the health probe
            # unless it remains valid and fresh; callers still close Rabbit.
            return


def _write_readiness_atomically(path: Path, payload: dict[str, Any]) -> None:
    _require_private_directory(path.parent)
    encoded = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("ascii")
    temporary = path.parent / (".ready-" + secrets.token_hex(8))
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(temporary, flags, 0o600)
    try:
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(encoded):
            offset += os.write(fd, encoded[offset:])
        os.fsync(fd)
    finally:
        os.close(fd)
    try:
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def readiness_healthy(config: Config, *, now_monotonic_ns: int | None = None,
                      proc_root: Path = Path("/proc")) -> bool:
    try:
        _require_private_directory(config.readiness_marker.parent)
        parent_info = config.readiness_marker.parent.lstat()
        info = config.readiness_marker.lstat()
        if (not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or
                info.st_mode & 0o077 or info.st_uid != os.geteuid() or info.st_uid != parent_info.st_uid or info.st_size > 2048):
            return False
        value = strict_json_object(config.readiness_marker.read_bytes(), max_bytes=2048)
        if set(value) != READINESS_FIELDS or value.get("schema_version") != 1 or value.get("status") != "ready":
            return False
        if not isinstance(value.get("boot_nonce"), str) or not BOOT_NONCE_RE.fullmatch(value["boot_nonce"]):
            return False
        numeric = ("updated_monotonic_ns", "consumer_pid", "consumer_start_ticks", "ocr_pid", "ocr_start_ticks")
        if any(isinstance(value.get(field), bool) or not isinstance(value.get(field), int) or value[field] <= 0 for field in numeric):
            return False
        now = time.monotonic_ns() if now_monotonic_ns is None else now_monotonic_ns
        age = now - value["updated_monotonic_ns"]
        if age < 0 or age > _readiness_max_age_ns(config):
            return False
        if _proc_start_ticks(value["consumer_pid"], proc_root) != value["consumer_start_ticks"]:
            return False
        if _proc_start_ticks(value["ocr_pid"], proc_root) != value["ocr_start_ticks"]:
            return False
        return True
    except Exception:
        return False


class GoWorkerClient:
    def __init__(self, base_url: str, token: str, timeout_ms: int, attempts: int):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout_ms = timeout_ms
        self.attempts = attempts
        self.opener = urllib.request.build_opener(_NoRedirect())

    def post(self, route: str, body: dict[str, Any], completion_deadline: float | None = None) -> dict[str, Any]:
        encoded = json.dumps(body, separators=(",", ":")).encode("utf-8")
        if len(encoded) > MAX_WORKER_REQUEST_BYTES:
            raise ConsumerError("go_boundary_request_size")
        last_error: Exception | None = None
        for index in range(self.attempts):
            timeout_ms = self.timeout_ms
            if completion_deadline is not None:
                remaining_ms = int((completion_deadline - time.monotonic()) * 1000)
                if remaining_ms <= 0:
                    raise TemporaryBoundaryError("go_boundary_timeout")
                timeout_ms = min(timeout_ms, remaining_ms)
            request = urllib.request.Request(
                self.base_url + route,
                data=encoded,
                headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"},
                method="POST",
            )
            try:
                with self.opener.open(request, timeout=timeout_ms / 1000) as response:
                    if response.status != 200:
                        raise TemporaryBoundaryError("go_boundary_status")
                    data = response.read(MAX_HTTP_RESPONSE_BYTES + 1)
                    if len(data) > MAX_HTTP_RESPONSE_BYTES:
                        raise TemporaryBoundaryError("go_boundary_response_size")
                    return strict_json_object(data, max_bytes=MAX_HTTP_RESPONSE_BYTES)
            except urllib.error.HTTPError as error:
                # Do not read or report error bodies; they can contain sensitive diagnostics.
                if 300 <= error.code < 400:
                    error.close()
                    raise ConsumerError("go_boundary_redirect") from None
                if error.code in (400, 404, 422):
                    error.close()
                    raise PermanentDeliveryError("invalid_delivery") from None
                last_error = TemporaryBoundaryError("go_boundary_status")
                error.close()
            except urllib.error.URLError:
                last_error = TemporaryBoundaryError("go_boundary_unavailable")
            except TimeoutError:
                last_error = TemporaryBoundaryError("go_boundary_timeout")
            except ConsumerError:
                raise
            except Exception:
                last_error = TemporaryBoundaryError("go_boundary_unavailable")
            if index + 1 < self.attempts:
                delay = 0.2 * (index + 1)
                if completion_deadline is not None:
                    remaining = completion_deadline - time.monotonic()
                    if remaining <= 0:
                        break
                    delay = min(delay, remaining)
                time.sleep(delay)
        raise last_error or TemporaryBoundaryError("go_boundary_unavailable")

    def claim(self, envelope: Envelope) -> dict[str, Any]:
        return self.post("/internal/worker/v1/claim", envelope.as_json())

    def complete(self, envelope: Envelope, execution: dict[str, Any], fields: dict[str, str | None],
                 completion_deadline: float) -> dict[str, Any]:
        body = envelope.as_json()
        body.update({"attempt_token": execution["attempt_token"], "fields": fields})
        return self.post("/internal/worker/v1/complete", body, completion_deadline)

    def fail(self, envelope: Envelope, execution: dict[str, Any], error_code: str,
             completion_deadline: float | None = None) -> dict[str, Any]:
        if error_code not in SAFE_ERRORS:
            error_code = "processing_failed"
        body = envelope.as_json()
        body.update({"attempt_token": execution["attempt_token"], "error_code": error_code})
        return self.post("/internal/worker/v1/fail", body, completion_deadline)

    def reject(self, envelope: Envelope, error_code: str) -> dict[str, Any]:
        del error_code  # Go assigns the persisted safe rejection category.
        return self.post("/internal/worker/v1/reject", envelope.as_json())


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        return None


class PipelineSupervisor:
    """Own one killable, resident OCR child; never fake-cancel a model call."""

    def __init__(self, config: Config, worker_script: Path | None = None):
        self.config = config
        self.worker_script = worker_script or Path(__file__).with_name("russiandocs_worker.py")
        self.child: subprocess.Popen[bytes] | None = None
        self.lock = threading.Lock()

    def start(self) -> None:
        with self.lock:
            self._start_locked()

    def readiness_identity(self) -> tuple[int, int] | None:
        # Do not take the recognize lock here: Rabbit heartbeats must continue
        # during a bounded OCR operation.
        child = self.child
        if child is None or child.poll() is not None:
            return None
        try:
            return child.pid, _proc_start_ticks(child.pid)
        except Exception:
            return None

    def _start_locked(self) -> None:
        if self.child is not None and self.child.poll() is None:
            return
        self._stop_locked()
        command = [
            sys.executable, str(self.worker_script),
            "--repo", str(self.config.russiandocs_repo),
            "--models-root", str(self.config.models_root),
            "--work-root", str(self.config.work_root),
            "--image-root", str(self.config.image_root),
            "--max-input-bytes", str(self.config.max_input_bytes),
            "--max-image-pixels", str(self.config.max_image_pixels),
            "--expected-russiandocs-version", self.config.expected_upstream_version,
            "--expected-models-version", self.config.expected_model_lock_version,
            "--parent-pid", str(os.getpid()),
        ]
        try:
            child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, start_new_session=True)
            self.child = child
            line = self._readline(child, self.config.startup_timeout_seconds)
            ready = strict_json_object(line, max_bytes=MAX_PROTOCOL_LINE_BYTES)
            if ready != {"schema_version": PIPELINE_SCHEMA, "type": "ready"}:
                raise ConsumerError("ocr_worker_not_ready")
        except Exception:
            self._stop_locked()
            raise ConsumerError("ocr_unavailable") from None

    @staticmethod
    def _readline(child: subprocess.Popen[bytes], timeout: float) -> bytes:
        assert child.stdout is not None
        ready: list[bytes | Exception] = []

        def read() -> None:
            try:
                line = child.stdout.readline(MAX_PROTOCOL_LINE_BYTES + 1)
                if not line or len(line) > MAX_PROTOCOL_LINE_BYTES or not line.endswith(b"\n"):
                    raise ConsumerError("ocr_protocol_failure")
                ready.append(line)
            except Exception as error:
                ready.append(error)

        thread = threading.Thread(target=read, daemon=True)
        thread.start()
        thread.join(timeout)
        if thread.is_alive():
            raise ConsumerError("ocr_read_timeout")
        if not ready or isinstance(ready[0], Exception):
            raise ConsumerError("ocr_protocol_failure")
        return ready[0]

    def recognize(self, request_id: str, image_id: str, image_path: Path, media_type: str, timeout_ms: int) -> dict[str, str | None]:
        if not _uuid_v4(request_id) or not _uuid_v4(image_id) or timeout_ms <= 0:
            raise ConsumerError("invalid_execution")
        try:
            resolved = image_path.resolve(strict=True)
            resolved.relative_to(self.config.image_root.resolve(strict=True))
            info = image_path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_size <= 0 or info.st_size > self.config.max_input_bytes:
                raise ConsumerError("image_unprocessable")
        except ConsumerError:
            raise
        except Exception:
            raise ConsumerError("image_unprocessable") from None
        request = {
            "schema_version": PIPELINE_SCHEMA, "type": "recognize", "request_id": request_id,
            "image_id": image_id, "image_path": str(resolved), "media_type": media_type,
        }
        encoded = json.dumps(request, separators=(",", ":")).encode("utf-8") + b"\n"
        if len(encoded) > MAX_PROTOCOL_LINE_BYTES:
            raise ConsumerError("image_unprocessable")
        with self.lock:
            try:
                self._start_locked()
                assert self.child is not None and self.child.stdin is not None
                self.child.stdin.write(encoded)
                self.child.stdin.flush()
                line = self._readline(self.child, timeout_ms / 1000)
                response = strict_json_object(line, max_bytes=MAX_PROTOCOL_LINE_BYTES)
                identity = {"schema_version", "type", "request_id", "image_id", "status", "code"}
                if (not identity.issubset(response) or response.get("schema_version") != PIPELINE_SCHEMA or
                        response.get("type") != "result" or response.get("request_id") != request_id or
                        response.get("image_id") != image_id):
                    raise ConsumerError("ocr_protocol_failure")
                if response.get("status") != "completed" or response.get("code") != "ok":
                    if set(response) != identity:
                        raise ConsumerError("ocr_protocol_failure")
                    code = response.get("code")
                    raise ConsumerError("image_unprocessable" if code == "invalid_image" else "processing_failed")
                if set(response) != identity | {"fields"}:
                    raise ConsumerError("ocr_protocol_failure")
                fields = response["fields"]
                if not isinstance(fields, dict) or set(fields) != set(FIELDS):
                    raise ConsumerError("ocr_protocol_failure")
                if any(value is not None and not isinstance(value, str) for value in fields.values()):
                    raise ConsumerError("ocr_protocol_failure")
                return fields
            except ConsumerError as error:
                if str(error) in ("ocr_read_timeout",):
                    self._stop_locked()
                    raise ConsumerError("processing_timeout") from None
                if str(error) in ("ocr_startup_timeout", "ocr_unavailable"):
                    self._stop_locked()
                    raise ConsumerError("ocr_unavailable") from None
                if str(error) == "image_unprocessable":
                    raise
                self._stop_locked()
                raise ConsumerError("processing_failed") from None
            except Exception:
                self._stop_locked()
                raise ConsumerError("processing_failed") from None

    def _stop_locked(self) -> None:
        child, self.child = self.child, None
        if child is None or child.poll() is not None:
            if child is not None:
                try:
                    child.wait(timeout=0)
                except Exception:
                    pass
            return
        try:
            os.killpg(child.pid, signal.SIGTERM)
            child.wait(timeout=self.config.processing_stop_grace_seconds)
        except Exception:
            try:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=self.config.processing_stop_grace_seconds)
            except Exception:
                pass

    def close(self) -> None:
        with self.lock:
            self._stop_locked()


def _image_path(root: Path, key: object) -> Path:
    if not _uuid_v4(key):
        raise ConsumerError("invalid_image_key")
    path = root / str(key)
    try:
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ConsumerError("image_unprocessable")
        resolved = path.resolve(strict=True)
        resolved.relative_to(root.resolve(strict=True))
    except ConsumerError:
        raise
    except Exception:
        raise ConsumerError("image_unprocessable") from None
    return path


def _validate_execution(value: Any, envelope: Envelope, config: Config) -> dict[str, Any]:
    required = {
        "attempt_token", "image_key", "media_type", "size_bytes", "attempt_no",
        "lease_expires_at", "pipeline_version", "hard_processing_timeout_ms",
        "completion_budget_ms", "deadline_at",
    }
    if not isinstance(value, dict) or set(value) != required:
        raise ConsumerError("invalid_claim_response")
    if (not _uuid_v4(value["attempt_token"]) or
            not _uuid_v4(value["image_key"]) or isinstance(value["attempt_no"], bool) or
            not isinstance(value["attempt_no"], int) or value["attempt_no"] != envelope.attempt_no or
            isinstance(value["size_bytes"], bool) or not isinstance(value["size_bytes"], int) or
            not 0 < value["size_bytes"] <= config.max_input_bytes or
            isinstance(value["hard_processing_timeout_ms"], bool) or not isinstance(value["hard_processing_timeout_ms"], int) or
            value["hard_processing_timeout_ms"] <= 0 or
            isinstance(value["completion_budget_ms"], bool) or not isinstance(value["completion_budget_ms"], int) or
            value["completion_budget_ms"] <= 0 or not isinstance(value["media_type"], str) or
            value["media_type"] not in {"image/jpeg", "image/png", "image/webp"} or
            not isinstance(value["pipeline_version"], str) or not value["pipeline_version"] or
            not isinstance(value["lease_expires_at"], str) or not isinstance(value["deadline_at"], str)):
        raise ConsumerError("invalid_claim_response")
    try:
        from datetime import datetime, timezone
        lease = datetime.fromisoformat(value["lease_expires_at"].replace("Z", "+00:00"))
        deadline = datetime.fromisoformat(value["deadline_at"].replace("Z", "+00:00"))
        now = datetime.now(timezone.utc)
        if (lease.tzinfo is None or deadline.tzinfo is None or lease <= now or deadline <= now or
                deadline > lease):
            raise ValueError("lease_expired")
    except Exception:
        raise ConsumerError("invalid_claim_response") from None
    return value


def _execution_timing_error(execution: dict[str, Any], config: Config) -> bool:
    """Check broker/HTTP deadlines against Go's claimed lease and completion budget."""
    from datetime import datetime, timezone
    try:
        now = datetime.now(timezone.utc)
        lease = datetime.fromisoformat(execution["lease_expires_at"].replace("Z", "+00:00"))
        deadline = datetime.fromisoformat(execution["deadline_at"].replace("Z", "+00:00"))
        remaining_lease_ms = int((lease - now).total_seconds() * 1000)
        remaining_processing_ms = int((deadline - now).total_seconds() * 1000)
        completion_budget_ms = execution["completion_budget_ms"]
        if config.http_timeout_ms > completion_budget_ms:
            return True
        if config.consumer_ack_timeout_ms <= remaining_lease_ms + completion_budget_ms:
            return True
        return remaining_lease_ms <= 0 or remaining_processing_ms <= 0 or remaining_processing_ms > execution["hard_processing_timeout_ms"]
    except Exception:
        return True


class DeliveryProcessor:
    def __init__(self, config: Config, client: GoWorkerClient, pipeline: PipelineSupervisor,
                 sleep: Callable[[float], None] = time.sleep):
        self.config = config
        self.client = client
        self.pipeline = pipeline
        self.sleep = sleep

    def _await_durable_callback(self, callback: Callable[[float], dict[str, Any]], label: str,
                                budget_ms: int) -> bool:
        deadline = time.monotonic() + budget_ms / 1000
        while True:
            result = callback(deadline)
            decision = result.get("decision")
            if decision == "ack":
                if set(result) != {"decision"}:
                    raise ConsumerError(f"invalid_{label}_response")
                return True
            if decision != "wait" or set(result) != {"decision", "retry_after_ms"}:
                raise ConsumerError(f"invalid_{label}_response")
            delay_ms = result.get("retry_after_ms")
            if isinstance(delay_ms, bool) or not isinstance(delay_ms, int) or delay_ms <= 0:
                raise ConsumerError("invalid_wait_response")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ConsumerError("go_boundary_unavailable")
            self.sleep(min(delay_ms / 1000, self.config.wait_poll_max_seconds, remaining))
            if time.monotonic() >= deadline:
                raise ConsumerError("go_boundary_unavailable")

    def process(self, envelope: Envelope) -> bool:
        """Return True only when Go authoritatively says the delivery can ACK."""
        while True:
            result = self.client.claim(envelope)
            decision = result.get("decision")
            if decision == "ack":
                if set(result) != {"decision"}:
                    raise ConsumerError("invalid_claim_response")
                return True
            if decision == "wait":
                delay_ms = result.get("retry_after_ms")
                if set(result) != {"decision", "retry_after_ms"} or isinstance(delay_ms, bool) or not isinstance(delay_ms, int) or delay_ms <= 0:
                    raise ConsumerError("invalid_wait_response")
                self.sleep(min(delay_ms / 1000, self.config.wait_poll_max_seconds))
                continue
            if decision != "execute" or set(result) != {"decision", "execution"}:
                raise ConsumerError("invalid_claim_response")
            execution = _validate_execution(result.get("execution"), envelope, self.config)
            error_code: str | None = None
            if _execution_timing_error(execution, self.config):
                error_code = "ocr_unavailable"
            fields: dict[str, str | None] | None = None
            if error_code is None:
                try:
                    path = _image_path(self.config.image_root, execution["image_key"])
                    info = path.stat()
                    if info.st_size != execution["size_bytes"]:
                        raise ConsumerError("image_unprocessable")
                    fields = self.pipeline.recognize(envelope.job_id, execution["image_key"], path,
                                                     execution["media_type"], execution["hard_processing_timeout_ms"])
                except ConsumerError as error:
                    error_code = str(error)
                except Exception:
                    error_code = "processing_failed"
            if error_code is not None:
                code = error_code if error_code in SAFE_ERRORS else "processing_failed"
                return self._await_durable_callback(
                    lambda deadline: self.client.fail(envelope, execution, code, deadline),
                    "failure", execution["completion_budget_ms"],
                )
            assert fields is not None
            return self._await_durable_callback(
                lambda deadline: self.client.complete(envelope, execution, fields, deadline),
                "completion", execution["completion_budget_ms"],
            )

    def reject(self, envelope: Envelope, code: str) -> bool:
        while True:
            result = self.client.reject(envelope, code)
            decision = result.get("decision")
            if decision == "ack":
                if set(result) != {"decision"}:
                    raise ConsumerError("invalid_rejection_response")
                return True
            if decision != "wait":
                raise ConsumerError("invalid_rejection_response")
            delay_ms = result.get("retry_after_ms")
            if set(result) != {"decision", "retry_after_ms"} or isinstance(delay_ms, bool) or not isinstance(delay_ms, int) or delay_ms <= 0:
                raise ConsumerError("invalid_wait_response")
            self.sleep(min(delay_ms / 1000, self.config.wait_poll_max_seconds))


class RabbitWorker:
    """Pika BlockingConnection adapter with heartbeats on its I/O thread."""

    def __init__(self, config: Config, client: GoWorkerClient, processor: DeliveryProcessor,
                 readiness: ReadinessMarker | None = None):
        self.config = config
        self.client = client
        self.processor = processor
        self.readiness = readiness
        self.generation = 0
        self.connection = None
        self.channel = None
        self.delivery_thread: threading.Thread | None = None
        self.delivery_lock = threading.Lock()

    def run(self) -> None:
        import pika

        password = self.config.read_secret(self.config.rabbit_password_file)
        parameters = pika.ConnectionParameters(
            host=self.config.rabbit_host, port=self.config.rabbit_port,
            virtual_host=self.config.rabbit_vhost,
            credentials=pika.PlainCredentials(self.config.rabbit_user, password),
            heartbeat=self.config.rabbit_heartbeat_seconds,
            blocked_connection_timeout=self.config.rabbit_blocked_connection_timeout_seconds,
            connection_attempts=1, socket_timeout=self.config.rabbit_blocked_connection_timeout_seconds,
        )
        if self.readiness is not None:
            self.readiness.clear()
        while True:
            connection = None
            retry = False
            try:
                self.processor.pipeline.start()
                connection = pika.BlockingConnection(parameters)
                self.connection = connection
                self.generation += 1
                generation = self.generation
                channel = connection.channel()
                self.channel = channel
                self._declare_topology(channel)
                channel.basic_qos(prefetch_count=1)
                channel.basic_consume(
                    queue=self.config.queue,
                    on_message_callback=lambda ch, method, properties, body: self._on_delivery(
                        generation, ch, method, properties, body),
                    auto_ack=False,
                )
                if self.readiness is not None:
                    self._start_readiness(connection, channel, generation)
                channel.start_consuming()
            except KeyboardInterrupt:
                return
            except Exception:
                # Never emit exception strings: pika exceptions can include connection values.
                retry = True
            finally:
                if self.readiness is not None:
                    self.readiness.clear()
                self.generation += 1
                self.channel = None
                self.connection = None
                if connection is not None:
                    try:
                        if connection.is_open:
                            connection.close()
                    except Exception:
                        pass
                thread = self.delivery_thread
                if thread is not None and thread.is_alive():
                    thread.join(self.config.processing_stop_grace_seconds + self.config.http_timeout_ms / 1000)
                if thread is not None and thread.is_alive():
                    # Do not overlap an old OCR attempt with a replacement delivery.
                    self.processor.pipeline.close()
                    while thread.is_alive():
                        thread.join(min(0.1, self.config.http_timeout_ms / 1000))
                self.delivery_thread = None
            if retry:
                time.sleep(self.config.reconnect_backoff_seconds)

    def _start_readiness(self, connection, channel, generation: int) -> None:
        if self.readiness is None:
            return
        identity = self.processor.pipeline.readiness_identity()
        if identity is None:
            raise ConsumerError("ocr_worker_not_ready")
        self.readiness.publish(identity)
        connection.call_later(self._readiness_tick_seconds(),
                              functools.partial(self._refresh_readiness, connection, channel, generation))

    def _refresh_readiness(self, connection, channel, generation: int) -> None:
        if self.readiness is None:
            return
        if (generation != self.generation or connection is not self.connection or channel is not self.channel):
            self.readiness.clear()
            return
        try:
            if not connection.is_open or not channel.is_open:
                self.readiness.clear()
                return
            identity = self.processor.pipeline.readiness_identity()
            if identity is None:
                self.readiness.clear()
                connection.close()
                return
            self.readiness.publish(identity)
            connection.call_later(self._readiness_tick_seconds(),
                                  functools.partial(self._refresh_readiness, connection, channel, generation))
        except Exception:
            self.readiness.clear()
            try:
                if connection.is_open:
                    connection.close()
            except Exception:
                pass

    def _readiness_tick_seconds(self) -> float:
        return max(0.1, self.config.rabbit_heartbeat_seconds / 3)

    def _declare_topology(self, channel) -> None:
        channel.exchange_declare(exchange=self.config.exchange, exchange_type="direct", durable=True)
        channel.exchange_declare(exchange=self.config.dead_letter_exchange, exchange_type="direct", durable=True)
        channel.queue_declare(queue=self.config.dead_letter_queue, durable=True, arguments={"x-queue-type": "quorum"})
        channel.queue_bind(queue=self.config.dead_letter_queue, exchange=self.config.dead_letter_exchange,
                           routing_key="recognition.dead")
        channel.queue_declare(queue=self.config.queue, durable=True, arguments={
            "x-queue-type": "quorum",
            "x-delivery-limit": -1,
            "x-single-active-consumer": True,
            "x-consumer-timeout": self.config.consumer_ack_timeout_ms,
            "x-dead-letter-exchange": self.config.dead_letter_exchange,
            "x-dead-letter-routing-key": "recognition.dead",
        })
        channel.queue_bind(queue=self.config.queue, exchange=self.config.exchange,
                           routing_key=self.config.routing_key)

    def _on_delivery(self, generation, channel, method, properties, body: bytes) -> None:
        envelope, error_code = parse_envelope(body)
        if envelope is None:
            channel.basic_reject(delivery_tag=method.delivery_tag, requeue=False)
            return
        if properties is None or getattr(properties, "message_id", None) != envelope.event_id:
            error_code = "message_id_mismatch"
        if self.delivery_thread is not None and self.delivery_thread.is_alive():
            # Prefetch=1 makes this a broker/protocol invariant violation; dead-letter it.
            channel.basic_reject(delivery_tag=method.delivery_tag, requeue=False)
            return

        def process() -> None:
            connection = self.connection
            try:
                ack = self.processor.reject(envelope, error_code) if error_code else self.processor.process(envelope)
                action = "ack" if ack else "close"
            except PermanentDeliveryError:
                action = "reject"
            except Exception:
                # Leave the delivery unacked. Closing the channel causes broker redelivery.
                action = "close"
            callback = functools.partial(self._settle, generation, method.delivery_tag, action)
            try:
                connection.add_callback_threadsafe(callback)
            except Exception:
                pass

        thread = threading.Thread(target=process, name="passport-ocr-delivery", daemon=True)
        self.delivery_thread = thread
        thread.start()

    def _settle(self, generation: int, delivery_tag: int, action: str) -> None:
        if generation != self.generation or self.channel is None or not self.channel.is_open:
            return
        try:
            if action == "ack":
                self.channel.basic_ack(delivery_tag=delivery_tag)
            elif action == "reject":
                self.channel.basic_reject(delivery_tag=delivery_tag, requeue=False)
            else:
                self.channel.close()
        except Exception:
            pass


def run(config: Config) -> None:
    logging.disable(logging.CRITICAL)
    readiness = ReadinessMarker(config)
    readiness.clear()
    worker_token = config.read_secret(config.worker_token_file)
    client = GoWorkerClient(config.go_worker_url, worker_token,
                            config.http_timeout_ms, config.boundary_attempts)
    pipeline = PipelineSupervisor(config)
    try:
        processor = DeliveryProcessor(config, client, pipeline)
        RabbitWorker(config, client, processor, readiness).run()
    finally:
        readiness.clear()
        pipeline.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--check-config", action="store_true",
                        help="validate worker configuration and selected source/model snapshot only")
    parser.add_argument("command", nargs="?", choices=("run", "healthcheck"), default="run")
    args = parser.parse_args(argv)
    if args.check_config:
        try:
            config = Config.load(args.config)
            config.read_secret(config.rabbit_password_file)
            config.read_secret(config.worker_token_file)
            validate_pipeline_snapshot(config.russiandocs_repo, config.models_root,
                                       config.expected_upstream_version, config.expected_model_lock_version)
        except Exception:
            print("worker_configuration_invalid", file=sys.stderr)
            return 2
        print("configuration_valid")
        return 0
    if args.command == "healthcheck":
        try:
            return 0 if readiness_healthy(Config.load(args.config)) else 1
        except Exception:
            return 1
    def stop(signum, frame):
        raise KeyboardInterrupt

    previous_sigterm = signal.signal(signal.SIGTERM, stop)
    try:
        run(Config.load(args.config))
    except ConsumerError as error:
        print(str(error), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 0
    finally:
        signal.signal(signal.SIGTERM, previous_sigterm)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
