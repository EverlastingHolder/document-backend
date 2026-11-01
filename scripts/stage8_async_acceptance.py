#!/usr/bin/env python3
"""Privacy-safe bounded load/quality harness for the async passport API.

The image is read once into memory and multipart bodies are sent in pieces, so
the harness does not make one image-sized copy per concurrent request. Output
contains aggregate counts and timings only; identifiers and response payloads
remain in process memory.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import http.client
import ipaddress
import json
import math
import os
import re
import secrets
import ssl
import statistics
import sys
import threading
import time
import urllib.parse
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable


FIELDS = (
    "surname", "given_name", "patronymic", "gender", "birth_date",
    "birth_place", "series", "number", "issue_date", "issuer",
    "department_code",
)
MEDIA_TYPES = {"image/jpeg", "image/png", "image/webp"}
SAFE_FAILURES = {
    "configuration_invalid", "image_unavailable", "image_too_large",
    "enrollment_failed", "http_transport_failed", "http_status_unexpected",
    "response_invalid", "post_failed", "poll_failed", "deadline_exceeded",
}
MAX_RESPONSE_BYTES = 64 * 1024
ALIAS_RE = re.compile(r"^[a-z][a-z0-9_-]{0,47}$")


class HarnessFailure(Exception):
    def __init__(self, category: str):
        self.category = category if category in SAFE_FAILURES else "response_invalid"
        super().__init__(self.category)


@dataclass(frozen=True)
class Endpoint:
    scheme: str
    host: str
    port: int

    @classmethod
    def parse(cls, value: str, allow_private_http: bool = False) -> "Endpoint":
        try:
            parsed = urllib.parse.urlsplit(value)
            if parsed.scheme not in ("https", "http") or not parsed.hostname:
                raise ValueError
            if parsed.username is not None or parsed.password is not None:
                raise ValueError
            if parsed.path not in ("", "/") or parsed.query or parsed.fragment:
                raise ValueError
            port = parsed.port or (443 if parsed.scheme == "https" else 80)
            if parsed.scheme == "http" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
                try:
                    address = ipaddress.ip_address(parsed.hostname)
                except ValueError:
                    raise ValueError from None
                if not allow_private_http or not (address.is_private or address.is_link_local):
                    raise ValueError
            return cls(parsed.scheme, parsed.hostname, port)
        except (ValueError, TypeError):
            raise HarnessFailure("configuration_invalid") from None

    def connection(self, timeout: float) -> http.client.HTTPConnection:
        if self.scheme == "https":
            return http.client.HTTPSConnection(self.host, self.port, timeout=timeout, context=ssl.create_default_context())
        return http.client.HTTPConnection(self.host, self.port, timeout=timeout)


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def _decode_json(data: bytes) -> Any:
    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        raise HarnessFailure("response_invalid") from None


def _read_response(response: http.client.HTTPResponse) -> bytes:
    data = response.read(MAX_RESPONSE_BYTES + 1)
    if len(data) > MAX_RESPONSE_BYTES:
        raise HarnessFailure("response_invalid")
    return data


def _request_json(endpoint: Endpoint, method: str, path: str, body: bytes | None,
                  headers: dict[str, str], timeout: float) -> tuple[int, dict[str, str], bytes]:
    connection = endpoint.connection(timeout)
    try:
        connection.request(method, path, body=body, headers=headers)
        response = connection.getresponse()
        data = _read_response(response)
        return response.status, {key.lower(): value for key, value in response.getheaders()}, data
    except HarnessFailure:
        raise
    except (OSError, http.client.HTTPException, ssl.SSLError, TimeoutError):
        raise HarnessFailure("http_transport_failed") from None
    finally:
        connection.close()


def _token() -> str:
    return "pbt1_" + base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode("ascii")


def enroll(endpoint: Endpoint, timeout: float) -> tuple[str, str]:
    token = _token()
    body = json.dumps({"token_sha256": hashlib.sha256(token.encode("ascii")).hexdigest()},
                      separators=(",", ":")).encode("ascii")
    status, _, response_body = _request_json(
        endpoint, "POST", "/v1/client-installations", body,
        {"Content-Type": "application/json", "Accept": "application/json"}, timeout,
    )
    if status not in (200, 201):
        raise HarnessFailure("enrollment_failed")
    payload = _decode_json(response_body)
    if not isinstance(payload, dict) or not isinstance(payload.get("client_id"), str):
        raise HarnessFailure("response_invalid")
    try:
        uuid.UUID(payload["client_id"])
    except (ValueError, AttributeError):
        raise HarnessFailure("response_invalid") from None
    return token, payload["client_id"]


class ControlFile:
    """Optional private control ledger for the external cleanup/DB observer."""

    def __init__(self, path: str | None, mode: str):
        self.path: Path | None = None
        self.run_id = str(uuid.uuid4())
        self._lock = threading.Lock()
        self.healthy = True
        if path is None:
            return
        try:
            requested = Path(path)
            parent = requested.parent.resolve(strict=True)
            info = parent.stat()
            if not parent.is_dir() or info.st_uid != os.getuid() or (info.st_mode & 0o077) != 0:
                raise HarnessFailure("configuration_invalid")
            self.path = parent / requested.name
            if self.path.exists():
                existing = self.path.lstat()
                if (self.path.is_symlink() or not self.path.is_file() or existing.st_uid != os.getuid() or
                        (existing.st_mode & 0o077) != 0 or existing.st_size > 1 << 20):
                    raise HarnessFailure("configuration_invalid")
                records = [json.loads(line, object_pairs_hook=_reject_duplicate_keys)
                           for line in self.path.read_text(encoding="utf-8").splitlines()]
                if (len(records) != 2 or records[0].keys() != {"run_id"} or
                        records[1] != {"mode": mode} or not isinstance(records[0]["run_id"], str)):
                    raise HarnessFailure("configuration_invalid")
                uuid.UUID(records[0]["run_id"])
                self.run_id = records[0]["run_id"]
                return
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
            if hasattr(os, "O_NOFOLLOW"):
                flags |= os.O_NOFOLLOW
            fd = os.open(self.path, flags, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                stream.write(json.dumps({"run_id": self.run_id}, separators=(",", ":")) + "\n")
                stream.write(json.dumps({"mode": mode}, separators=(",", ":")) + "\n")
                stream.flush()
                os.fsync(stream.fileno())
        except HarnessFailure:
            raise
        except OSError:
            raise HarnessFailure("configuration_invalid") from None

    def record(self, **values: str) -> bool:
        if self.path is None:
            return True
        if set(values) - {"client_id", "job_id", "status"}:
            raise HarnessFailure("configuration_invalid")
        with self._lock:
            try:
                fd = os.open(self.path, os.O_WRONLY | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0))
                with os.fdopen(fd, "w", encoding="utf-8") as stream:
                    stream.write(json.dumps(values, separators=(",", ":")) + "\n")
                    stream.flush()
                    os.fsync(stream.fileno())
            except OSError:
                self.healthy = False
                return False
        return True

    def finish(self, status: str) -> bool:
        if status not in ("complete", "partial"):
            raise HarnessFailure("configuration_invalid")
        return self.record(status=status)


def _multipart_parts(image: bytes, media_type: str) -> tuple[str, bytes, bytes]:
    boundary = "stage8-" + secrets.token_hex(16)
    prefix = (
        f"--{boundary}\r\n"
        'Content-Disposition: form-data; name="file"; filename="upload"\r\n'
        f"Content-Type: {media_type}\r\n\r\n"
    ).encode("ascii")
    suffix = f"\r\n--{boundary}--\r\n".encode("ascii")
    return boundary, prefix, suffix


def _post_image(endpoint: Endpoint, token: str, image: bytes, media_type: str,
                key: str, timeout: float, deadline: float) -> tuple[int, str | None, float, list[int]]:
    boundary, prefix, suffix = _multipart_parts(image, media_type)
    body_length = len(prefix) + len(image) + len(suffix)
    started = time.monotonic()
    attempt_statuses: list[int] = []
    for attempt in range(3):
        if time.monotonic() >= deadline:
            break
        connection = endpoint.connection(min(timeout, max(0.1, deadline - time.monotonic())))
        try:
            connection.putrequest("POST", "/v1/passport/jobs")
            connection.putheader("Authorization", "Bearer " + token)
            connection.putheader("Idempotency-Key", key)
            connection.putheader("Accept", "application/json")
            connection.putheader("Content-Type", f"multipart/form-data; boundary={boundary}")
            connection.putheader("Content-Length", str(body_length))
            connection.endheaders()
            connection.send(prefix)
            connection.send(image)
            connection.send(suffix)
            response = connection.getresponse()
            payload_bytes = _read_response(response)
            attempt_statuses.append(response.status)
            if response.status in (200, 202):
                payload = _decode_json(payload_bytes)
                if response.status != 202:
                    return response.status, None, time.monotonic() - started, attempt_statuses
                job_id = payload.get("job_id") if isinstance(payload, dict) else None
                try:
                    uuid.UUID(job_id)
                except (ValueError, TypeError, AttributeError):
                    raise HarnessFailure("response_invalid") from None
                if (set(payload) != {"job_id", "status", "status_url", "created_at"} or
                        payload["status"] not in ("queued", "processing") or
                        payload["status_url"] != f"/v1/passport/jobs/{job_id}" or
                        not isinstance(payload["created_at"], str)):
                    raise HarnessFailure("response_invalid")
                return response.status, job_id, time.monotonic() - started, attempt_statuses
            if response.status != 503 and response.status not in (408, 429):
                return response.status, None, time.monotonic() - started, attempt_statuses
        except HarnessFailure:
            raise
        except (OSError, http.client.HTTPException, ssl.SSLError, TimeoutError):
            pass
        finally:
            connection.close()
        if attempt < 2:
            time.sleep(min(0.1 * (attempt + 1), max(0.0, deadline - time.monotonic())))
    if attempt_statuses:
        return attempt_statuses[-1], None, time.monotonic() - started, attempt_statuses
    raise HarnessFailure("http_transport_failed")


def validate_result(result: Any) -> dict[str, Any]:
    result_keys = {"request_id", "status", "review_required", "pipeline_version", "fields", "warnings"}
    if not isinstance(result, dict) or set(result) != result_keys or not isinstance(result.get("fields"), dict):
        raise HarnessFailure("response_invalid")
    try:
        uuid.UUID(result["request_id"])
    except (ValueError, TypeError, AttributeError):
        raise HarnessFailure("response_invalid") from None
    if result["status"] != "completed" or result["review_required"] is not True:
        raise HarnessFailure("response_invalid")
    fields = result["fields"]
    if set(fields) != set(FIELDS):
        raise HarnessFailure("response_invalid")
    issue_count = 0
    for name in FIELDS:
        field = fields[name]
        if not isinstance(field, dict) or set(field) != {"value", "state", "issues"} or field.get("state") not in ("recognized", "uncertain", "missing"):
            raise HarnessFailure("response_invalid")
        if field["value"] is not None and not isinstance(field["value"], str):
            raise HarnessFailure("response_invalid")
        issues = field.get("issues")
        if not isinstance(issues, list) or any(not isinstance(issue, dict) or set(issue) != {"code", "message"} for issue in issues):
            raise HarnessFailure("response_invalid")
        issue_count += len(issues)
    if (not isinstance(result.get("warnings"), list) or
            any(not isinstance(warning, dict) or set(warning) != {"code", "message"} for warning in result["warnings"]) or
            not isinstance(result.get("pipeline_version"), str) or not result["pipeline_version"]):
        raise HarnessFailure("response_invalid")
    return {"fields": fields, "issue_count": issue_count + len(result["warnings"])}


def _read_manifest(path: Path) -> list[dict[str, Any]]:
    try:
        info = path.lstat()
        parent_info = path.parent.resolve(strict=True).stat()
        if (not path.is_file() or path.is_symlink() or info.st_size > 1 << 20 or
                info.st_uid != os.getuid() or (info.st_mode & 0o077) != 0 or (parent_info.st_mode & 0o077) != 0):
            raise HarnessFailure("configuration_invalid")
        data = path.read_bytes()
        manifest = json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys)
    except HarnessFailure:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError):
        raise HarnessFailure("configuration_invalid") from None
    if not isinstance(manifest, dict) or set(manifest) != {"fixtures"} or not isinstance(manifest["fixtures"], list):
        raise HarnessFailure("configuration_invalid")
    aliases: set[str] = set()
    groups: set[str] = set()
    fixtures = []
    for item in manifest["fixtures"]:
        required = {"alias", "image", "media_type", "expected_fields", "scored_fields"}
        if not isinstance(item, dict) or set(item) not in (required, required | {"document_group"}):
            raise HarnessFailure("configuration_invalid")
        alias, image_path, media_type, expected = item["alias"], item["image"], item["media_type"], item["expected_fields"]
        if not isinstance(alias, str) or not ALIAS_RE.fullmatch(alias) or alias in aliases:
            raise HarnessFailure("configuration_invalid")
        group = item.get("document_group", alias)
        if not isinstance(group, str) or not ALIAS_RE.fullmatch(group):
            raise HarnessFailure("configuration_invalid")
        groups.add(group)
        if not isinstance(image_path, str) or not image_path or media_type not in MEDIA_TYPES:
            raise HarnessFailure("configuration_invalid")
        if not isinstance(expected, dict) or set(expected) != set(FIELDS):
            raise HarnessFailure("configuration_invalid")
        scored = item["scored_fields"]
        if not isinstance(scored, list) or len(set(scored)) != len(scored) or any(field not in FIELDS for field in scored):
            raise HarnessFailure("configuration_invalid")
        for value in expected.values():
            if (not isinstance(value, dict) or set(value) != {"value", "state"} or
                    (value["value"] is not None and not isinstance(value["value"], str)) or
                    value["state"] not in ("recognized", "uncertain", "missing")):
                raise HarnessFailure("configuration_invalid")
        aliases.add(alias)
        fixtures.append({"alias": alias, "document_group": group, "image": Path(image_path),
                         "media_type": media_type, "expected_fields": expected, "scored_fields": scored})
    if not fixtures:
        raise HarnessFailure("configuration_invalid")
    return fixtures


def _read_image(path: Path, max_bytes: int) -> bytes:
    try:
        info = path.lstat()
        parent_info = path.parent.resolve(strict=True).stat()
        if (path.is_symlink() or not path.is_file() or info.st_size <= 0 or info.st_size > max_bytes or
                info.st_uid != os.getuid() or (info.st_mode & 0o077) != 0 or (parent_info.st_mode & 0o077) != 0):
            raise HarnessFailure("image_too_large" if info.st_size > max_bytes else "image_unavailable")
        with path.open("rb") as image_file:
            data = image_file.read(max_bytes + 1)
    except HarnessFailure:
        raise
    except OSError:
        raise HarnessFailure("image_unavailable") from None
    if not data or len(data) > max_bytes:
        raise HarnessFailure("image_too_large" if data else "image_unavailable")
    return data


def _status(endpoint: Endpoint, token: str, job_id: str, timeout: float) -> tuple[int, Any]:
    status, headers, body = _request_json(
        endpoint, "GET", f"/v1/passport/jobs/{job_id}", None,
        {"Authorization": "Bearer " + token, "Accept": "application/json"}, timeout,
    )
    return status, _decode_json(body) if body else None


def _receipt(endpoint: Endpoint, token: str, job_id: str, timeout: float, deadline: float | None = None) -> bool:
    if deadline is not None and time.monotonic() >= deadline:
        return False
    bounded_timeout = min(timeout, max(0.1, deadline - time.monotonic())) if deadline is not None else timeout
    status, _, _ = _request_json(
        endpoint, "POST", f"/v1/passport/jobs/{job_id}/receipt", b"",
        {"Authorization": "Bearer " + token, "Content-Length": "0", "Accept": "application/json"}, bounded_timeout,
    )
    if status != 204:
        return False
    bounded_timeout = min(timeout, max(0.1, deadline - time.monotonic())) if deadline is not None else timeout
    gone, payload = _status(endpoint, token, job_id, bounded_timeout)
    return gone == 410 and isinstance(payload, dict) and payload.get("code") == "job_result_gone"


def _quality(result: dict[str, Any], expected: dict[str, Any], scored_fields: list[str],
             include_field_names: bool = False) -> dict[str, Any]:
    checked = validate_result(result)
    counts = {"matched_fields": 0, "mismatched_fields": 0, "missing_fields": 0,
              "unscored_fields": len(FIELDS) - len(scored_fields), "issues": checked["issue_count"]}
    names = {"matched_field_names": [], "mismatched_field_names": [], "missing_field_names": []}
    for name in scored_fields:
        actual = checked["fields"].get(name)
        if actual is None:
            counts["missing_fields"] += 1
            names["missing_field_names"].append(name)
        elif actual["value"] == expected[name]["value"] and actual["state"] == expected[name]["state"]:
            counts["matched_fields"] += 1
            names["matched_field_names"].append(name)
        elif actual["value"] is None or actual["state"] == "missing":
            counts["missing_fields"] += 1
            names["missing_field_names"].append(name)
        else:
            counts["mismatched_fields"] += 1
            names["mismatched_field_names"].append(name)
    if include_field_names:
        counts.update(names)
    return counts


def _poll_one(endpoint: Endpoint, token: str, job_id: str, accepted_at: float,
              deadline: float, timeout: float, interval: float,
              progress: Callable[[], None] | None = None) -> dict[str, Any]:
    last_progress = time.monotonic()
    while time.monotonic() < deadline:
        try:
            status, payload = _status(endpoint, token, job_id, min(timeout, max(0.1, deadline - time.monotonic())))
        except HarnessFailure:
            return {"state": "poll_error", "job_id": job_id}
        if status == 410 and isinstance(payload, dict) and payload.get("code") == "job_result_gone":
            return {"state": "result_gone", "job_id": job_id, "queue_ms": (time.monotonic() - accepted_at) * 1000,
                    "terminal": True}
        if status != 200 or not isinstance(payload, dict):
            return {"state": "poll_error", "job_id": job_id}
        current = payload.get("status")
        common_keys = {"job_id", "status", "created_at", "updated_at", "started_at", "finished_at"}
        expected_keys = common_keys | ({"result"} if current == "completed" else {"error"} if current == "failed" else set())
        if payload.get("job_id") != job_id or set(payload) != expected_keys:
            return {"state": "invalid_result", "job_id": job_id}
        if current in ("queued", "processing"):
            if current == "queued" and payload["started_at"] is not None:
                return {"state": "invalid_result", "job_id": job_id}
            if current == "processing" and not isinstance(payload["started_at"], str):
                return {"state": "invalid_result", "job_id": job_id}
            if progress is not None and time.monotonic() - last_progress >= 30:
                progress()
                last_progress = time.monotonic()
            time.sleep(min(interval, max(0.0, deadline - time.monotonic())))
            continue
        if current == "completed":
            if not isinstance(payload["started_at"], str) or not isinstance(payload["finished_at"], str):
                return {"state": "invalid_result", "job_id": job_id, "terminal": True}
            result = payload.get("result")
            try:
                validated = validate_result(result)
            except HarnessFailure:
                return {"state": "invalid_result", "job_id": job_id, "terminal": True}
            if result.get("request_id") != job_id:
                return {"state": "invalid_result", "job_id": job_id, "terminal": True}
            return {"state": "completed", "job_id": job_id, "result": result, "queue_ms": (time.monotonic() - accepted_at) * 1000,
                    "issues": validated["issue_count"], "terminal": True}
        if current == "failed":
            if not isinstance(payload["finished_at"], str):
                return {"state": "invalid_result", "job_id": job_id, "terminal": True}
            error = payload.get("error")
            safe_codes = {"image_unprocessable", "processing_failed", "ocr_unavailable", "processing_timeout"}
            if not isinstance(error, dict) or error.get("code") not in safe_codes:
                return {"state": "invalid_result", "job_id": job_id, "terminal": True}
            return {"state": "failed", "job_id": job_id, "queue_ms": (time.monotonic() - accepted_at) * 1000,
                    "terminal": True}
        return {"state": "invalid_result", "job_id": job_id}
    return {"state": "deadline", "job_id": job_id}


def _percentile(values: list[float], percentile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, math.ceil(percentile * len(ordered)) - 1)
    return round(ordered[index], 3)


def _summary(values: list[float]) -> dict[str, float | None]:
    return {"p50_ms": _percentile(values, 0.50), "p95_ms": _percentile(values, 0.95),
            "max_ms": round(max(values), 3) if values else None}


def _write_private_report(path: str, report: dict[str, Any]) -> None:
    try:
        requested = Path(path)
        parent = requested.parent.resolve(strict=True)
        info = parent.stat()
        if not parent.is_dir() or info.st_uid != os.getuid() or (info.st_mode & 0o077) != 0:
            raise HarnessFailure("configuration_invalid")
        target = parent / requested.name
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        fd = os.open(target, flags, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
    except HarnessFailure:
        raise
    except OSError:
        raise HarnessFailure("configuration_invalid") from None


def _submit_one(endpoint: Endpoint, token: str, image: bytes, media_type: str,
                timeout: float, deadline: float, barrier: threading.Barrier,
                control: ControlFile) -> dict[str, Any]:
    key = uuid.uuid4().hex
    barrier.wait()
    accepted_at = time.monotonic()
    try:
        status, job_id, elapsed, attempt_statuses = _post_image(endpoint, token, image, media_type, key, timeout, deadline)
    except HarnessFailure as exc:
        return {"state": "post_error", "category": exc.category, "latency_ms": (time.monotonic() - accepted_at) * 1000, "http_statuses": []}
    if status == 202 and job_id:
        control.record(job_id=job_id)
        return {"state": "accepted", "job_id": job_id, "accepted_at": time.monotonic(), "latency_ms": elapsed * 1000, "http_statuses": attempt_statuses}
    return {"state": "post_rejected", "http_status": status, "latency_ms": elapsed * 1000, "http_statuses": attempt_statuses}


def _serial_fixture(endpoint: Endpoint, token: str, fixture: dict[str, Any], max_bytes: int,
                    timeout: float, deadline: float, poll_interval: float,
                    control: ControlFile, progress: Callable[[], None],
                    include_field_names: bool = False) -> dict[str, Any]:
    image = _read_image(fixture["image"], max_bytes)
    key = uuid.uuid4().hex
    status, job_id, post_elapsed, _ = _post_image(endpoint, token, image, fixture["media_type"], key, timeout, deadline)
    if status != 202 or not job_id:
        return {"alias": fixture["alias"], "state": "post_rejected"}
    control.record(job_id=job_id)
    accepted_at = time.monotonic()
    final = _poll_one(endpoint, token, job_id, accepted_at, deadline, timeout, poll_interval, progress)
    quality = None
    if final["state"] == "completed":
        quality = _quality(final["result"], fixture["expected_fields"], fixture["scored_fields"], include_field_names)
    acknowledged = _receipt(endpoint, token, job_id, timeout, deadline) if final.get("terminal") else False
    report = {"alias": fixture["alias"],
              "state": final["state"], "quality": quality, "receipt_verified": acknowledged,
              "post_ms": round(post_elapsed * 1000, 3)}
    if final.get("queue_ms") is not None:
        report["queue_to_terminal_ms"] = round(final["queue_ms"], 3)
    return report


def _progress(stage: str, done: int, total: int) -> None:
    print(json.dumps({"progress": stage, "done": done, "total": total}, separators=(",", ":")), file=sys.stderr, flush=True)


def _collect(futures: list[concurrent.futures.Future[Any]], stage: str) -> list[Any]:
    pending = set(futures)
    completed: list[Any] = []
    last_report = time.monotonic()
    while pending:
        done, pending = concurrent.futures.wait(pending, timeout=30, return_when=concurrent.futures.FIRST_COMPLETED)
        for future in done:
            try:
                completed.append(future.result())
            except Exception:
                completed.append({"state": "post_error" if stage == "post" else "poll_error",
                                  "category": "http_transport_failed"})
        now = time.monotonic()
        if now - last_report >= 30 and pending:
            _progress(stage, len(completed), len(futures))
            last_report = now
    return completed


def _receipt_task(endpoint: Endpoint, token: str, item: dict[str, Any], timeout: float,
                  deadline: float) -> bool:
    if not item.get("terminal"):
        return False
    return _receipt(endpoint, token, item["job_id"], timeout, deadline)


def run(args: argparse.Namespace) -> tuple[int, dict[str, Any]]:
    endpoint = Endpoint.parse(args.base_url, args.allow_private_http)
    if (args.mode not in ("quality", "burst") or args.max_image_bytes is None or args.max_image_bytes <= 0 or
            args.deadline_seconds is None or args.deadline_seconds <= 0 or
            args.poll_interval_seconds is None or args.poll_interval_seconds <= 0 or
            args.poll_concurrency is None or args.poll_concurrency <= 0 or args.poll_concurrency > 100 or
            args.timeout_seconds is None or args.timeout_seconds <= 0 or args.timeout_seconds > 60):
        raise HarnessFailure("configuration_invalid")
    if args.mode == "burst" and (not args.image or args.media_type not in MEDIA_TYPES):
        raise HarnessFailure("configuration_invalid")
    if args.mode == "quality" and not args.quality_manifest:
        raise HarnessFailure("configuration_invalid")
    if (args.mode == "quality" and args.image) or (args.mode == "burst" and args.quality_manifest):
        raise HarnessFailure("configuration_invalid")
    if args.mode == "burst" and args.include_field_names:
        raise HarnessFailure("configuration_invalid")
    image = _read_image(Path(args.image), args.max_image_bytes) if args.image else b""
    fixtures = _read_manifest(Path(args.quality_manifest)) if args.quality_manifest else []
    if args.mode == "quality" and len(fixtures) > 5:
        raise HarnessFailure("configuration_invalid")
    deadline = time.monotonic() + args.deadline_seconds
    control = ControlFile(args.control_file, args.mode)
    _progress("enrollment", 0, 1)
    token, client_id = enroll(endpoint, min(args.timeout_seconds, 30, args.deadline_seconds))
    control.record(client_id=client_id)
    if args.wait_for_observer:
        _progress("observer_ready", 0, 1)
        if not sys.stdin.readline():
            raise HarnessFailure("configuration_invalid")
    if args.mode == "quality":
        quality_reports = []
        for index, fixture in enumerate(fixtures):
            def report_quality_progress(index: int = index) -> None:
                _progress("quality_poll", index, len(fixtures))
            quality_reports.append(_serial_fixture(endpoint, token, fixture, args.max_image_bytes,
                                                    args.timeout_seconds, deadline, args.poll_interval_seconds,
                                                    control, report_quality_progress, args.include_field_names))
            _progress("quality", index + 1, len(fixtures))
        ok = (all(item["state"] == "completed" and item["receipt_verified"] for item in quality_reports) and
              (control.path is None or control.healthy))
        control.finish("complete" if ok else "partial")
        report = {"mode": "quality", "fixtures": quality_reports,
                  "fixture_count": len(fixtures),
                  "unique_documents": len({item["document_group"] for item in fixtures}),
                  "control_ledger_complete": control.healthy}
        return (0 if ok else 1), report

    barrier = threading.Barrier(101)
    with concurrent.futures.ThreadPoolExecutor(max_workers=100) as executor:
        futures = [executor.submit(_submit_one, endpoint, token, image, args.media_type,
                                  args.timeout_seconds, deadline, barrier, control) for _ in range(100)]
        barrier.wait()
        posts = _collect(futures, "post")
    accepted = [item for item in posts if item["state"] == "accepted"]
    terminal_by_id: dict[str, dict[str, Any]] = {}
    if accepted:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.poll_concurrency) as executor:
            futures = [executor.submit(_poll_one, endpoint, token, item["job_id"], item["accepted_at"], deadline,
                                       args.timeout_seconds, args.poll_interval_seconds) for item in accepted]
            for result in _collect(futures, "poll"):
                if result.get("job_id"):
                    terminal_by_id[result["job_id"]] = result
    receipt_jobs = [terminal_by_id[item["job_id"]] for item in accepted
                    if item["job_id"] in terminal_by_id and terminal_by_id[item["job_id"]].get("terminal")]
    receipts_verified = 0
    if receipt_jobs:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.poll_concurrency) as executor:
            futures = [executor.submit(_receipt_task, endpoint, token, item, args.timeout_seconds, deadline)
                       for item in receipt_jobs]
            receipts_verified = sum(bool(value) for value in _collect(futures, "receipt"))
    post_statuses: dict[str, int] = {}
    for item in posts:
        for status_code in item.get("http_statuses", []):
            key = str(status_code)
            post_statuses[key] = post_statuses.get(key, 0) + 1
        if not item.get("http_statuses"):
            post_statuses["transport_error"] = post_statuses.get("transport_error", 0) + 1
    states: dict[str, int] = {}
    terminal = list(terminal_by_id.values())
    for item in terminal:
        states[item["state"]] = states.get(item["state"], 0) + 1
    report = {
        "mode": "burst", "requests_requested": 100, "http_post_concurrency": 100,
        "accepted": len(accepted), "post_outcomes": post_statuses, "terminal_outcomes": states,
        "receipt_204_and_get_410": receipts_verified,
        "post_latency": _summary([item["latency_ms"] for item in posts if isinstance(item.get("latency_ms"), (int, float))]),
        "queue_to_terminal": _summary([item["queue_ms"] for item in terminal if isinstance(item.get("queue_ms"), (int, float))]),
    }
    ok = (len(accepted) == 100 and len(terminal_by_id) == len(accepted) and
          all(item["state"] in ("completed", "failed") for item in terminal) and
          receipts_verified == len(receipt_jobs) and (control.path is None or control.healthy))
    control.finish("complete" if ok else "partial")
    return (0 if ok else 1), report


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", help="API origin, without a path")
    parser.add_argument("--mode", required=True, choices=("quality", "burst"))
    parser.add_argument("--allow-private-http", action="store_true",
                        help="permit cleartext only to a literal private/link-local IP")
    parser.add_argument("--image", help="one valid representative image for burst mode; loaded into RAM")
    parser.add_argument("--media-type", choices=sorted(MEDIA_TYPES))
    parser.add_argument("--max-image-bytes", type=int)
    parser.add_argument("--deadline-seconds", type=float)
    parser.add_argument("--poll-interval-seconds", type=float)
    parser.add_argument("--timeout-seconds", type=float)
    parser.add_argument("--poll-concurrency", type=int)
    parser.add_argument("--quality-manifest", help="private manifest; quality mode accepts at most five fixtures")
    parser.add_argument("--control-file", help="new file in an owned 0700 directory; contains only run/client/job IDs")
    parser.add_argument("--report-file", help="new aggregate-only JSON file in an owned 0700 directory")
    parser.add_argument("--prepare-control-ledger", action="store_true",
                        help="create a private control-file header without contacting the API")
    parser.add_argument("--wait-for-observer", action="store_true",
                        help="pause after enrollment and fsyncing client_id, before the first job POST")
    parser.add_argument("--include-field-names", action="store_true",
                        help="quality mode only: include canonical field names in scored categories, never values")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.prepare_control_ledger:
        if not args.control_file:
            print(json.dumps({"status": "failed", "category": "configuration_invalid"}, separators=(",", ":")))
            return 2
        try:
            ControlFile(args.control_file, args.mode)
        except HarnessFailure as exc:
            print(json.dumps({"status": "failed", "category": exc.category}, separators=(",", ":")))
            return 2
        print(json.dumps({"control_ledger": "ready"}, separators=(",", ":")))
        return 0
    try:
        exit_code, report = run(args)
    except HarnessFailure as exc:
        print(json.dumps({"status": "failed", "category": exc.category}, separators=(",", ":")))
        return 2
    except Exception:
        print(json.dumps({"status": "failed", "category": "response_invalid"}, separators=(",", ":")))
        return 2
    report["status"] = "complete" if exit_code == 0 else "partial_or_failed"
    if args.report_file:
        try:
            _write_private_report(args.report_file, report)
        except HarnessFailure as exc:
            print(json.dumps({"status": "failed", "category": exc.category}, separators=(",", ":")))
            return 2
    print(json.dumps(report, sort_keys=True, separators=(",", ":")))
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
