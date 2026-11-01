from __future__ import annotations

import importlib.util
import ipaddress
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT_PATH = Path(__file__).resolve().parents[1] / "deploy/linux/install_state.py"
SPEC = importlib.util.spec_from_file_location("passport_install_state", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
install_state = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = install_state
SPEC.loader.exec_module(install_state)


class DockerNetworkInventoryTests(unittest.TestCase):
    def inventory(self, routes, networks):
        outputs = [
            json.dumps(routes).encode(),
            " ".join(f"network-{i}" for i in range(len(networks))),
            json.dumps(networks).encode(),
        ]
        with mock.patch.object(install_state.subprocess, "check_output", side_effect=outputs):
            return install_state.host_networks()

    def test_handles_missing_and_null_ipam_metadata_and_mixed_ip_families(self) -> None:
        observed = self.inventory(
            [
                {"dst": "default"},
                {"dst": "10.42.0.0/16"},
                {"dst": "2001:db8:1::/64"},
            ],
            [
                {"Name": "network-without-ipam"},
                {"Name": "network-null-ipam", "IPAM": None},
                {"Name": "network-null-config", "IPAM": {"Config": None}},
                {"Name": "network-mixed", "IPAM": {"Config": [
                    {"Subnet": "172.30.0.0/24"},
                    {"Subnet": "2001:db8:2::/64"},
                    {"Subnet": None},
                ]}},
            ],
        )

        self.assertEqual(
            set(observed),
            {
                ipaddress.ip_network("10.42.0.0/16"),
                ipaddress.ip_network("2001:db8:1::/64"),
                ipaddress.ip_network("172.30.0.0/24"),
                ipaddress.ip_network("2001:db8:2::/64"),
            },
        )

    def test_selects_a_free_candidate_around_host_routes_and_existing_networks(self) -> None:
        routes = [{"dst": "172.30.0.0/24"}, {"dst": "default"}]
        networks = [
            {"Name": "other-project_private", "IPAM": {"Config": [{"Subnet": "172.31.0.0/24"}]}},
            {"Name": "ipv6-only", "IPAM": {"Config": [{"Subnet": "fd00::/64"}]}},
        ]
        outputs = [json.dumps(routes).encode(), "network-0 network-1", json.dumps(networks).encode()]
        with mock.patch.object(install_state.subprocess, "check_output", side_effect=outputs):
            selected = install_state.select_subnet()

        self.assertEqual(selected, "172.29.0.0/24")

    def test_does_not_silently_reuse_any_preexisting_project_network_for_a_fresh_subnet(self) -> None:
        # A fresh runtime has no trusted install marker. Any existing subnet,
        # including one named for this Compose project, remains occupied.
        networks = [
            {"Name": "passport-backend_private", "IPAM": {"Config": [{"Subnet": "172.30.0.0/24"}]}}
        ]
        outputs = [b"[]", "network-0", json.dumps(networks).encode()]
        with mock.patch.object(install_state.subprocess, "check_output", side_effect=outputs):
            selected = install_state.select_subnet()

        self.assertEqual(selected, "172.31.0.0/24")


class LanBindPolicyTests(unittest.TestCase):
    def test_accepts_private_and_loopback_ipv4_only(self) -> None:
        for address in ("10.8.1.2", "172.20.3.4", "192.168.5.6", "127.0.0.1"):
            with self.subTest(address=address):
                self.assertTrue(install_state.is_lan_bind_address(address))

    def test_rejects_wildcard_public_and_non_ipv4_addresses(self) -> None:
        for address in ("0.0.0.0", "8.8.8.8", "169.254.1.2", "100.64.1.2", "::1", "bad"):
            with self.subTest(address=address):
                self.assertFalse(install_state.is_lan_bind_address(address))

    def test_ingress_mode_bind_and_port_are_part_of_rerun_identity(self) -> None:
        lan = {"domain": "", "public_bind": "192.168.1.40", "tls_mode": "lan",
               "certificate_sha256": None, "http_port": 8080}
        self.assertTrue(install_state.ingress_identity_matches(lan, "", "192.168.1.40", "lan", None, 8080))
        self.assertFalse(install_state.ingress_identity_matches(lan, "", "192.168.1.40", "lan", None, 8081))
        self.assertFalse(install_state.ingress_identity_matches(lan, "", "192.168.1.40", "managed", None, None))
        self.assertFalse(install_state.ingress_identity_matches(lan, "", "192.168.1.41", "lan", None, 8080))

        # Existing HTTPS markers did not have an http_port field; preserving a
        # null HTTPS port keeps those installations eligible for safe reruns.
        legacy_https = {"domain": "api.example.test", "public_bind": "0.0.0.0",
                        "tls_mode": "managed", "certificate_sha256": None}
        self.assertTrue(install_state.ingress_identity_matches(
            legacy_https, "api.example.test", "0.0.0.0", "managed", None, None))

    def test_lan_environment_uses_http_only_overlay_values(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            env = install_state.install_env(Path(tmp), "172.29.0.0/24", "", "192.168.1.40", 10001, 10001,
                                            "lan", install_state.LAN_HTTP_PORT)
            self.assertTrue((Path(tmp) / "compose.lan.env").is_file())
            self.assertEqual(env["PASSPORT_PUBLIC_BIND"], "192.168.1.40")
            self.assertEqual(env["PASSPORT_HTTP_PORT"], "8080")
            self.assertEqual(env["PASSPORT_INGRESS_IP"], "172.29.0.20")
            self.assertNotIn("PASSPORT_TLS_CERTIFICATE_FILE", env)
            self.assertNotIn("PASSPORT_TLS_PRIVATE_KEY_FILE", env)
            self.assertEqual(install_state.install_env(
                Path(tmp), "172.29.0.0/24", "", "192.168.1.40", 10001, 10001,
                "lan", install_state.LAN_HTTP_PORT), env)


if __name__ == "__main__":
    unittest.main()
