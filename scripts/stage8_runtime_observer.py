#!/usr/bin/env python3
"""Read-only, aggregate-only sampler for an isolated Stage 8 OCR run.

The private JSONL control file contains run_id, client_id, and accepted job_id
UUID records. This tool never prints IDs, reads image/result data, or calls a
mutating endpoint. Redirect output only into an approved private log.
"""
from __future__ import annotations
import argparse
import datetime as dt
import json
import os
import pathlib
import shutil
import stat
import subprocess
import sys
import time
import uuid

SERVICES = ("api", "postgres", "rabbitmq", "ocr", "lan-debug-caddy")
PROJECT_LABEL = "com.docker.compose.project"
SERVICE_LABEL = "com.docker.compose.service"
QUEUES = {"passport.recognition.work", "passport.recognition.dead"}

class ObserverFailure(Exception):
    pass

def run(args, *, input_text=None, timeout=8):
    try:
        p = subprocess.run(args, input=input_text, capture_output=True, text=True,
                           timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ObserverFailure("command_unavailable") from None
    if p.returncode:
        raise ObserverFailure("command_failed")
    return p.stdout

def read_control(path):
    try:
        parent = path.parent.lstat()
        if (not stat.S_ISDIR(parent.st_mode) or stat.S_ISLNK(parent.st_mode)
                or parent.st_uid != os.getuid() or stat.S_IMODE(parent.st_mode) != 0o700):
            raise ObserverFailure("control_parent_permissions")
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            st = os.fstat(fd)
            if (not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid()
                    or stat.S_IMODE(st.st_mode) != 0o600 or st.st_nlink != 1
                    or st.st_size > 1024 * 1024):
                raise ObserverFailure("control_file_permissions")
            raw = os.read(fd, 1024 * 1024 + 1)
        finally:
            os.close(fd)
        lines = raw.splitlines(keepends=True)
        records = [json.loads(line) for line in lines if line.endswith(b"\n")]
        if (len(records) < 3 or not all(isinstance(row, dict) for row in records)
                or set(records[0]) != {"run_id"} or set(records[1]) != {"mode"}
                or records[1]["mode"] not in {"quality", "burst"}
                or set(records[2]) != {"client_id"}):
            raise ObserverFailure("control_schema_invalid")
        uuid.UUID(records[0]["run_id"])
        mode = records[1]["mode"]
        owner = str(uuid.UUID(records[2]["client_id"]))
        jobs = set()
        complete = "in_progress"
        for row in records[3:]:
            if not isinstance(row, dict):
                raise ObserverFailure("control_schema_invalid")
            if set(row) == {"status"} and row["status"] in {"complete", "partial"}:
                complete = row["status"]
                continue
            if set(row) != {"job_id"}:
                raise ObserverFailure("control_schema_invalid")
            jobs.add(str(uuid.UUID(row["job_id"])))
        return owner, len(jobs), mode, complete
    except ObserverFailure:
        raise
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError):
        raise ObserverFailure("control_read_failed") from None

def get_containers(context, project):
    ids = run(["docker", "--context", context, "ps", "-aq", "--filter",
               f"label={PROJECT_LABEL}={project}"]).split()
    if not ids:
        raise ObserverFailure("project_containers_missing")
    try:
        rows = json.loads(run(["docker", "--context", context, "inspect", *ids]))
        found = {}
        for row in rows:
            labels = row.get("Config", {}).get("Labels") or {}
            service = labels.get(SERVICE_LABEL)
            if labels.get(PROJECT_LABEL) != project or service not in SERVICES:
                continue
            state = row.get("State", {})
            found[service] = {
                "status": state.get("Health", {}).get("Status", state.get("Status", "unknown")),
                "restarts": int(row.get("RestartCount", 0)),
                "id": row.get("Id", ""), "name": row.get("Name", "").lstrip("/"),
            }
        if not found:
            raise ObserverFailure("container_metadata_missing")
        return found
    except (ValueError, TypeError, KeyError):
        raise ObserverFailure("container_metadata_invalid") from None

def resource_stats(context, found):
    ids = [v["id"] for v in found.values() if v["status"] in {"running", "healthy"}]
    if not ids:
        return {}
    out = run(["docker", "--context", context, "stats", "--no-stream", "--format",
               "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}", *ids])
    service_for_name = {v["name"]: k for k, v in found.items()}
    stats = {}
    for line in out.splitlines():
        fields = line.split("|")
        if len(fields) == 4 and fields[0] in service_for_name:
            stats[service_for_name[fields[0]]] = {
                "cpu": fields[1], "memory": fields[2], "memory_percent": fields[3]}
    return stats

