from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path


SCRIPT_PATH = Path(__file__).with_name("build-linux-deployment-kit.py")
SPEC = importlib.util.spec_from_file_location("build_linux_deployment_kit", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
kit = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = kit
SPEC.loader.exec_module(kit)


def put(root: Path, relative: str, data: bytes = b"placeholder\n", mode: int = 0o644) -> Path:
    path = root / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    path.chmod(mode)
    return path


def git_commit(root: Path) -> str:
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Kit Test"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.email", "kit-test@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(root), "add", "."], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)
    return subprocess.run(
        ["git", "-C", str(root), "rev-parse", "HEAD"], check=True,
        stdout=subprocess.PIPE, text=True,
    ).stdout.strip()


class LinuxKitTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.repo = self.base / "public"
        self.source = self.base / "upstream"
        self.models = self.base / "models"
        self.repo.mkdir()
        self.source.mkdir()
        self.models.mkdir()
        self._make_project()
        self.source_commit = self._make_source()
        kit.EXPECTED_SOURCE_COMMIT = self.source_commit

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _make_project(self) -> None:
        for relative in kit.PROJECT_EXACT_FILES:
            put(self.repo, relative)
        put(self.repo, "cmd/api/main.go", mode=0o644)
        put(self.repo, "cmd/api/main_test.go")
        put(self.repo, "internal/jobs/model.go")
        put(self.repo, "internal/jobs/model_test.go")
        put(self.repo, "internal/jobstore/postgres/migrations/0001.sql")
        put(self.repo, "internal/jobstore/postgres/private.json")
        put(self.repo, "ocr/runtime/async_consumer.py")
        put(self.repo, "ocr/runtime/russiandocs_worker.py")
        put(self.repo, "ocr/runtime/secret_bootstrap.py")
        put(self.repo, "install.sh", b"#!/bin/sh\n", 0o755)
        put(self.repo, "deploy/linux/install-target.sh", b"#!/bin/sh\n", 0o755)
        put(self.repo, "deploy/linux/templates/worker.json", b"{}\n")
        put(self.repo, "outputs/private.txt", b"must not enter bundle")
        self.project_commit = git_commit(self.repo)

    def _make_source(self) -> str:
        put(self.source, "document_processing/__init__.py", b"__version__ = '4.10.0'\n")
        put(self.source, "document_processing/config/alphabets.py")
        put(self.source, "document_processing/config/ocr_alphabets.json", b"{}\n")
        put(self.source, "document_processing/pipeline_modules/page_registration/templates/ref.json",
            b'{"pages":[{"image":"reference.jpg","mask":"reference-mask.png"}]}\n')
        put(self.source, "document_processing/pipeline_modules/page_registration/templates/reference.jpg", b"synthetic erased template")
        put(self.source, "document_processing/pipeline_modules/page_registration/templates/reference-mask.png", b"synthetic mask")
        weight = b"synthetic-weight-a"
        table = b"synthetic-weight-b"
        entries = []
        for name, data in (("recognizer.onnx", weight), ("labels.npz", table)):
            put(self.models, name, data)
            entries.append({"path": name, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
        lock = {"models_version": "v11", "files": entries}
        lock_bytes = (json.dumps(lock, sort_keys=True) + "\n").encode()
        put(self.source, "document_processing/models.lock.json", lock_bytes)
        put(self.source, "LICENSE", b"fixture license\n")
        put(self.source, "NOTICE", b"fixture notice\n")
        return git_commit(self.source)

    def test_allowlist_excludes_tests_research_outputs_and_git_metadata(self) -> None:
        files = kit._repo_files(self.repo)
        paths = {item.archive_path for item in files}
        self.assertIn("cmd/api/main.go", paths)
        self.assertIn("internal/jobstore/postgres/migrations/0001.sql", paths)
        self.assertIn("deploy/linux/templates/worker.json", paths)
        self.assertIn("compose.lan.yaml", paths)
        self.assertIn("deploy/ingress/Dockerfile.lan", paths)
        self.assertIn("deploy/ingress/Caddyfile.lan", paths)
        self.assertNotIn("cmd/api/main_test.go", paths)
        self.assertNotIn("internal/jobs/model_test.go", paths)
        self.assertNotIn("internal/jobstore/postgres/private.json", paths)
        self.assertNotIn("outputs/private.txt", paths)
        self.assertFalse(any(path.startswith(".git/") for path in paths))

    def test_lan_runtime_overlay_is_required_in_bundle(self) -> None:
        (self.repo / "compose.lan.yaml").unlink()
        with self.assertRaisesRegex(kit.KitError, "required_project_file_missing"):
            kit.collect_inventory(self.repo, self.source, self.models)

    def test_model_snapshot_checks_lock_hash_and_rejects_unlocked_assets(self) -> None:
        lock_path = self.source / "document_processing/models.lock.json"
        lock, _, model_files = kit._validate_models(self.models, lock_path)
        self.assertEqual(lock["models_version"], "v11")
        self.assertEqual(len(model_files), 2)
        self.assertEqual(sum(item.source.stat().st_size for item in model_files), len(b"synthetic-weight-a") + len(b"synthetic-weight-b"))

        (self.models / "unlocked.onnx").write_bytes(b"unexpected")
        with self.assertRaisesRegex(kit.KitError, "unlocked_model_asset"):
            kit._validate_models(self.models, lock_path)

    def test_model_hash_mismatch_fails_closed(self) -> None:
        (self.models / "recognizer.onnx").write_bytes(b"changed")
        with self.assertRaisesRegex(kit.KitError, "model_asset_hash_mismatch"):
            kit._validate_models(self.models, self.source / "document_processing/models.lock.json")

    def test_image_and_credential_payloads_are_rejected_without_echo(self) -> None:
        for path, data, category in (
            ("assets/models/photo.jpg", b"x", "unapproved_model_file_type"),
            ("docs/example.md", b"Bearer pbt1_" + b"A" * 43, "credential_material_detected"),
            ("deploy/linux/example.env", b"postgres://user:secret@example.invalid/db", "credential_material_detected"),
            ("deploy/config.json", b"/Users/holder/private/model", "host_path_detected"),
        ):
            with self.subTest(category=category):
                with self.assertRaisesRegex(kit.KitError, category) as caught:
                    kit._content_scan(path, data)
                self.assertNotIn(data.decode("utf-8", errors="ignore"), str(caught.exception))

    def test_symlinked_model_is_rejected(self) -> None:
        (self.models / "alias.onnx").symlink_to(self.models / "recognizer.onnx")
        with self.assertRaisesRegex(kit.KitError, "model_asset_symlink"):
            kit._validate_models(self.models, self.source / "document_processing/models.lock.json")

    def test_inventory_manifest_and_archive_are_private_and_reproducible(self) -> None:
        files, manifest = kit.collect_inventory(self.repo, self.source, self.models)
        self.assertEqual(manifest["source"]["commit"], self.source_commit)
        self.assertEqual(manifest["source"]["version"], "4.10.0")
        self.assertTrue(manifest["source"]["clean"])
        self.assertEqual(manifest["source"]["package_files_count"], 7)
        self.assertEqual(manifest["source"]["license_files_count"], 2)
        self.assertEqual(manifest["source"]["files_count"], 10)
        self.assertEqual(manifest["source"]["templates_files_count"], 3)
        self.assertEqual(manifest["models"]["lock_version"], "v11")
        paths = {entry["path"] for entry in manifest["files"]}
        self.assertIn("assets/russiandocs/LICENSE", paths)
        self.assertIn("assets/russiandocs/NOTICE", paths)
        self.assertIn("assets/russiandocs/document_processing/pipeline_modules/page_registration/templates/reference.jpg", paths)
        self.assertIn("assets/models/recognizer.onnx", paths)
        self.assertNotIn(".git/config", paths)
        self.assertNotIn("outputs/private.txt", paths)

        local = self.repo / ".local"
        local.mkdir(mode=0o700)
        local.chmod(0o700)
        output_dir = local / "linux-deployment"
        output = output_dir / "kit-a.tar.gz"
        kit.write_bundle(output, files, manifest, self.repo)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(output_dir.stat().st_mode), 0o700)
        with tarfile.open(output, "r:gz") as archive:
            members = archive.getmembers()
            self.assertTrue(all(member.isfile() for member in members))
            self.assertTrue(all(not member.name.startswith("/") and ".." not in Path(member.name).parts for member in members))
            names = {member.name for member in members}
            self.assertIn("install.sh", names)
            self.assertIn("bundle-manifest.json", names)
            self.assertNotIn("outputs/private.txt", names)
            bundled_manifest = json.load(archive.extractfile("bundle-manifest.json"))
            self.assertEqual(len(bundled_manifest["files"]), len(files) - 1)
            self.assertEqual(bundled_manifest["bundle"]["checksums"], "SHA256SUMS")
            checksum_entries = archive.extractfile("SHA256SUMS").read().decode("ascii").splitlines()
            self.assertEqual(len(checksum_entries), len(files))
            for line in checksum_entries:
                digest, archive_path = line.split("  ", 1)
                if archive_path == "bundle-manifest.json":
                    self.assertEqual(digest, hashlib.sha256(archive.extractfile(archive_path).read()).hexdigest())
                else:
                    entry = next(item for item in bundled_manifest["files"] if item["path"] == archive_path)
                    self.assertEqual(digest, entry["sha256"])
            for entry in bundled_manifest["files"]:
                data = archive.extractfile(entry["path"]).read()
                self.assertEqual(len(data), entry["size"])
                self.assertEqual(hashlib.sha256(data).hexdigest(), entry["sha256"])

        second = output_dir / "kit-b.tar.gz"
        kit.write_bundle(second, files, manifest, self.repo)
        self.assertEqual(output.read_bytes(), second.read_bytes())

    def test_archive_output_rejects_existing_file_and_nonprivate_parent(self) -> None:
        files, manifest = kit.collect_inventory(self.repo, self.source, self.models)
        local = self.repo / ".local"
        local.mkdir(mode=0o700)
        local.chmod(0o700)
        output_dir = local / "linux-deployment"
        output_dir.mkdir(mode=0o700)
        output = output_dir / "kit.tar.gz"
        output.write_bytes(b"existing")
        with self.assertRaisesRegex(kit.KitError, "output_already_exists"):
            kit.write_bundle(output, files, manifest, self.repo)

        output.unlink()
        output_dir.chmod(0o755)
        with self.assertRaisesRegex(kit.KitError, "output_parent_not_private"):
            kit.write_bundle(output, files, manifest, self.repo)


if __name__ == "__main__":
    unittest.main()
