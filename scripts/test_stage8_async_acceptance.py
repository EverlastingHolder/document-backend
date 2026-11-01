import contextlib
import http.server
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import time
import unittest
import uuid
from unittest import mock

import stage8_async_acceptance as harness


class FakeAsyncAPI(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    request_queue_size = 128
    lock = threading.Lock()
    enrollment_bodies = []
    auth_tokens = set()
    idempotency = {}
    receipt_ids = set()
    redirects = False

    @classmethod
    def reset(cls):
        with cls.lock:
            cls.enrollment_bodies = []
            cls.auth_tokens = set()
            cls.idempotency = {}
            cls.receipt_ids = set()
            cls.redirects = False

    def log_message(self, _format, *_args):
        return

    def send_json(self, status, value):
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.path == "/v1/client-installations":
            with self.lock:
                self.enrollment_bodies.append(body)
            self.send_json(201, {"client_id": str(uuid.uuid4()), "created_at": "2026-11-01T00:00:00Z"})
            return
        if self.path == "/v1/passport/jobs":
            if self.redirects:
                self.send_response(307)
                self.send_header("Location", "http://redirect.invalid/v1/passport/jobs")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            token = self.headers.get("Authorization", "")
            key = self.headers.get("Idempotency-Key", "")
            with self.lock:
                self.auth_tokens.add(token)
                job_id = self.idempotency.setdefault(key, str(uuid.uuid4()))
            self.send_json(202, {"job_id": job_id, "status": "queued",
                                 "status_url": f"/v1/passport/jobs/{job_id}",
                                 "created_at": "2026-11-01T00:00:00Z"})
            return
        if self.path.endswith("/receipt"):
            job_id = self.path.split("/")[-2]
            with self.lock:
                self.receipt_ids.add(job_id)
            self.send_response(204)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_json(404, {"code": "not_found"})

    def do_GET(self):
        job_id = self.path.rsplit("/", 1)[-1]
        with self.lock:
            gone = job_id in self.receipt_ids
        if gone:
            self.send_json(410, {"code": "job_result_gone"})
            return
        result = {
            "request_id": job_id,
            "status": "completed",
            "review_required": True,
            "pipeline_version": "synthetic-pipeline",
            "fields": {name: {"value": "PRIVATE-VALUE", "state": "recognized", "issues": []}
                       for name in harness.FIELDS},
            "warnings": [],
        }
        self.send_json(200, {"job_id": job_id, "status": "completed",
                             "created_at": "2026-11-01T00:00:00Z",
                             "updated_at": "2026-11-01T00:00:00Z",
                             "started_at": "2026-11-01T00:00:01Z",
                             "finished_at": "2026-11-01T00:00:02Z",
                             "result": result})


class Stage8AcceptanceTests(unittest.TestCase):
    def setUp(self):
        FakeAsyncAPI.reset()
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeAsyncAPI)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.image = self.root / "private-image.bin"
        self.image.write_bytes(b"synthetic-image-bytes")
        self.image.chmod(0o600)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def args(self, **overrides):
        base = {
            "base_url": f"http://127.0.0.1:{self.server.server_port}",
            "mode": "burst", "image": str(self.image), "media_type": "image/png",
            "max_image_bytes": 1024, "deadline_seconds": 20,
            "poll_interval_seconds": 0.01, "timeout_seconds": 2,
            "poll_concurrency": 20, "quality_manifest": None,
            "control_file": str(self.root / "control.jsonl"), "wait_for_observer": False,
            "allow_private_http": False, "report_file": None, "include_field_names": False,
        }
        base.update(overrides)
        return type("Args", (), base)()

    def test_endpoint_refuses_remote_cleartext_and_non_origin_urls(self):
        for url in ("http://example.invalid", "https://example.invalid/base", "https://user@example.invalid"):
            with self.subTest(url=url), self.assertRaises(harness.HarnessFailure) as caught:
                harness.Endpoint.parse(url)
            self.assertEqual(str(caught.exception), "configuration_invalid")
        endpoint = harness.Endpoint.parse("http://192.168.1.91:8080", allow_private_http=True)
        self.assertEqual(endpoint.host, "192.168.1.91")

    def test_manifest_requires_private_opaque_alias_and_scored_fields(self):
        expected = {name: {"value": "PRIVATE-VALUE", "state": "recognized"} for name in harness.FIELDS}
        manifest_path = self.root / "manifest.json"
        manifest_path.write_text(json.dumps({"fixtures": [{
            "alias": "opaque-fixture-01", "document_group": "group-01",
            "image": str(self.image), "media_type": "image/png",
            "expected_fields": expected, "scored_fields": ["surname", "series"],
        }]}))
        manifest_path.chmod(0o600)
        fixtures = harness._read_manifest(manifest_path)
        self.assertEqual(fixtures[0]["alias"], "opaque-fixture-01")
        self.assertEqual(fixtures[0]["scored_fields"], ["surname", "series"])
        self.assertEqual(fixtures[0]["document_group"], "group-01")

    def test_result_validation_and_scoring_keep_values_out_of_counts(self):
        job_id = str(uuid.uuid4())
        result = {"request_id": job_id, "status": "completed", "review_required": True,
                  "pipeline_version": "synthetic-pipeline",
                  "fields": {name: {"value": "PRIVATE-VALUE", "state": "recognized", "issues": []}
                             for name in harness.FIELDS}, "warnings": []}
        scored = {name: {"value": "PRIVATE-VALUE", "state": "recognized"} for name in harness.FIELDS}
        counts = harness._quality(result, scored, ["surname", "series"])
        self.assertEqual(counts, {"matched_fields": 2, "mismatched_fields": 0, "missing_fields": 0,
                                  "unscored_fields": 9, "issues": 0})
        no_gold = harness._quality(result, scored, [])
        self.assertEqual(no_gold, {"matched_fields": 0, "mismatched_fields": 0, "missing_fields": 0,
                                   "unscored_fields": 11, "issues": 0})
        expected_with_mismatch = dict(scored)
        expected_with_mismatch["series"] = {"value": "OTHER-VALUE", "state": "recognized"}
        diagnostic = harness._quality(result, expected_with_mismatch, ["surname", "series"], True)
        self.assertEqual(diagnostic["matched_field_names"], ["surname"])
        self.assertEqual(diagnostic["mismatched_field_names"], ["series"])
        self.assertEqual(diagnostic["missing_field_names"], [])
        self.assertNotIn("PRIVATE-VALUE", json.dumps(diagnostic))
        self.assertNotIn("OTHER-VALUE", json.dumps(diagnostic))
        bad = dict(result)
        bad["fields"] = dict(result["fields"])
        del bad["fields"]["issuer"]
        with self.assertRaises(harness.HarnessFailure) as caught:
            harness.validate_result(bad)
        self.assertEqual(str(caught.exception), "response_invalid")

    def test_control_file_is_private_and_contains_no_credential_or_result(self):
        control_path = self.root / "control.jsonl"
        control = harness.ControlFile(str(control_path), "quality")
        token = harness._token()
        control.record(client_id=str(uuid.uuid4()))
        control.record(job_id=str(uuid.uuid4()))
        control.finish("complete")
        self.assertEqual(control_path.stat().st_mode & 0o777, 0o600)
        text = control_path.read_text()
        self.assertNotIn(token, text)
        self.assertNotIn("PRIVATE-VALUE", text)
        self.assertIn('"job_id"', text)

    def test_control_ledger_can_be_prepared_before_observer_starts(self):
        control_path = self.root / "observer-control.jsonl"
        initial = harness.ControlFile(str(control_path), "quality")
        reopened = harness.ControlFile(str(control_path), "quality")
        self.assertEqual(initial.run_id, reopened.run_id)
        reopened.record(client_id=str(uuid.uuid4()))
        self.assertEqual(len(control_path.read_text().splitlines()), 3)
        with self.assertRaises(harness.HarnessFailure):
            harness.ControlFile(str(control_path), "burst")

    def test_private_aggregate_report_is_new_0600_file_without_ids_or_values(self):
        report_path = self.root / "aggregate.json"
        harness._write_private_report(str(report_path), {
            "accepted": 1, "terminal_outcomes": {"completed": 1}, "post_latency": {"p50_ms": 10.0},
        })
        self.assertEqual(report_path.stat().st_mode & 0o777, 0o600)
        contents = report_path.read_text()
        self.assertNotIn("job_id", contents)
        self.assertNotIn("client_id", contents)
        self.assertNotIn("token", contents)
        self.assertNotIn("OCR-VALUE", contents)

    def test_enrollment_posts_only_hash_and_returns_secret_only_in_process(self):
        endpoint = harness.Endpoint.parse(f"http://127.0.0.1:{self.server.server_port}")
        token, client_id = harness.enroll(endpoint, 2)
        self.assertTrue(token.startswith("pbt1_"))
        self.assertEqual(uuid.UUID(client_id).version, 4)
        payload = json.loads(FakeAsyncAPI.enrollment_bodies[0])
        self.assertEqual(set(payload), {"token_sha256"})
        self.assertEqual(len(payload["token_sha256"]), 64)
        self.assertNotIn(token.encode(), FakeAsyncAPI.enrollment_bodies[0])

    def test_hundred_concurrent_posts_drain_and_receipt_without_outputting_values_or_ids(self):
        args = self.args()
        active = 0
        maximum_active = 0
        keys = set()
        lock = threading.Lock()
        jobs = {}
        post_barrier = threading.Barrier(100)

        def fake_post(_endpoint, _token, _image, _mime, key, _timeout, _deadline):
            nonlocal active, maximum_active
            with lock:
                active += 1
                maximum_active = max(maximum_active, active)
                keys.add(key)
            post_barrier.wait()
            time.sleep(0.03)
            job_id = str(uuid.uuid4())
            jobs[job_id] = key
            with lock:
                active -= 1
            return 202, job_id, 0.03, [202]

        def fake_poll(_endpoint, _token, job_id, _accepted_at, _deadline, _timeout, _interval):
            return {"state": "completed", "job_id": job_id, "queue_ms": 35.0, "terminal": True}

        with (mock.patch.object(harness, "enroll", return_value=("pbt1_PRIVATE_ONLY_IN_TEST", str(uuid.uuid4()))),
              mock.patch.object(harness, "_post_image", side_effect=fake_post),
              mock.patch.object(harness, "_poll_one", side_effect=fake_poll),
              mock.patch.object(harness, "_receipt", return_value=True),
              contextlib.redirect_stderr(io.StringIO())):
            exit_code, report = harness.run(args)
        self.assertEqual(exit_code, 0, report)
        self.assertEqual(maximum_active, 100)
        self.assertEqual(len(keys), 100)
        self.assertEqual(report["accepted"], 100)
        self.assertEqual(report["terminal_outcomes"], {"completed": 100})
        self.assertEqual(report["receipt_204_and_get_410"], 100)
        self.assertEqual(report["post_latency"]["p50_ms"], 30.0)
        serialized = json.dumps(report)
        for secret in ("PRIVATE-VALUE", "job_id", "client_id", str(self.image), "pbt1_PRIVATE_ONLY_IN_TEST"):
            self.assertNotIn(secret, serialized)
        ledger = (self.root / "control.jsonl").read_text()
        self.assertEqual(len([line for line in ledger.splitlines() if '"job_id"' in line]), 100)
        self.assertEqual((self.root / "control.jsonl").stat().st_mode & 0o777, 0o600)

    def test_http_post_and_receipt_cycle_does_not_follow_redirects_or_echo_result(self):
        endpoint = harness.Endpoint.parse(f"http://127.0.0.1:{self.server.server_port}")
        token, _ = harness.enroll(endpoint, 2)
        status, job_id, _, _ = harness._post_image(endpoint, token, b"synthetic", "image/png",
                                                    "a" * 32, 2, time.monotonic() + 5)
        self.assertEqual(status, 202)
        polled, payload = harness._status(endpoint, token, job_id, 2)
        self.assertEqual(polled, 200)
        checked = harness.validate_result(payload["result"])
        self.assertEqual(set(checked["fields"]), set(harness.FIELDS))
        self.assertTrue(harness._receipt(endpoint, token, job_id, 2))
        self.assertNotIn(token, json.dumps(payload))

    def test_quality_mode_scores_only_reviewed_fields_and_reports_opaque_alias_counts(self):
        expected = {name: {"value": "PRIVATE-VALUE", "state": "recognized"} for name in harness.FIELDS}
        manifest_path = self.root / "quality-manifest.json"
        manifest_path.write_text(json.dumps({"fixtures": [{
            "alias": "opaque-fixture-a", "document_group": "opaque-group-a",
            "image": str(self.image), "media_type": "image/png",
            "expected_fields": expected, "scored_fields": ["surname", "series"],
        }]}))
        manifest_path.chmod(0o600)
        args = self.args(mode="quality", image=None, media_type=None,
                         quality_manifest=str(manifest_path), control_file=str(self.root / "quality-control.jsonl"))
        with contextlib.redirect_stderr(io.StringIO()):
            exit_code, report = harness.run(args)
        self.assertEqual(exit_code, 0, report)
        self.assertEqual(report["unique_documents"], 1)
        self.assertEqual(report["fixtures"][0]["quality"], {
            "matched_fields": 2, "mismatched_fields": 0, "missing_fields": 0,
            "unscored_fields": 9, "issues": 0,
        })
        serialized = json.dumps(report)
        self.assertNotIn("PRIVATE-VALUE", serialized)
        self.assertNotIn("document_group", serialized)

    def test_post_redirect_is_not_followed(self):
        FakeAsyncAPI.redirects = True
        endpoint = harness.Endpoint.parse(f"http://127.0.0.1:{self.server.server_port}")
        status, job_id, _, statuses = harness._post_image(endpoint, "pbt1_test", b"body", "image/png",
                                                          "a" * 32, 2, time.monotonic() + 5)
        self.assertEqual(status, 307)
        self.assertIsNone(job_id)
        self.assertEqual(statuses, [307])
        self.assertEqual(FakeAsyncAPI.idempotency, {})


if __name__ == "__main__":
    unittest.main()