def queue_snapshot(context, rabbit_id):
    out = run(["docker", "--context", context, "exec", rabbit_id, "rabbitmqctl",
               "list_queues", "name", "messages_ready", "messages_unacknowledged", "consumers"], timeout=12)
    result = {}
    try:
        for line in out.splitlines():
            fields = line.split("\t")
            if len(fields) == 4 and fields[0] in QUEUES:
                result[fields[0]] = {"ready": int(fields[1]), "unacknowledged": int(fields[2]),
                                    "consumers": int(fields[3])}
    except ValueError:
        raise ObserverFailure("queue_snapshot_invalid") from None
    if set(result) != QUEUES:
        raise ObserverFailure("queue_snapshot_incomplete")
    return result

def database_snapshot(context, pg_id, owner):
    # Owner UUID is validated from the private ledger; SQL output is aggregate-only.
    sql = f"""
WITH now_value AS MATERIALIZED (SELECT clock_timestamp() AS at),
owner_jobs AS MATERIALIZED (
 SELECT job_id,state,created_at,finished_at,result,image_purged_at
 FROM jobs WHERE owner_id='{owner}'
),
owner_attempts AS MATERIALIZED (
 SELECT a.attempt_no,a.state,a.started_at,a.finished_at,a.lease_expires_at,a.error_code
 FROM job_attempts a JOIN owner_jobs j USING (job_id)
),
events AS (
 SELECT started_at AS at,1 AS delta FROM owner_attempts WHERE started_at IS NOT NULL
 UNION ALL SELECT COALESCE(finished_at,(SELECT at FROM now_value)),-1
 FROM owner_attempts WHERE started_at IS NOT NULL
),
overlap AS (
 SELECT SUM(delta) OVER (ORDER BY at,delta ROWS UNBOUNDED PRECEDING) AS active FROM events
)
SELECT jsonb_build_object(
 'jobs_total',(SELECT count(*) FROM owner_jobs),
 'queued',(SELECT count(*) FROM owner_jobs WHERE state='queued'),
 'processing',(SELECT count(*) FROM owner_jobs WHERE state='processing'),
 'completed',(SELECT count(*) FROM owner_jobs WHERE state='completed'),
 'failed',(SELECT count(*) FROM owner_jobs WHERE state='failed'),
 'attempts_total',(SELECT count(*) FROM owner_attempts),
 'retry_attempts',(SELECT count(*) FROM owner_attempts WHERE attempt_no>1),
 'expired_processing_attempts',(SELECT count(*) FROM owner_attempts,now_value
  WHERE state='processing' AND lease_expires_at<now_value.at),
 'processing_timeout_attempts',(SELECT count(*) FROM owner_attempts WHERE error_code='processing_timeout'),
 'terminal_images_not_purged',(SELECT count(*) FROM owner_jobs
  WHERE state IN ('completed','failed') AND image_purged_at IS NULL),
 'terminal_results_retained',(SELECT count(*) FROM owner_jobs
  WHERE state IN ('completed','failed') AND result IS NOT NULL),
 'unpublished_outbox',(SELECT count(*) FROM outbox o JOIN owner_jobs j USING (job_id)
  WHERE o.published_at IS NULL),
 'max_overlapping_attempt_intervals',(SELECT COALESCE(max(active),0) FROM overlap),
 'attempt_duration_p50_seconds',(SELECT percentile_cont(0.50) WITHIN GROUP
  (ORDER BY extract(epoch FROM finished_at-started_at)) FROM owner_attempts WHERE finished_at IS NOT NULL),
 'attempt_duration_p95_seconds',(SELECT percentile_cont(0.95) WITHIN GROUP
  (ORDER BY extract(epoch FROM finished_at-started_at)) FROM owner_attempts WHERE finished_at IS NOT NULL),
 'terminal_latency_p50_seconds',(SELECT percentile_cont(0.50) WITHIN GROUP
  (ORDER BY extract(epoch FROM finished_at-created_at)) FROM owner_jobs WHERE finished_at IS NOT NULL),
 'terminal_latency_p95_seconds',(SELECT percentile_cont(0.95) WITHIN GROUP
  (ORDER BY extract(epoch FROM finished_at-created_at)) FROM owner_jobs WHERE finished_at IS NOT NULL)
)::text;
"""
    out = run(["docker", "--context", context, "exec", "-i", pg_id, "psql", "-X", "-qAt",
               "-v", "ON_ERROR_STOP=1", "-U", "passport", "-d", "passport"], input_text=sql)
    try:
        value = json.loads(out.strip())
        if not isinstance(value, dict):
            raise ValueError
        return value
    except (ValueError, json.JSONDecodeError):
        raise ObserverFailure("database_aggregate_invalid") from None

