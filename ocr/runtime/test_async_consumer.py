from __future__ import annotations

import json
import contextlib
import hashlib
import io
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
import warnings
from types import SimpleNamespace
from unittest import mock
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from ocr.runtime.async_consumer import (
    Config,
    ConsumerError,
    DeliveryProcessor,
    Envelope,
    PipelineSupervisor,
    RabbitWorker,
    GoWorkerClient,
    ReadinessMarker,
    _validate_execution,
    main,
    parse_envelope,
    readiness_healthy,
    strict_json_object,
)
from ocr.runtime.russiandocs_worker import FIELDS, SCHEMA, validate_pipeline_snapshot
from ocr.runtime import secret_bootstrap


def uid() -> str:
    return str(uuid.uuid4())


def config_for(root: Path) -> Config:
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    image_root = root / "images"
    repo = root / "repo"
    models = root / "models"
    work = root / "work"
    for path in (image_root, repo, models, work):
        path.mkdir(mode=0o700)
    password = root / "rabbit.secret"
    worker = root / "worker.secret"
    password.write_text("rabbit-secret\n", encoding="ascii")
    worker.write_text("pwt1_" + "A" * 43 + "\n", encoding="ascii")
    password.chmod(0o600)
    worker.chmod(0o600)
    return Config(
        rabbit_host="rabbitmq", rabbit_port=5672, rabbit_user="passport",
        rabbit_password_file=password, rabbit_vhost="/", queue="passport.recognition.work",
        exchange="passport.jobs", routing_key="recognition.requested",
        dead_letter_exchange="passport.jobs.dlx", dead_letter_queue="passport.recognition.dead",
        go_worker_url="http://api:8081", worker_token_file=worker,
        readiness_marker=root / "ready", image_root=image_root,
        russiandocs_repo=repo, models_root=models, work_root=work, max_input_bytes=1024,
        max_image_pixels=100000,
        expected_upstream_version="4.5.3", expected_model_lock_version="v2",
        startup_timeout_seconds=2, processing_stop_grace_seconds=0.1,
        http_timeout_ms=500, boundary_attempts=2, reconnect_backoff_seconds=0.1,
        wait_poll_max_seconds=0.1, rabbit_heartbeat_seconds=60,
        rabbit_blocked_connection_timeout_seconds=5, consumer_ack_timeout_ms=600000,
    )


def valid_body(**overrides) -> bytes:
    value = {"schema_version": 1, "event_id": uid(), "job_id": uid(), "attempt_no": 1}
    value.update(overrides)
    return json.dumps(value).encode("utf-8")


class EnvelopeTests(unittest.TestCase):
    def test_accepts_exact_v1_envelope(self) -> None:
        envelope, error = parse_envelope(valid_body())
        self.assertIsNone(error)
        self.assertIsInstance(envelope, Envelope)
        self.assertEqual(envelope.schema_version, 1)

    def test_rejects_boolean_integer_duplicate_and_unknown_fields(self) -> None:
        for body in (
            valid_body(schema_version=True),
            valid_body(attempt_no=True),
            valid_body(extra="not-allowed"),
            b'{"schema_version":1,"schema_version":1,"event_id":"' + uid().encode() +
            b'","job_id":"' + uid().encode() + b'","attempt_no":1}',
        ):
            with self.subTest(body=body):
                self.assertEqual(parse_envelope(body), (None, "malformed_envelope"))

    def test_unsupported_wire_version_preserves_only_valid_identity_for_go_rejection(self) -> None:
        envelope, error = parse_envelope(valid_body(schema_version=2))
        self.assertIsNotNone(envelope)
        self.assertEqual(error, "unsupported_envelope")

    def test_invalid_id_and_oversized_body_are_dead_letter_candidates(self) -> None:
        self.assertEqual(parse_envelope(valid_body(job_id="../not-an-id")), (None, "malformed_envelope"))
        self.assertEqual(parse_envelope(b" " * 20000), (None, "malformed_envelope"))


class ConfigCheckTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.config = config_for(self.root / "runtime")
        package = self.config.russiandocs_repo / "document_processing"
        package.mkdir()
        (package / "__init__.py").write_text('__version__ = "4.5.3"\n', encoding="utf-8")
        asset = self.config.models_root / "weights.bin"
        asset.write_bytes(b"locked model fixture")
        lock = {
            "models_version": "v2",
            "files": [{"path": "weights.bin", "size": asset.stat().st_size,
                       "sha256": hashlib.sha256(asset.read_bytes()).hexdigest()}],
        }
        (package / "models.lock.json").write_text(json.dumps(lock), encoding="utf-8")

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_snapshot_check_validates_source_and_locked_model_bytes(self) -> None:
        validate_pipeline_snapshot(self.config.russiandocs_repo, self.config.models_root, "4.5.3", "v2")

    def test_snapshot_check_rejects_model_tampering_and_wrong_versions(self) -> None:
        with self.assertRaises(RuntimeError):
            validate_pipeline_snapshot(self.config.russiandocs_repo, self.config.models_root, "other", "v2")
        with self.assertRaises(RuntimeError):
            validate_pipeline_snapshot(self.config.russiandocs_repo, self.config.models_root, "4.5.3", "other")
        (self.config.models_root / "weights.bin").write_bytes(b"changed")
        with self.assertRaises(RuntimeError):
            validate_pipeline_snapshot(self.config.russiandocs_repo, self.config.models_root, "4.5.3", "v2")

    def test_check_config_exits_before_run_or_pipeline_import(self) -> None:
        config_path = self.root / "worker.json"
        config_path.write_text(json.dumps({
            name: str(value) if isinstance(value, Path) else value
            for name, value in self.config.__dict__.items()
        }), encoding="utf-8")
        config_path.chmod(0o600)
        output = io.StringIO()
        with contextlib.redirect_stdout(output), \
             mock.patch("ocr.runtime.async_consumer.run", side_effect=AssertionError("runtime started")):
            self.assertEqual(main(["--config", str(config_path), "--check-config"]), 0)
        self.assertEqual(output.getvalue(), "configuration_valid\n")

    def test_check_config_failure_is_fixed_and_does_not_echo_source_path(self) -> None:
        config_path = self.root / "worker.json"
        config_path.write_text(json.dumps({
            name: str(value) if isinstance(value, Path) else value
            for name, value in self.config.__dict__.items()
        }), encoding="utf-8")
        config_path.chmod(0o600)
        (self.config.models_root / "weights.bin").write_bytes(b"tampered")
        output = io.StringIO()
        errors = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            self.assertEqual(main(["--config", str(config_path), "--check-config"]), 2)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(errors.getvalue(), "worker_configuration_invalid\n")
        self.assertNotIn(str(self.root), errors.getvalue())

    def test_check_config_rejects_invalid_worker_secret_without_echoing_it(self) -> None:
        config_path = self.root / "worker.json"
        config_path.write_text(json.dumps({
            name: str(value) if isinstance(value, Path) else value
            for name, value in self.config.__dict__.items()
        }), encoding="utf-8")
        config_path.chmod(0o600)
        secret_value = "not-a-worker-secret"
        self.config.worker_token_file.write_text(secret_value, encoding="ascii")
        output = io.StringIO()
        errors = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            self.assertEqual(main(["--config", str(config_path), "--check-config"]), 2)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(errors.getvalue(), "worker_configuration_invalid\n")
        self.assertNotIn(secret_value, errors.getvalue())

    def test_bootstrap_forwards_check_flag_after_privilege_drop(self) -> None:
        with mock.patch.object(secret_bootstrap.sys, "argv", ["secret_bootstrap.py", "/input/config.json", "--check-config"]), \
             mock.patch.object(secret_bootstrap, "prepare", return_value=Path("/run/passport-ocr/config.json")), \
             mock.patch.object(secret_bootstrap.os, "setgroups"), \
             mock.patch.object(secret_bootstrap.os, "setgid"), \
             mock.patch.object(secret_bootstrap.os, "setuid"), \
             mock.patch.object(secret_bootstrap.os, "execv") as execv:
            self.assertEqual(secret_bootstrap.main(), 0)
        argv = execv.call_args.args[1]
        self.assertEqual(argv[-1], "--check-config")
        self.assertIn("--config", argv)

    def test_strict_json_rejects_duplicates_and_non_objects(self) -> None:
        with self.assertRaises(ValueError):
            strict_json_object(b'{"x":1,"x":2}')
        with self.assertRaises(ValueError):
            strict_json_object(b"[]")


class DeliveryProcessorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.config = config_for(self.root)
        self.envelope = Envelope(1, uid(), uid(), 1)
        self.image_key = uid()
        self.image_path = self.config.image_root / self.image_key
        self.image_path.write_bytes(b"image bytes")
        self.image_path.chmod(0o600)
        lease = datetime.now(timezone.utc) + timedelta(seconds=300)
        deadline = datetime.now(timezone.utc) + timedelta(seconds=60)
        self.execution = {
            "attempt_token": uid(),
            "image_key": self.image_key,
            "media_type": "image/jpeg",
            "size_bytes": len(b"image bytes"),
            "attempt_no": 1,
            "lease_expires_at": lease.isoformat(),
            "pipeline_version": "russiandocs-release-4.6.0-models-prod1",
            "hard_processing_timeout_ms": 60000,
            "completion_budget_ms": 100000,
            "deadline_at": deadline.isoformat(),
        }
        self.sleep_calls: list[float] = []

    def tearDown(self) -> None:
        self.temp.cleanup()

    def processor(self, client, pipeline):
        return DeliveryProcessor(self.config, client, pipeline, self.sleep_calls.append)

    def test_ack_only_after_go_claim_confirms_terminal_state(self) -> None:
        class Client:
            def claim(inner, envelope):
                return {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                raise AssertionError("terminal duplicate must not run OCR")

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))

    def test_claim_wait_is_delayed_then_reclaimed_without_nack_loop(self) -> None:
        class Client:
            calls = 0

            def claim(inner, envelope):
                inner.calls += 1
                return {"decision": "wait", "retry_after_ms": 250} if inner.calls == 1 else {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                raise AssertionError("wait must not run OCR")

        client = Client()
        self.assertTrue(self.processor(client, Pipeline()).process(self.envelope))
        self.assertEqual(client.calls, 2)
        self.assertEqual(self.sleep_calls, [0.1])

    def test_reject_wait_retries_claim_boundary_without_immediate_requeue(self) -> None:
        class Client:
            calls = 0

            def reject(inner, envelope, code):
                inner.calls += 1
                return {"decision": "wait", "retry_after_ms": 250} if inner.calls == 1 else {"decision": "ack"}

        client = Client()
        self.assertTrue(self.processor(client, object()).reject(self.envelope, "unsupported_envelope"))
        self.assertEqual(client.calls, 2)
        self.assertEqual(self.sleep_calls, [0.1])

    def test_completion_result_is_posted_before_delivery_can_ack(self) -> None:
        events: list[str] = []

        class Client:
            def claim(inner, envelope):
                events.append("claim")
                return {"decision": "execute", "execution": dict(self.execution)}

            def complete(inner, envelope, execution, fields, completion_deadline):
                events.append("complete")
                self.assertEqual(set(fields), set(FIELDS))
                return {"decision": "ack"}

        class Pipeline:
            def recognize(inner, request_id, image_id, image_path, media_type, timeout):
                events.append("ocr")
                self.assertEqual(request_id, self.envelope.job_id)
                self.assertEqual(image_id, self.image_key)
                return {name: None for name in FIELDS}

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))
        self.assertEqual(events, ["claim", "ocr", "complete"])

    def test_completion_wait_retries_same_result_without_reclaim_or_ocr(self) -> None:
        events: list[str] = []

        class Client:
            completions = 0

            def claim(inner, envelope):
                events.append("claim")
                return {"decision": "execute", "execution": dict(self.execution)}

            def complete(inner, envelope, execution, fields, completion_deadline):
                events.append("complete")
                inner.completions += 1
                return {"decision": "wait", "retry_after_ms": 10} if inner.completions == 1 else {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                events.append("ocr")
                return {name: None for name in FIELDS}

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))
        self.assertEqual(events, ["claim", "ocr", "complete", "complete"])

    def test_rpc_failure_escapes_without_ack_and_without_db_guess(self) -> None:
        class Client:
            def claim(inner, envelope):
                raise ConsumerError("go_boundary_unavailable")

        class Pipeline:
            def recognize(inner, *args):
                raise AssertionError("no claim, no OCR")

        with self.assertRaisesRegex(ConsumerError, "go_boundary_unavailable"):
            self.processor(Client(), Pipeline()).process(self.envelope)

    def test_pipeline_failure_is_recorded_with_closed_error_code_before_ack(self) -> None:
        observed: list[str] = []

        class Client:
            def claim(inner, envelope):
                return {"decision": "execute", "execution": dict(self.execution)}

            def fail(inner, envelope, execution, error_code, completion_deadline):
                observed.append(error_code)
                return {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                raise ConsumerError("processing_timeout")

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))
        self.assertEqual(observed, ["processing_timeout"])

    def test_failure_wait_retries_same_failure_without_reclaim(self) -> None:
        events: list[str] = []

        class Client:
            failures = 0

            def claim(inner, envelope):
                events.append("claim")
                return {"decision": "execute", "execution": dict(self.execution)}

            def fail(inner, envelope, execution, error_code, completion_deadline):
                events.append("fail")
                inner.failures += 1
                return {"decision": "wait", "retry_after_ms": 10} if inner.failures == 1 else {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                events.append("ocr")
                raise ConsumerError("processing_timeout")

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))
        self.assertEqual(events, ["claim", "ocr", "fail", "fail"])

    def test_claim_must_match_attempt_and_stay_within_image_limit(self) -> None:
        invalid = dict(self.execution, size_bytes=1025)
        with self.assertRaisesRegex(ConsumerError, "invalid_claim_response"):
            _validate_execution(invalid, self.envelope, self.config)
        invalid = dict(self.execution, attempt_no=True)
        with self.assertRaisesRegex(ConsumerError, "invalid_claim_response"):
            _validate_execution(invalid, self.envelope, self.config)

    def test_unexpected_image_size_does_not_run_pipeline(self) -> None:
        self.image_path.write_bytes(b"different size")

        class Client:
            def claim(inner, envelope):
                return {"decision": "execute", "execution": dict(self.execution)}

            def fail(inner, envelope, execution, error_code, completion_deadline):
                self.assertEqual(error_code, "image_unprocessable")
                return {"decision": "ack"}

        class Pipeline:
            def recognize(inner, *args):
                raise AssertionError("size mismatch must be rejected before OCR")

        self.assertTrue(self.processor(Client(), Pipeline()).process(self.envelope))


