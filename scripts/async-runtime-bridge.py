#!/usr/bin/env python3
"""Operate a detached, private-configured SSH forward for the LAN debug ingress."""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import fcntl
import hashlib
import ipaddress
import json
import os
import re
import shlex
import signal
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any


SCHEMA = "passport-async-bridge/v1"
ALIAS_RE = re.compile(r"^[A-Za-z0-9_.-]+$")
SSH_CONNECT_TIMEOUT_SECONDS = 5
SSH_CONNECTION_ATTEMPTS = 3
START_TIMEOUT_SECONDS = 20


class BridgeError(Exception):
    pass


def run(
    argv: list[str], *, check: bool = False, timeout: float = 3.0
) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(argv, check=False, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise BridgeError("local process inspection timed out") from exc
    except OSError as exc:
        raise BridgeError("required local process utility is unavailable") from exc
    if check and result.returncode != 0:
        raise BridgeError("bridge command failed; verify the private SSH profile")
    return result


def load_config(path: Path) -> dict[str, Any]:
    if not path.is_absolute() or path.is_symlink() or not path.is_file():
        raise BridgeError("private bridge config must be an absolute regular file")
    st = path.stat()
    parent_stat = path.parent.stat()
    if (
        stat.S_IMODE(st.st_mode) != 0o600
        or st.st_uid != os.getuid()
        or path.parent.is_symlink()
        or stat.S_IMODE(parent_stat.st_mode) != 0o700
        or parent_stat.st_uid != os.getuid()
    ):
        raise BridgeError("private bridge config permissions are invalid")
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise BridgeError("private bridge config is unreadable") from exc
    expected = {
        "schema_version",
        "bind_address",
        "bind_port",
        "target_address",
        "target_port",
        "ssh_config",
        "ssh_host_alias",
    }
    if not isinstance(data, dict) or set(data) != expected or data.get("schema_version") != SCHEMA:
        raise BridgeError("private bridge config schema is invalid")
    for key in ("bind_address", "target_address"):
        value = data.get(key)
        if not isinstance(value, str):
            raise BridgeError("bridge address is invalid")
        try:
            address = ipaddress.ip_address(value)
        except ValueError as exc:
            raise BridgeError("bridge address is invalid") from exc
        if address.is_unspecified or address.is_multicast:
            raise BridgeError("wildcard or multicast bridge addresses are forbidden")
    for key in ("bind_port", "target_port"):
        value = data.get(key)
        if type(value) is not int or not 1 <= value <= 65535:
            raise BridgeError("bridge port is invalid")
    ssh_config = data.get("ssh_config")
    if not isinstance(ssh_config, str) or not Path(ssh_config).is_absolute():
        raise BridgeError("SSH config path is invalid")
    ssh_path = Path(ssh_config)
    if ssh_path.is_symlink() or not ssh_path.is_file():
        raise BridgeError("SSH config file is unavailable")
    alias = data.get("ssh_host_alias")
    if not isinstance(alias, str) or alias.startswith("-") or not ALIAS_RE.fullmatch(alias):
        raise BridgeError("SSH host alias is invalid")
    return data


def private_pid_path(config_path: Path) -> Path:
    return config_path.with_name(config_path.name + ".pid.json")


def lstart(pid: int) -> str | None:
    result = run(["ps", "-p", str(pid), "-o", "lstart="])
    return result.stdout.strip() if result.returncode == 0 and result.stdout.strip() else None


def command_info(pid: int) -> tuple[str, list[str]] | None:
    comm = run(["ps", "-p", str(pid), "-o", "comm="])
    args = run(["ps", "-ww", "-p", str(pid), "-o", "args="])
    if comm.returncode != 0 or args.returncode != 0 or not comm.stdout.strip() or not args.stdout.strip():
        return None
    try:
        return Path(comm.stdout.strip()).name, shlex.split(args.stdout.strip())
    except ValueError:
        return None


def listener_pids(bind_address: str, bind_port: int) -> list[int]:
    result = run(["lsof", "-nP", "-t", f"-iTCP@{bind_address}:{bind_port}", "-sTCP:LISTEN"])
    if result.returncode not in (0, 1):
        raise BridgeError("cannot inspect local listening sockets")
    pids: set[int] = set()
    for raw in result.stdout.splitlines():
        try:
            pid = int(raw)
        except ValueError:
            continue
        entries = run(["lsof", "-nP", "-a", "-p", str(pid), f"-iTCP@{bind_address}:{bind_port}", "-sTCP:LISTEN"])
        if entries.returncode == 0 and entries.stdout.strip():
            pids.add(pid)
    return sorted(pids)


def expected_forward(data: dict[str, Any]) -> str:
    return f"{data['bind_address']}:{data['bind_port']}:{data['target_address']}:{data['target_port']}"


def build_ssh_argv(data: dict[str, Any]) -> list[str]:
    return [
        "ssh",
        "-F", data["ssh_config"],
        "-S", "none",
        "-o", "ControlMaster=no",
        "-o", "ControlPath=none",
        "-o", "ExitOnForwardFailure=yes",
        "-o", "ForkAfterAuthentication=no",
        "-o", f"ConnectTimeout={SSH_CONNECT_TIMEOUT_SECONDS}",
        "-o", f"ConnectionAttempts={SSH_CONNECTION_ATTEMPTS}",
        "-o", "ServerAliveInterval=30",
        "-o", "ServerAliveCountMax=3",
        "-o", "BatchMode=yes",
        "-N",
        "-L", expected_forward(data),
        data["ssh_host_alias"],
    ]


def command_digest(argv: list[str]) -> str:
    return hashlib.sha256("\0".join(argv).encode()).hexdigest()


def verify_command(pid: int, saved: dict[str, Any], data: dict[str, Any]) -> bool:
    start = lstart(pid)
    info = command_info(pid)
    if start is None or info is None or start != saved.get("start_time"):
        return False
    executable, argv = info
    if executable != "ssh" or command_digest(argv) != saved.get("command_sha256"):
        return False
    required_pairs = [
        ("-S", "none"),
        ("-F", data["ssh_config"]),
        ("-o", "ControlMaster=no"),
        ("-o", "ControlPath=none"),
        ("-o", "ExitOnForwardFailure=yes"),
        ("-L", expected_forward(data)),
    ]
    for flag, value in required_pairs:
        if not any(argv[i : i + 2] == [flag, value] for i in range(len(argv) - 1)):
            return False
    if not argv or argv[-1] != data["ssh_host_alias"]:
        return False
    if "-N" not in argv:
        return False
    for option, expected in (
        ("ConnectTimeout", str(SSH_CONNECT_TIMEOUT_SECONDS)),
        ("ConnectionAttempts", str(SSH_CONNECTION_ATTEMPTS)),
        ("ForkAfterAuthentication", "no"),
    ):
        values = [argv[index + 1].split("=", 1)[1] for index in range(len(argv) - 1) if argv[index] == "-o" and "=" in argv[index + 1] and argv[index + 1].split("=", 1)[0] == option]
        if values and values != [expected]:
            return False
    return True


def verify_process(pid: int, saved: dict[str, Any], data: dict[str, Any], config_path: Path) -> bool:
    return verify_command(pid, saved, data) and listener_pids(data["bind_address"], data["bind_port"]) == [pid]


def read_pid_record(path: Path) -> dict[str, Any] | None:
    try:
        st = path.lstat()
    except FileNotFoundError:
        return None
    parent_stat = path.parent.stat()
    if (
        stat.S_ISLNK(st.st_mode)
        or not stat.S_ISREG(st.st_mode)
        or stat.S_IMODE(st.st_mode) != 0o600
        or st.st_uid != os.getuid()
        or path.parent.is_symlink()
        or stat.S_IMODE(parent_stat.st_mode) != 0o700
        or parent_stat.st_uid != os.getuid()
    ):
        raise BridgeError("private bridge PID record is unsafe")
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise BridgeError("private bridge PID record is unreadable") from exc
    if not isinstance(data, dict) or type(data.get("pid")) is not int:
        raise BridgeError("private bridge PID record is invalid")
    return data


def write_pid_record(path: Path, data: dict[str, Any]) -> None:
    parent = path.parent
    pst = parent.stat()
    if parent.is_symlink() or not stat.S_ISDIR(pst.st_mode) or stat.S_IMODE(pst.st_mode) != 0o700:
        raise BridgeError("bridge runtime directory must be an owned 0700 directory")
    if pst.st_uid != os.getuid():
        raise BridgeError("bridge runtime directory owner is invalid")
    if path.exists() or path.is_symlink():
        raise BridgeError("bridge PID record already exists")
    fd, temp_name = tempfile.mkstemp(prefix=".bridge-pid-", dir=parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as stream:
            json.dump(data, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        try:
            os.link(temp_name, path, follow_symlinks=False)
        except FileExistsError as exc:
            raise BridgeError("bridge PID record already exists") from exc
        os.unlink(temp_name)
        dirfd = os.open(parent, os.O_RDONLY)
        try:
            os.fsync(dirfd)
        finally:
            os.close(dirfd)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass


@contextlib.contextmanager
def lifecycle_lock(config_path: Path):
    lock_path = config_path.with_name(config_path.name + ".lock")
    flags = os.O_CREAT | os.O_RDWR
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(lock_path, flags, 0o600)
    except OSError as exc:
        raise BridgeError("private bridge lock is unavailable") from exc
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o600:
            raise BridgeError("private bridge lock is unsafe")
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def status(config_path: Path, data: dict[str, Any]) -> int:
    pid_path = private_pid_path(config_path)
    record = read_pid_record(pid_path)
    if record is None:
        print("stopped")
        return 1
    pid = record["pid"]
    if not verify_process(pid, record, data, config_path):
        raise BridgeError("PID record does not match the exact SSH listener; refusing action")
    print(f"running pid={pid} start={record['start_time']} bind={data['bind_address']}:{data['bind_port']} target={data['target_address']}:{data['target_port']}")
    return 0


def start(config_path: Path, data: dict[str, Any]) -> int:
    pid_path = private_pid_path(config_path)
    existing = read_pid_record(pid_path)
    if existing is not None:
        if verify_process(existing["pid"], existing, data, config_path):
            return status(config_path, data)
        if lstart(existing["pid"]) is not None:
            raise BridgeError("PID record points to a different live process; refusing to signal it")
        if listener_pids(data["bind_address"], data["bind_port"]):
            raise BridgeError("target bind address is occupied by an untracked process")
        pid_path.unlink()
    elif listener_pids(data["bind_address"], data["bind_port"]):
        raise BridgeError("target bind address is occupied by an untracked process")

    argv = build_ssh_argv(data)
    try:
        process = subprocess.Popen(
            argv,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
            close_fds=True,
        )
    except OSError as exc:
        raise BridgeError("SSH process could not be started") from exc

    record: dict[str, Any] | None = None
    try:
        deadline = time.monotonic() + START_TIMEOUT_SECONDS
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise BridgeError("SSH forward exited before readiness")
            info = command_info(process.pid)
            started = lstart(process.pid)
            if info is not None and started is not None and info[0] == "ssh":
                record = {
                    "pid": process.pid,
                    "start_time": started,
                    "command_sha256": command_digest(info[1]),
                }
                pids = listener_pids(data["bind_address"], data["bind_port"])
                if pids == [process.pid] and verify_process(process.pid, record, data, config_path):
                    write_pid_record(pid_path, record)
                    return status(config_path, data)
                if pids and pids != [process.pid]:
                    raise BridgeError("target bind address was claimed by an unknown process")
            time.sleep(0.1)
        raise BridgeError("SSH forward did not become ready before the bounded startup deadline")
    except BaseException as original_error:
        if record is None:
            info = command_info(process.pid)
            started = lstart(process.pid)
            if info is not None and started is not None and info[0] == "ssh":
                record = {
                    "pid": process.pid,
                    "start_time": started,
                    "command_sha256": command_digest(info[1]),
                }
        if record is not None:
            cleaned = cleanup_started_process(process.pid, record, data)
            if cleaned:
                try:
                    saved = read_pid_record(pid_path)
                    if saved == record:
                        pid_path.unlink()
                except (BridgeError, OSError):
                    raise BridgeError("startup failed; private PID record cleanup was incomplete") from original_error
            else:
                raise BridgeError("startup failed; exact SSH process could not be safely stopped") from original_error
        raise


def cleanup_started_process(pid: int, record: dict[str, Any], data: dict[str, Any]) -> bool:
    if not verify_command(pid, record, data):
        return False
    listeners = listener_pids(data["bind_address"], data["bind_port"])
    if listeners not in ([], [pid]):
        return False
    if not verify_command(pid, record, data):
        return False
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return True
    deadline = time.monotonic() + 5.0
    while time.monotonic() < deadline:
        if lstart(pid) is None:
            return not listener_pids(data["bind_address"], data["bind_port"])
        time.sleep(0.1)
    # The identity check is intentionally conservative; never escalate to an unknown PID.
    return False


def stop(config_path: Path, data: dict[str, Any]) -> int:
    pid_path = private_pid_path(config_path)
    record = read_pid_record(pid_path)
    if record is None:
        print("stopped")
        return 0
    pid = record["pid"]
    if not verify_process(pid, record, data, config_path):
        raise BridgeError("PID record does not match the exact SSH listener; refusing to signal")
    if not verify_process(pid, record, data, config_path):
        raise BridgeError("SSH process identity changed; refusing to signal")
    os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 5.0
    while time.monotonic() < deadline:
        if lstart(pid) is None:
            break
        time.sleep(0.1)
    if lstart(pid) is not None or listener_pids(data["bind_address"], data["bind_port"]):
        raise BridgeError("SSH forward did not stop; PID record retained")
    pid_path.unlink()
    print("stopped")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("start", "status", "stop"))
    parser.add_argument("config", type=Path, help="operator-owned mode-0600 bridge descriptor")
    args = parser.parse_args()
    try:
        data = load_config(args.config)
        with lifecycle_lock(args.config):
            if args.action == "start":
                return start(args.config, data)
            if args.action == "status":
                return status(args.config, data)
            return stop(args.config, data)
    except BridgeError as exc:
        print(f"async bridge: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