def image_usage(root):
    try:
        st = root.lstat()
        if not stat.S_ISDIR(st.st_mode) or stat.S_ISLNK(st.st_mode):
            raise ObserverFailure("image_root_invalid")
        if st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o700:
            raise ObserverFailure("image_root_permissions")
        total = files = temp = symlinks = 0
        pending = [root]
        while pending:
            current = pending.pop()
            with os.scandir(current) as entries:
                for entry in entries:
                    item = entry.stat(follow_symlinks=False)
                    if stat.S_ISLNK(item.st_mode):
                        symlinks += 1
                    elif stat.S_ISDIR(item.st_mode):
                        pending.append(pathlib.Path(entry.path))
                    elif stat.S_ISREG(item.st_mode):
                        total += item.st_size
                        files += 1
                        temp += int(entry.name.startswith(".upload-"))
        return {"regular_files": files, "logical_bytes": total,
                "upload_temporaries": temp, "symlinks": symlinks}
    except ObserverFailure:
        raise
    except OSError:
        raise ObserverFailure("image_usage_unavailable") from None

def disk_snapshot(root, profile):
    host_free = shutil.disk_usage(root).free
    out = run(["colima", "ssh", "--profile", profile, "--", "df", "-Pk", "/"], timeout=8)
    vm_free = None
    for line in out.splitlines()[1:]:
        fields = line.split()
        if len(fields) >= 6 and fields[3].isdigit():
            vm_free = int(fields[3]) * 1024
    if vm_free is None:
        raise ObserverFailure("vm_disk_snapshot_invalid")
    return {"host_free_bytes": host_free, "colima_root_free_bytes": vm_free}

def make_sample(args):
    owner, ledger_jobs, mode, ledger_status = read_control(args.control_file)
    found = get_containers(args.context, args.project)
    if "postgres" not in found or "rabbitmq" not in found:
        raise ObserverFailure("database_or_broker_missing")
    return {
        "sampled_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "run_mode": mode,
        "ledger_status": ledger_status,
        "attempt_overlap_scope": "database_attempt_intervals_not_model_call_counter",
        "ledger_accepted_job_count": ledger_jobs,
        "containers": {k: {"status": v["status"], "restarts": v["restarts"]} for k, v in found.items()},
        "missing_services": sorted(set(SERVICES) - set(found)),
        "container_resources": resource_stats(args.context, found),
        "rabbit_queues": queue_snapshot(args.context, found["rabbitmq"]["id"]),
        "test_owner_jobs": database_snapshot(args.context, found["postgres"]["id"], owner),
        "image_store": image_usage(args.image_root),
        "disk": disk_snapshot(args.image_root, args.vm_profile),
    }

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--control-file", required=True, type=pathlib.Path)
    parser.add_argument("--image-root", required=True, type=pathlib.Path)
    parser.add_argument("--context", default="colima-passport-stage7-deploy-20261101")
    parser.add_argument("--vm-profile", default="passport-stage7-deploy-20261101")
    parser.add_argument("--project", default="passport-stage7-candidate-20261101")
    parser.add_argument("--interval-seconds", type=float, default=2.0)
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    if args.interval_seconds <= 0:
        print('{"observer_error":"invalid_interval"}', file=sys.stderr)
        return 2
    try:
        while True:
            print(json.dumps(make_sample(args), sort_keys=True, separators=(",", ":")), flush=True)
            if args.once:
                return 0
            time.sleep(args.interval_seconds)
    except KeyboardInterrupt:
        return 0
    except ObserverFailure as exc:
        print(json.dumps({"observer_error": str(exc)}), file=sys.stderr)
        return 2

if __name__ == "__main__":
    raise SystemExit(main())