class PipelineSupervisorTests(unittest.TestCase):
    def test_deadline_kills_and_discards_the_hung_child(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = config_for(root)
            script = root / "slow_worker.py"
            script.write_text(
                "import sys,time\n"
            "print('{\"schema_version\":\"passport-russiandocsocr-worker/v1\",\"type\":\"ready\"}', flush=True)\n"
                "sys.stdin.readline()\n"
                "time.sleep(10)\n",
                encoding="utf-8",
            )
            image_key = uid()
            image = config.image_root / image_key
            image.write_bytes(b"x")
            image.chmod(0o600)
            supervisor = PipelineSupervisor(config, worker_script=script)
            try:
                started = time.monotonic()
                with self.assertRaisesRegex(ConsumerError, "processing_timeout"):
                    supervisor.recognize(uid(), image_key, image, "image/jpeg", 80)
                self.assertLess(time.monotonic() - started, 2)
                self.assertIsNone(supervisor.child)
            finally:
                supervisor.close()

    @unittest.skipUnless(sys.platform.startswith("linux"), "Linux parent-death signal")
    def test_sigkill_of_consumer_parent_kills_model_child(self) -> None:
        module_root = Path(__file__).resolve().parents[2]
        child_code = (
            "import os,sys,time; sys.path.insert(0,sys.argv[1]); "
            "from ocr.runtime.russiandocs_worker import install_parent_death_signal; "
            "install_parent_death_signal(int(sys.argv[2])); print('ready',flush=True); time.sleep(60)"
        )
        parent_code = (
            "import subprocess,sys,os,time; "
            "p=subprocess.Popen([sys.executable,'-c',sys.argv[2],sys.argv[1],str(os.getpid())],"
            "stdout=subprocess.PIPE,start_new_session=True); "
            "print(p.pid,flush=True); time.sleep(60)"
        )
        parent = subprocess.Popen(
            [sys.executable, "-c", parent_code, str(module_root), child_code],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, start_new_session=True,
        )
        try:
            assert parent.stdout is not None
            line = parent.stdout.readline().strip()
            self.assertTrue(line.isdigit(), "parent did not report child PID")
            child_pid = int(line)
            os.kill(parent.pid, 9)
            parent.wait(timeout=2)
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                try:
                    with open(f"/proc/{child_pid}/stat", encoding="ascii") as stat_file:
                        state = stat_file.read().split()[2]
                    if state == "Z":
                        break
                except FileNotFoundError:
                    break
                time.sleep(0.05)
            else:
                self.fail("OCR child survived its consumer parent")
        finally:
            if parent.poll() is None:
                parent.kill()
                parent.wait(timeout=2)


class RabbitAdapterTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.config = config_for(self.root)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_queue_declaration_uses_quorum_sac_timeout_and_dlq(self) -> None:
        calls = []

        class Channel:
            def exchange_declare(self, **kwargs):
                calls.append(("exchange", kwargs))

            def queue_declare(self, **kwargs):
                calls.append(("queue", kwargs))

            def queue_bind(self, **kwargs):
                calls.append(("bind", kwargs))

        worker = RabbitWorker(self.config, None, None)
        worker._declare_topology(Channel())
        queue = next(args for kind, args in calls if kind == "queue" and args["queue"] == self.config.queue)
        self.assertEqual(queue["arguments"], {
            "x-queue-type": "quorum",
            "x-delivery-limit": -1,
            "x-single-active-consumer": True,
            "x-consumer-timeout": self.config.consumer_ack_timeout_ms,
            "x-dead-letter-exchange": self.config.dead_letter_exchange,
            "x-dead-letter-routing-key": "recognition.dead",
        })

    def test_ack_runs_only_after_processor_result_on_connection_io_callback(self) -> None:
        called = threading.Event()

        class Channel:
            is_open = True
            acked = []

            def basic_ack(self, **kwargs):
                self.acked.append(kwargs["delivery_tag"])
                called.set()

            def basic_reject(self, **kwargs):
                raise AssertionError("valid envelope should not be rejected")

            def close(self):
                self.is_open = False

        class Connection:
            def __init__(self):
                self.callbacks = []

            def add_callback_threadsafe(self, callback):
                self.callbacks.append(callback)

        class Processor:
            def process(self, envelope):
                return True

            def reject(self, envelope, error_code):
                return True

        worker = RabbitWorker(self.config, None, Processor())
        channel = Channel()
        connection = Connection()
        worker.channel = channel
        worker.connection = connection
        worker.generation = 4
        self_event = uid()
        message = {"schema_version": 1, "event_id": self_event, "job_id": uid(), "attempt_no": 1}
        properties = type("Properties", (), {"message_id": self_event})()
        worker._on_delivery(4, channel, type("Method", (), {"delivery_tag": 77})(), properties,
                            json.dumps(message).encode("utf-8"))
        thread = worker.delivery_thread
        assert thread is not None
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(channel.acked, [], "ack must not run from the processing thread")
        self.assertEqual(len(connection.callbacks), 1)
        connection.callbacks.pop()()
        self.assertTrue(called.wait(timeout=1))
        self.assertEqual(channel.acked, [77])

    def test_stale_connection_generation_cannot_ack_new_channel(self) -> None:
        class Channel:
            is_open = True
            acked = []

            def basic_ack(self, **kwargs):
                self.acked.append(kwargs["delivery_tag"])

        class Processor:
            def process(self, envelope):
                return True

            def reject(self, envelope, code):
                return True

        worker = RabbitWorker(self.config, None, Processor())
        new_channel = Channel()
        worker.channel = new_channel
        worker.generation = 9
        worker._settle(8, 4, "ack")
        self.assertEqual(new_channel.acked, [])

    def test_malformed_message_is_dead_lettered_without_go_or_ocr(self) -> None:
        rejected = []

        class Channel:
            def basic_reject(self, **kwargs):
                rejected.append(kwargs)

        class Processor:
            def process(self, envelope):
                raise AssertionError("malformed delivery cannot reach Go claim")

            def reject(self, envelope, code):
                raise AssertionError("malformed delivery has no trusted identity")

        worker = RabbitWorker(self.config, None, Processor())
        worker._on_delivery(1, Channel(), type("Method", (), {"delivery_tag": 13})(), None, b"not-json")
        self.assertEqual(rejected, [{"delivery_tag": 13, "requeue": False}])

    def test_message_id_mismatch_is_durably_rejected_by_go_before_ack(self) -> None:
        calls = []
        pending = []

        class Channel:
            is_open = True

            def basic_ack(self, **kwargs):
                calls.append(("ack", kwargs["delivery_tag"]))

            def basic_reject(self, **kwargs):
                calls.append(("reject", kwargs["delivery_tag"]))

            def close(self):
                self.is_open = False

        class Connection:
            def add_callback_threadsafe(self, callback):
                pending.append(callback)

        class Processor:
            def process(self, envelope):
                calls.append("claim")
                return True

            def reject(self, envelope, error_code):
                calls.append(("go_reject", error_code))
                return True

        worker = RabbitWorker(self.config, None, Processor())
        worker.channel = Channel()
        worker.connection = Connection()
        worker.generation = 1
        body = valid_body()
        envelope, _ = parse_envelope(body)
        props = type("Properties", (), {"message_id": "different"})()
        worker._on_delivery(1, worker.channel, type("Method", (), {"delivery_tag": 5})(), props, body)
        assert worker.delivery_thread is not None
        worker.delivery_thread.join(timeout=2)
        self.assertEqual(calls, [("go_reject", "message_id_mismatch")])
        self.assertEqual(len(pending), 1)
        pending.pop()()
        self.assertEqual(calls[-1], ("ack", 5))


class ConfigAndHTTPTests(unittest.TestCase):
    def test_config_is_strict_and_private_and_token_shape_is_checked(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = config_for(root)
            from dataclasses import asdict
            values = asdict(config)
            for key, value in list(values.items()):
                if isinstance(value, Path):
                    values[key] = str(value)
            config_path = root / "worker.json"
            config_path.write_text(json.dumps(values), encoding="utf-8")
            config_path.chmod(0o600)
            loaded = Config.load(config_path)
            self.assertEqual(loaded.consumer_ack_timeout_ms, config.consumer_ack_timeout_ms)
            values["unexpected"] = True
            config_path.write_text(json.dumps(values), encoding="utf-8")
            with self.assertRaisesRegex(ConsumerError, "invalid_worker_config"):
                Config.load(config_path)

    def test_go_http_redirect_is_not_followed(self) -> None:
        forwarded = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                if self.path == "/start":
                    self.send_response(307)
                    self.send_header("Location", "/forward")
                    self.end_headers()
                else:
                    forwarded.append(self.path)
                    self.send_response(200)
                    self.end_headers()

            def log_message(self, *args):
                return

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            client = GoWorkerClient(f"http://127.0.0.1:{server.server_port}", "pwt1_" + "A" * 43, 500, 1)
            with self.assertRaisesRegex(ConsumerError, "go_boundary_redirect"):
                client.post("/start", {})
            self.assertEqual(forwarded, [])
        finally:
            server.shutdown()
            thread.join(timeout=2)
            server.server_close()

    def test_bootstrap_copies_only_whitelisted_secrets_into_private_tmpfs(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = config_for(root)
            source_password = root / "mounted-rabbit-password"
            source_token = root / "mounted-worker-token"
            source_password.write_text("rabbit-secret\n", encoding="ascii")
            source_token.write_text("pwt1_" + "A" * 43 + "\n", encoding="ascii")
            source_password.chmod(0o600)
            source_token.chmod(0o600)
            config_values = {
                field: str(value) if isinstance(value, Path) else value
                for field, value in __import__("dataclasses").asdict(config).items()
            }
            config_values["rabbit_password_file"] = str(source_password)
            config_values["worker_token_file"] = str(source_token)
            config_path = root / "worker.json"
            config_path.write_text(json.dumps(config_values), encoding="utf-8")
            config_path.chmod(0o600)
            private_tmpfs = root / "runtime-tmpfs"
            private_tmpfs.mkdir(mode=0o700)
            original_modes = (source_password.stat().st_mode & 0o777, source_token.stat().st_mode & 0o777)
            with (
                mock.patch.object(secret_bootstrap.os, "geteuid", return_value=0),
                mock.patch.object(secret_bootstrap.os, "chown"),
                mock.patch.object(secret_bootstrap.os, "fchown"),
                mock.patch.dict(secret_bootstrap.SECRET_SOURCES, {
                    "rabbit_password_file": source_password,
                    "worker_token_file": source_token,
                }),
            ):
                copied_config = secret_bootstrap.prepare(config_path, 10001, 10001, private_tmpfs)
            self.assertEqual(copied_config, private_tmpfs.resolve() / "worker-config.json")
            loaded = Config.load(copied_config)
            self.assertEqual(loaded.readiness_marker, private_tmpfs.resolve() / "ready")
            self.assertEqual(loaded.read_secret(loaded.rabbit_password_file), "rabbit-secret")
            self.assertEqual(loaded.read_secret(loaded.worker_token_file), "pwt1_" + "A" * 43)
            self.assertEqual(copied_config.stat().st_mode & 0o777, 0o600)
            self.assertEqual(loaded.rabbit_password_file.stat().st_mode & 0o777, 0o600)
            self.assertEqual(loaded.worker_token_file.stat().st_mode & 0o777, 0o600)
            self.assertEqual((source_password.stat().st_mode & 0o777, source_token.stat().st_mode & 0o777), original_modes)
            copied_json = copied_config.read_text(encoding="utf-8")
            self.assertNotIn("rabbit-secret", copied_json)
            self.assertNotIn("pwt1_", copied_json)

    def test_bootstrap_healthcheck_drops_to_worker_identity_before_exec(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            root.chmod(0o700)
            root = root.resolve()
            uid, gid = os.getuid(), os.getgid()
            config_path = root / "worker-config.json"
            config_path.write_text("{}", encoding="ascii")
            config_path.chmod(0o600)
            stdout, stderr = io.StringIO(), io.StringIO()
            with (
                mock.patch.object(secret_bootstrap.os, "geteuid", return_value=0),
                mock.patch.object(secret_bootstrap.os, "setgroups") as setgroups,
                mock.patch.object(secret_bootstrap.os, "setgid") as setgid,
                mock.patch.object(secret_bootstrap.os, "setuid") as setuid,
                mock.patch.object(secret_bootstrap.os, "execv") as execv,
                contextlib.redirect_stdout(stdout),
                contextlib.redirect_stderr(stderr),
            ):
                result = secret_bootstrap.healthcheck(config_path, uid, gid, root)
            self.assertEqual(result, 1)  # mocked exec returns; real exec replaces the process
            setgroups.assert_called_once_with([])
            setgid.assert_called_once_with(gid)
            setuid.assert_called_once_with(uid)
            args = execv.call_args.args
            self.assertEqual(args[0], sys.executable)
            self.assertEqual(args[1], [
                sys.executable, "-m", "ocr.runtime.async_consumer",
                "--config", str(config_path), "healthcheck",
            ])
            self.assertEqual(stdout.getvalue(), "")
            self.assertEqual(stderr.getvalue(), "")

            with mock.patch.object(secret_bootstrap.os, "geteuid", return_value=0), \
                    mock.patch.object(secret_bootstrap.os, "execv") as rejected_exec:
                self.assertEqual(secret_bootstrap.healthcheck(root / "other.json", uid, gid, root), 1)
            rejected_exec.assert_not_called()


class ReadinessTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.config = config_for(self.root)
        self.marker = ReadinessMarker(self.config)
        self.consumer_pid = os.getpid()
        self.child_pid = self.consumer_pid + 100000

    def tearDown(self) -> None:
        self.marker.clear()
        self.temp.cleanup()

    def _proc_starts(self, pid, proc_root=Path("/proc")):
        del proc_root
        if pid == self.consumer_pid:
            return 101
        if pid == self.child_pid:
            return 202
        raise FileNotFoundError("process identity unavailable")

    def test_marker_requires_both_warmed_processes_and_is_private_non_pii(self) -> None:
        with mock.patch("ocr.runtime.async_consumer._proc_start_ticks", side_effect=self._proc_starts):
            self.assertFalse(readiness_healthy(self.config))
            self.marker.publish((self.child_pid, 202))
            now = time.monotonic_ns()
            self.assertTrue(readiness_healthy(self.config, now_monotonic_ns=now))
            data = self.config.readiness_marker.read_text(encoding="ascii")
            self.assertNotIn("job_id", data)
            self.assertNotIn("image_path", data)
            self.assertNotIn("worker_token", data)
            self.assertNotIn("rabbit-secret", data)
            self.assertNotIn(str(self.config.image_root), data)
            self.assertEqual(self.config.readiness_marker.stat().st_mode & 0o777, 0o600)
            self.config.readiness_marker.chmod(0o644)
            self.assertFalse(readiness_healthy(self.config, now_monotonic_ns=now))
            self.config.readiness_marker.chmod(0o600)
            self.root.chmod(0o750)
            self.assertFalse(readiness_healthy(self.config, now_monotonic_ns=now))
            self.root.chmod(0o700)
            marker_time = json.loads(data)["updated_monotonic_ns"]
            stale_at = now + (4 * self.config.rabbit_heartbeat_seconds + 10) * 1_000_000_000
            self.assertFalse(readiness_healthy(self.config, now_monotonic_ns=stale_at))
            self.assertFalse(readiness_healthy(self.config, now_monotonic_ns=marker_time - 1))
            with mock.patch("ocr.runtime.async_consumer._proc_start_ticks", side_effect=FileNotFoundError):
                self.assertFalse(readiness_healthy(self.config, now_monotonic_ns=now))
            self.marker.clear()  # Normal shutdown removes the marker, rather than leaving a stale ready state.
            self.assertFalse(self.config.readiness_marker.exists())

    def test_healthcheck_is_a_silent_exit_status_probe(self) -> None:
        from dataclasses import asdict

        values = {key: str(value) if isinstance(value, Path) else value for key, value in asdict(self.config).items()}
        config_path = self.root / "healthcheck.json"
        config_path.write_text(json.dumps(values), encoding="utf-8")
        config_path.chmod(0o600)
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch("ocr.runtime.async_consumer.readiness_healthy", return_value=True):
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", ResourceWarning)
                with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    result = main(["--config", str(config_path), "healthcheck"])
        self.assertEqual(result, 0)
        self.assertEqual(stdout.getvalue(), "")
        self.assertEqual(stderr.getvalue(), "")

    def test_rabbit_tick_keeps_busy_job_healthy_and_clears_on_child_death(self) -> None:
        starts = {self.consumer_pid: 101, self.child_pid: 202}

        class FakeConnection:
            is_open = True

            def __init__(self):
                self.callbacks = []
                self.closed = False

            def call_later(self, delay, callback):
                self.callbacks.append((delay, callback))

            def close(self):
                self.closed = True
                self.is_open = False

        class FakeChannel:
            is_open = True

        class FakePipeline:
            child_alive = True

            def readiness_identity(self):
                return (self.child_pid, 202) if self.child_alive else None

        connection = FakeConnection()
        channel = FakeChannel()
        pipeline = FakePipeline()
        pipeline.child_pid = self.child_pid
        worker = RabbitWorker(self.config, None, SimpleNamespace(pipeline=pipeline), self.marker)
        worker.connection, worker.channel, worker.generation = connection, channel, 7
        with mock.patch("ocr.runtime.async_consumer._proc_start_ticks", side_effect=lambda pid, proc_root=Path("/proc"): starts[pid]):
            worker._start_readiness(connection, channel, 7)
            worker.delivery_thread = threading.current_thread()  # Busy OCR does not block Rabbit I/O ticks.
            delay, callback = connection.callbacks.pop(0)
            self.assertGreater(delay, 0)
            callback()
            self.assertTrue(readiness_healthy(self.config))

            channel.is_open = False
            _, callback = connection.callbacks.pop(0)
            callback()
            self.assertFalse(readiness_healthy(self.config))  # Broker/channel disconnect clears the marker.
            self.assertFalse(self.config.readiness_marker.exists())
            channel.is_open = True
            worker._start_readiness(connection, channel, 7)

            pipeline.child_alive = False
            _, callback = connection.callbacks.pop(0)
            callback()
            self.assertFalse(readiness_healthy(self.config))
            self.assertFalse(self.config.readiness_marker.exists())
            self.assertTrue(connection.closed)


if __name__ == "__main__":
    unittest.main()
