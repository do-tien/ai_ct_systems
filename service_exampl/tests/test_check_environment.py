"""Behaviour tests for the LC1-03 environment verifier.

Fixtures are tiny synthetic bundle trees; they prove the identity/guard behaviour,
not model quality or real checkpoint loading.
"""

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from nodule_ai import check_environment
from nodule_ai.check_environment import (
    CheckError,
    check_environment as run_check,
    main,
    verify_bundle,
)

HAS_TORCH = check_environment._torch_available()


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def make_bundle(root: Path, *, version="0.6.11", revision="a" * 40, checkpoint=True, config_bytes=b'{"inference": true}') -> tuple[Path, dict]:
    """Create a minimal bundle tree plus a manifest that matches it."""
    (root / "configs").mkdir(parents=True, exist_ok=True)
    metadata = json.dumps({"version": version}).encode()
    (root / "configs" / "metadata.json").write_bytes(metadata)
    (root / "configs" / "inference.json").write_bytes(config_bytes)

    files = [
        {"path": "configs/metadata.json", "algorithm": "sha256", "digest": _sha256(metadata), "size": len(metadata)},
        {"path": "configs/inference.json", "algorithm": "sha256", "digest": _sha256(config_bytes), "size": len(config_bytes)},
    ]
    manifest = {
        "schema_version": "1",
        "bundle_name": "lung_nodule_ct_detection",
        "bundle_version": version,
        "source_revision": revision,
        "license": "Apache-2.0",
        "files": files,
    }
    if checkpoint:
        (root / "models").mkdir(parents=True, exist_ok=True)
        weights = b"fake-checkpoint-bytes"
        (root / "models" / "model.pt").write_bytes(weights)
        files.append(
            {
                "path": "models/model.pt",
                "algorithm": "sha256",
                "digest": _sha256(weights),
                "size": len(weights),
                "expected_upstream": {"algorithm": "md5", "digest": hashlib.md5(weights).hexdigest()},
            }
        )
    return root, manifest


class VerifyBundleTests(unittest.TestCase):
    def test_accepts_intact_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle")
            verify_bundle(root, manifest, require_weights=True)

    def test_rejects_modified_config(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle")
            (root / "configs" / "inference.json").write_bytes(b'{"inference": false}')
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=False)

    def test_rejects_revision_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle", revision="not-a-revision")
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=False)

    def test_rejects_version_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle", version="0.6.11")
            manifest["bundle_version"] = "9.9.9"
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=False)

    def test_rejects_modified_checkpoint_when_weights_required(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle")
            (root / "models" / "model.pt").write_bytes(b"tampered")
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=True)

    def test_rejects_upstream_hash_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle")
            for entry in manifest["files"]:
                if entry["path"] == "models/model.pt":
                    entry["expected_upstream"]["digest"] = "0" * 32
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=True)

    def test_allows_missing_weights_without_requirement(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, manifest = make_bundle(Path(tmp) / "bundle", checkpoint=False)
            verify_bundle(root, manifest, require_weights=False)
            with self.assertRaises(ValueError):
                verify_bundle(root, manifest, require_weights=True)


class CliEvidenceTests(unittest.TestCase):
    def _write(self, root: Path, manifest: dict) -> Path:
        path = root / "manifest.json"
        path.write_text(json.dumps(manifest), encoding="utf-8")
        return path

    def test_load_mode_fails_without_checkpoint_and_records_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root, manifest = make_bundle(base / "bundle", checkpoint=False)
            manifest_path = self._write(base, manifest)
            out = base / "environment.json"

            code = main(
                [
                    "--mode", "load",
                    "--bundle-dir", str(root),
                    "--manifest", str(manifest_path),
                    "--device", "cpu",
                    "--out", str(out),
                ]
            )
            self.assertEqual(code, 1)
            evidence = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(evidence["status"], "failed")
            self.assertEqual(evidence["mode"], "load")
            self.assertEqual(evidence["error"]["code"], "verification_failed")
            self.assertIn("model_load", evidence)

    def test_failed_rerun_replaces_stale_passed_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root, manifest = make_bundle(base / "bundle", checkpoint=False)
            manifest_path = self._write(base, manifest)
            out = base / "environment.json"
            out.write_text(json.dumps({"schema_version": "1", "mode": "imports", "status": "passed"}), encoding="utf-8")

            code = main(
                [
                    "--mode", "load",
                    "--bundle-dir", str(root),
                    "--manifest", str(manifest_path),
                    "--device", "cpu",
                    "--out", str(out),
                ]
            )
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["status"], "failed")

    def test_load_mode_requires_device(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root, manifest = make_bundle(base / "bundle")
            manifest_path = self._write(base, manifest)
            with self.assertRaises(SystemExit) as raised:
                main(
                    [
                        "--mode", "load",
                        "--bundle-dir", str(root),
                        "--manifest", str(manifest_path),
                        "--out", str(base / "environment.json"),
                    ]
                )
            self.assertEqual(raised.exception.code, 2)

    @unittest.skipUnless(HAS_TORCH, "requires the AI environment (torch/monai)")
    def test_imports_mode_records_not_run_fields(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root, manifest = make_bundle(base / "bundle", checkpoint=False)
            manifest_path = self._write(base, manifest)
            out = base / "environment.json"

            code = main(
                [
                    "--mode", "imports",
                    "--bundle-dir", str(root),
                    "--manifest", str(manifest_path),
                    "--out", str(out),
                ]
            )
            self.assertEqual(code, 0)
            evidence = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(evidence["status"], "passed")
            self.assertEqual(evidence["device"]["actual"], "not_run")
            self.assertEqual(evidence["hardware"], "not_run")
            self.assertEqual(evidence["model_load"]["status"], "not_run")
            self.assertEqual(evidence["bundle"]["revision"], manifest["source_revision"])
            for name in ("monai", "torch", "numpy", "nibabel", "SimpleITK"):
                self.assertIn(name, evidence["packages"])

    @unittest.skipUnless(HAS_TORCH, "requires the AI environment (torch/monai)")
    def test_explicit_cuda_unavailable_fails_without_cpu_fallback(self):
        import torch

        if torch.cuda.is_available():
            self.skipTest("host has CUDA; the no-fallback path is not exercised here")

        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root, manifest = make_bundle(base / "bundle")
            manifest_path = self._write(base, manifest)
            with self.assertRaises(CheckError) as raised:
                run_check("load", root, manifest, "cuda:0")
            self.assertEqual(raised.exception.code, "device_unavailable")


if __name__ == "__main__":
    unittest.main()
