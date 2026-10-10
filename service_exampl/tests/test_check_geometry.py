"""QA CLI tests: identity rejection, landmark bounds, and failed-run evidence."""

from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from nodule_ai.check_geometry import main
from nodule_ai.dicom_geometry import voxel_to_lps_affine, voxel_to_world

HAS_SITK = importlib.util.find_spec("SimpleITK") is not None
HAS_PYDICOM = importlib.util.find_spec("pydicom") is not None
HAS_MONAI = importlib.util.find_spec("monai") is not None

if HAS_SITK:
    from nodule_ai.volume import read_ct_volume

try:  # the fixture writer is shared with test_volume
    from test_volume import make_series
except ImportError:  # pragma: no cover - discovery top-level dir differs
    make_series = None


def _series(directory: Path) -> tuple[dict, dict]:
    plane = np.zeros((4, 6), dtype=np.int16)
    make_series(directory, [(float(k) * 1.2, plane + k, k + 1, f"s{k}.dcm") for k in range(3)])
    image, metadata = read_ct_volume(directory)
    affine = voxel_to_lps_affine(metadata)
    centre = list(voxel_to_world((2.0, 1.0, 1.0), affine))
    manifest = {
        "study_instance_uid": metadata["study_instance_uid"],
        "series_instance_uid": metadata["series_instance_uid"],
        "frame_of_reference_uid": metadata["frame_of_reference_uid"],
        "sop_instance_uids": metadata["sop_instance_uids"],
    }
    reference = {
        "study_instance_uid": metadata["study_instance_uid"],
        "series_instance_uid": metadata["series_instance_uid"],
        "frame_of_reference_uid": metadata["frame_of_reference_uid"],
        "target_reference_id": "nodule-test",
        "nodules": [
            {
                "id": "nodule-test",
                "center_mm": centre,
                "measurements": [{"annotation_id": 1, "center_mm": centre}],
            }
        ],
    }
    return manifest, reference


@unittest.skipUnless(HAS_SITK and HAS_PYDICOM, "requires the geometry environment (SimpleITK + pydicom)")
class GeometryCliTests(unittest.TestCase):
    def test_passing_run_writes_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            manifest, reference = _series(directory)
            (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
            (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
            out = root / "geometry-check.json"

            code = main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--out", str(out),
            ])
            self.assertEqual(code, 0)
            evidence = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(evidence["status"], "passed")
            self.assertEqual(evidence["completeness"], "verified")
            self.assertEqual(len(evidence["landmarks"]), 2)
            for landmark in evidence["landmarks"]:
                self.assertLessEqual(landmark["error_mm"], 1e-3)
                # Every landmark is checked against the independent raw-tag oracle.
                self.assertLessEqual(landmark["raw_tag_error_mm"], 1e-4)
            self.assertEqual(evidence["bundle_transform"]["status"], "not_run")
            self.assertTrue(evidence["nifti"]["written"])
            self.assertFalse(evidence["nifti"]["persisted"])

    def test_wrong_case_reference_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            manifest, reference = _series(directory)
            reference["series_instance_uid"] = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000009"
            (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
            (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
            out = root / "geometry-check.json"

            code = main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--out", str(out),
            ])
            self.assertEqual(code, 2)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "UID_MISMATCH")

    def test_wrong_inventory_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            manifest, reference = _series(directory)
            manifest["sop_instance_uids"] = manifest["sop_instance_uids"] + ["1.3.6.1.4.1.14519.5.2.1.6279.6001.999999999999999999999999999999"]
            (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
            (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
            out = root / "geometry-check.json"

            code = main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--out", str(out),
            ])
            self.assertNotEqual(code, 0)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "INCOMPLETE_SERIES")

    def test_nonfinite_and_out_of_volume_landmarks_are_rejected(self):
        for mutate, expected in (
            (lambda ref: ref["nodules"][0].__setitem__("center_mm", [float("nan"), 0.0, 0.0]), "INVALID_REFERENCE"),
            (lambda ref: ref["nodules"][0].__setitem__("center_mm", [5000.0, 5000.0, 5000.0]), "LANDMARK_OUT_OF_BOUNDS"),
        ):
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                directory = root / "series"
                manifest, reference = _series(directory)
                mutate(reference)
                (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
                (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
                out = root / "geometry-check.json"
                code = main([
                    "--series-dir", str(directory),
                    "--reference", str(root / "reference.json"),
                    "--manifest", str(root / "manifest.json"),
                    "--out", str(out),
                ])
                self.assertEqual(code, 2)
                self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], expected)

    def test_failed_verification_replaces_prior_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            manifest, reference = _series(directory)
            (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
            (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
            out = root / "geometry-check.json"
            sentinel = root / "unrelated.json"
            sentinel.write_text("{}", encoding="utf-8")

            self.assertEqual(main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--out", str(out),
            ]), 0)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["status"], "passed")

            (root / "reference.json").write_text("not json", encoding="utf-8")
            self.assertNotEqual(main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--out", str(out),
            ]), 0)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["status"], "failed")
            self.assertEqual(sentinel.read_text(encoding="utf-8"), "{}")

    def test_bundle_dir_without_monai_fails_loudly(self):
        if HAS_MONAI:
            self.skipTest("MONAI is importable here; the loud-failure path is not exercised")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            manifest, reference = _series(directory)
            (root / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
            (root / "reference.json").write_text(json.dumps(reference), encoding="utf-8")
            out = root / "geometry-check.json"
            code = main([
                "--series-dir", str(directory),
                "--reference", str(root / "reference.json"),
                "--manifest", str(root / "manifest.json"),
                "--bundle-dir", str(root / "bundle"),
                "--nifti-out", str(root / "volume.nii.gz"),
                "--out", str(out),
            ])
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "MONAI_UNAVAILABLE")


@unittest.skipUnless(HAS_MONAI, "requires the AI environment (monai)")
class BundleProbeGuardTests(unittest.TestCase):
    def test_missing_bundle_config_is_rejected(self):
        from nodule_ai.check_geometry import run_bundle_transform
        from nodule_ai.errors import GeometryError

        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(GeometryError) as raised:
                run_bundle_transform(Path(tmp) / "no-bundle", Path(tmp) / "x.nii.gz", [])
            self.assertEqual(raised.exception.code, "INVALID_REFERENCE")


if __name__ == "__main__":
    unittest.main()
