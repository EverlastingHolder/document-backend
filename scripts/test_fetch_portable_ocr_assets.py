from __future__ import annotations

import hashlib
import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parent
FETCH_PATH = SCRIPTS / "fetch_portable_ocr_assets.py"
FETCH_SPEC = importlib.util.spec_from_file_location("fetch_portable_ocr_assets", FETCH_PATH)
assert FETCH_SPEC is not None and FETCH_SPEC.loader is not None
fetcher = importlib.util.module_from_spec(FETCH_SPEC)
sys.modules[FETCH_SPEC.name] = fetcher
FETCH_SPEC.loader.exec_module(fetcher)
builder = fetcher.kit


def put(root: Path, relative: str, data: bytes) -> Path:
    path = root / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return path


class PortableAssetMaterializerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.source = self.base / "upstream"
        self.models = self.base / "locked-models"
        self.source.mkdir()
        self.models.mkdir()

        put(self.source, "document_processing/__init__.py", b"__version__ = '4.10.0'\n")
        put(self.source, "document_processing/pipeline.py", b"def run(): return None\n")
        put(self.source, "document_processing/config/alphabets.json", b'{"alphabet":"test"}\n')
        template_dir = "document_processing/pipeline_modules/page_registration/templates"
        put(self.source, f"{template_dir}/references.json",
            b'{"image":"reference.jpg","mask":"reference-mask.png"}\n')
        put(self.source, f"{template_dir}/reference.jpg", b"synthetic-template")
        put(self.source, f"{template_dir}/reference-mask.png", b"synthetic-mask")

        files = []
        for name, data in (("detector/model.onnx", b"synthetic-onnx"),
                           ("detector/model.json", b'{"threshold":0.5}\n')):
            put(self.models, name, data)
            files.append({"path": name, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
        put(self.source, "document_processing/models.lock.json",
            (json.dumps({"models_version": "v11", "files": files}, sort_keys=True) + "\n").encode())
        put(self.source, "LICENSE", b"MIT License\n")
        put(self.source, "NOTICE", b"Model and source license notice\n")
        put(self.source, ".gitignore", b"/document_processing/models/\n")

        subprocess.run(["git", "init", "-q", str(self.source)], check=True)
        subprocess.run(["git", "-C", str(self.source), "config", "user.name", "Asset Test"], check=True)
        subprocess.run(["git", "-C", str(self.source), "config", "user.email", "asset-test@example.invalid"], check=True)
        subprocess.run(["git", "-C", str(self.source), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.source), "commit", "-qm", "fixture"], check=True)
        self.commit = subprocess.run(
            ["git", "-C", str(self.source), "rev-parse", "HEAD"],
            check=True, text=True, stdout=subprocess.PIPE,
        ).stdout.strip()
        builder.EXPECTED_SOURCE_COMMIT = self.commit

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_standalone_script_loads_its_validator(self) -> None:
        result = subprocess.run(
            [sys.executable, str(FETCH_PATH), "--help"],
            check=False, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        self.assertEqual(result.returncode, 0)
        self.assertIn("--source-root", result.stdout)
        self.assertNotIn(str(self.base), result.stdout + result.stderr)

    def test_materializes_only_verified_assets_with_stable_manifest(self) -> None:
        out1 = self.base / "out1"
        out2 = self.base / "out2"
        first = fetcher.materialize(self.source, self.models, out1)
        second = fetcher.materialize(self.source, self.models, out2)

        self.assertEqual(first, second)
        self.assertEqual(first["source"]["commit"], self.commit)
        self.assertEqual(first["source"]["version"], "4.10.0")
        self.assertEqual(first["models"]["lock_version"], "v11")
        self.assertEqual(first["models"]["files_count"], 2)
        expected = {
            "russiandocs/LICENSE",
            "russiandocs/NOTICE",
            "russiandocs/SOURCE_PROVENANCE.json",
            "russiandocs/document_processing/__init__.py",
            "russiandocs/document_processing/pipeline.py",
            "russiandocs/document_processing/config/alphabets.json",
            "russiandocs/document_processing/pipeline_modules/page_registration/templates/references.json",
            "russiandocs/document_processing/pipeline_modules/page_registration/templates/reference.jpg",
            "russiandocs/document_processing/pipeline_modules/page_registration/templates/reference-mask.png",
            "russiandocs/document_processing/models.lock.json",
            "models/detector/model.onnx",
            "models/detector/model.json",
            "portable-assets.json",
        }
        actual = {p.relative_to(out1).as_posix() for p in out1.rglob("*") if p.is_file()}
        self.assertEqual(actual, expected)
        for rel in expected:
            self.assertEqual((out1 / rel).read_bytes(), (out2 / rel).read_bytes())
        manifest = json.loads((out1 / fetcher.MANIFEST_NAME).read_bytes())
        self.assertEqual(len(manifest["files"]), 12)
        for entry in manifest["files"]:
            data = (out1 / entry["path"]).read_bytes()
            self.assertEqual(len(data), entry["size"])
            self.assertEqual(hashlib.sha256(data).hexdigest(), entry["sha256"])
            self.assertEqual((out1 / entry["path"]).stat().st_mtime_ns, 0)
        for directory in (out1, *(p for p in out1.rglob("*") if p.is_dir())):
            self.assertEqual(directory.stat().st_mtime_ns, 0)
        self.assertEqual(json.loads((out1 / "russiandocs/SOURCE_PROVENANCE.json").read_bytes())["commit"], self.commit)

    def test_tampered_or_unlocked_model_fails_without_leaving_output(self) -> None:
        (self.models / "detector/model.onnx").write_bytes(b"tampered")
        output = self.base / "failed-output"
        with self.assertRaisesRegex(fetcher.AssetError, "model_asset_hash_mismatch"):
            fetcher.materialize(self.source, self.models, output)
        self.assertFalse(output.exists())

    def test_extra_model_file_fails_closed(self) -> None:
        put(self.models, "unexpected.onnx", b"not-in-lock")
        with self.assertRaisesRegex(fetcher.AssetError, "unlocked_model_asset"):
            fetcher.materialize(self.source, self.models, self.base / "failed-output")

    def test_existing_or_symlink_output_root_is_rejected(self) -> None:
        output = self.base / "existing"
        output.mkdir()
        with self.assertRaisesRegex(fetcher.AssetError, "output_root_must_be_new"):
            fetcher.materialize(self.source, self.models, output)

        alias = self.base / "alias"
        alias.symlink_to(output, target_is_directory=True)
        with self.assertRaisesRegex(fetcher.AssetError, "output_root_must_be_new"):
            fetcher.materialize(self.source, self.models, alias)

    def test_source_version_or_worktree_drift_fails_closed(self) -> None:
        init = self.source / "document_processing/__init__.py"
        init.write_bytes(b"__version__ = '4.10.1'\n")
        with self.assertRaisesRegex(fetcher.AssetError, "source_revision_mismatch"):
            fetcher.materialize(self.source, self.models, self.base / "failed-output")

    def test_private_credential_material_is_rejected_and_partial_output_removed(self) -> None:
        put(self.source, "document_processing/pipeline.py", b"# pbt1_" + b"A" * 43 + b"\n")
        subprocess.run(["git", "-C", str(self.source), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.source), "commit", "-qm", "unsafe fixture"], check=True)
        self.commit = subprocess.run(
            ["git", "-C", str(self.source), "rev-parse", "HEAD"],
            check=True, text=True, stdout=subprocess.PIPE,
        ).stdout.strip()
        builder.EXPECTED_SOURCE_COMMIT = self.commit

        output = self.base / "rejected-output"
        with self.assertRaisesRegex(fetcher.AssetError, "credential_material_detected"):
            fetcher.materialize(self.source, self.models, output)
        self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
