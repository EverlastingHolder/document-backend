import importlib.util
import json
import signal
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


sys.dont_write_bytecode = True
MODULE_PATH = Path(__file__).with_name("async-runtime-bridge.py")
SPEC = importlib.util.spec_from_file_location("async_runtime_bridge", MODULE_PATH)
bridge = importlib.util.module_from_spec(SPEC)
assert SPEC is not None and SPEC.loader is not None
SPEC.loader.exec_module(bridge)


class BridgeRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.ssh_config = self.root / "ssh_config"
        self.ssh_config.write_text("Host test\n")
        self.ssh_config.chmod(0o600)
        self.config_path = self.root / "bridge.json"
        self.data = {
            "schema_version": bridge.SCHEMA,
            "bind_address": "192.0.2.7",
            "bind_port": 18080,
            "target_address": "172.30.0.21",
            "target_port": 8080,
            "ssh_config": str(self.ssh_config),
            "ssh_host_alias": "isolated-profile",
        }
        self.config_path.write_text(json.dumps(self.data))
        self.config_path.chmod(0o600)

    def tearDown(self):
        self.temp.cleanup()

    def test_descriptor_rejects_option_like_host_alias(self):
        self.data["ssh_host_alias"] = "-oProxyCommand=unsafe"
        self.config_path.write_text(json.dumps(self.data))
        self.config_path.chmod(0o600)
        with self.assertRaises(bridge.BridgeError):
            bridge.load_config(self.config_path)

    def test_new_ssh_command_has_bounded_connect_and_no_shared_controlmaster(self):
        argv = bridge.build_ssh_argv(self.data)
        self.assertIn("-S", argv)
        self.assertEqual(argv[argv.index("-S") + 1], "none")
        options = [argv[i + 1] for i, value in enumerate(argv[:-1]) if value == "-o"]
        self.assertIn("ControlMaster=no", options)
        self.assertIn("ControlPath=none", options)
        self.assertIn("ForkAfterAuthentication=no", options)
        self.assertIn("ConnectTimeout=5", options)
        self.assertIn("ConnectionAttempts=3", options)
        self.assertEqual(argv[-1], self.data["ssh_host_alias"])

    def test_process_inspection_has_a_timeout(self):
        with mock.patch.object(bridge.subprocess, "run", side_effect=bridge.subprocess.TimeoutExpired("ps", 3)) as run:
            with self.assertRaises(bridge.BridgeError):
                bridge.run(["ps", "-p", "1"])
        self.assertEqual(run.call_args.kwargs["timeout"], 3.0)

    def test_cleanup_signals_only_verified_new_listener(self):
        pid = 4444
        record = {"pid": pid, "start_time": "Mon Nov  2 10:00:00 2026", "command_sha256": "verified"}
        with (
            mock.patch.object(bridge, "verify_command", return_value=True),
            mock.patch.object(bridge, "listener_pids", side_effect=[[pid], []]),
            mock.patch.object(bridge, "lstart", return_value=None),
            mock.patch.object(bridge.os, "kill") as kill,
            mock.patch.object(bridge.time, "monotonic", side_effect=[1.0, 2.0]),
        ):
            self.assertTrue(bridge.cleanup_started_process(pid, record, self.data))
        kill.assert_called_once_with(pid, signal.SIGTERM)

    def test_cleanup_refuses_unverified_pid(self):
        pid = 4546
        record = {"pid": pid, "start_time": "unknown", "command_sha256": "not-matching"}
        with (
            mock.patch.object(bridge, "verify_command", return_value=False),
            mock.patch.object(bridge.os, "kill") as kill,
        ):
            self.assertFalse(bridge.cleanup_started_process(pid, record, self.data))
        kill.assert_not_called()

    def test_start_timeout_cleans_only_its_exact_spawned_process(self):
        pid = 4242
        fake_process = mock.Mock(pid=pid)
        fake_process.poll.return_value = None
        argv = bridge.build_ssh_argv(self.data)
        with (
            mock.patch.object(bridge, "read_pid_record", return_value=None),
            mock.patch.object(bridge, "listener_pids", return_value=[]),
            mock.patch.object(bridge.subprocess, "Popen", return_value=fake_process) as popen,
            mock.patch.object(bridge, "command_info", return_value=("ssh", argv)),
            mock.patch.object(bridge, "lstart", return_value="Mon Nov  2 10:00:00 2026"),
            mock.patch.object(bridge, "cleanup_started_process", return_value=True) as cleanup,
            mock.patch.object(bridge.time, "monotonic", side_effect=[10.0, 31.0]),
            mock.patch.object(bridge, "START_TIMEOUT_SECONDS", 20),
        ):
            with self.assertRaisesRegex(bridge.BridgeError, "bounded startup deadline"):
                bridge.start(self.config_path, self.data)
        self.assertEqual(popen.call_args.args[0], argv)
        self.assertTrue(popen.call_args.kwargs["start_new_session"])
        self.assertTrue(popen.call_args.kwargs["close_fds"])
        self.assertEqual(cleanup.call_args.args[0], pid)

    def test_failed_pid_record_write_cleans_spawned_process(self):
        pid = 4343
        fake_process = mock.Mock(pid=pid)
        fake_process.poll.return_value = None
        argv = bridge.build_ssh_argv(self.data)
        record = {"pid": pid, "start_time": "Mon Nov  2 10:00:00 2026", "command_sha256": bridge.command_digest(argv)}
        with (
            mock.patch.object(bridge, "read_pid_record", return_value=None),
            mock.patch.object(bridge, "listener_pids", side_effect=[[], [pid]]),
            mock.patch.object(bridge.subprocess, "Popen", return_value=fake_process),
            mock.patch.object(bridge, "command_info", return_value=("ssh", argv)),
            mock.patch.object(bridge, "lstart", return_value=record["start_time"]),
            mock.patch.object(bridge, "verify_process", return_value=True),
            mock.patch.object(bridge, "write_pid_record", side_effect=bridge.BridgeError("private record write failed")),
            mock.patch.object(bridge, "cleanup_started_process", return_value=True) as cleanup,
            mock.patch.object(bridge.time, "monotonic", side_effect=[10.0, 11.0]),
        ):
            with self.assertRaisesRegex(bridge.BridgeError, "private record write failed"):
                bridge.start(self.config_path, self.data)
        self.assertEqual(cleanup.call_args.args[:3], (pid, record, self.data))

    def test_unknown_pid_record_is_never_signalled(self):
        record = {"pid": 4545, "start_time": "unknown", "command_sha256": "not-matching"}
        with (
            mock.patch.object(bridge, "read_pid_record", return_value=record),
            mock.patch.object(bridge, "verify_process", return_value=False),
            mock.patch.object(bridge.os, "kill") as kill,
        ):
            with self.assertRaisesRegex(bridge.BridgeError, "refusing to signal"):
                bridge.stop(self.config_path, self.data)
        kill.assert_not_called()

    def test_pid_record_requires_private_mode(self):
        path = bridge.private_pid_path(self.config_path)
        path.write_text('{"pid": 1234}')
        path.chmod(0o644)
        with self.assertRaises(bridge.BridgeError):
            bridge.read_pid_record(path)
        self.assertTrue(stat.S_ISREG(path.stat().st_mode))


if __name__ == "__main__":
    unittest.main()
