"""Bundle runtime tests: guards, tiny strict loader and transform-only geometry.

The transform tests exercise real MONAI transforms on a real NIfTI; they are
coordinate QA, not synthetic model evidence. No detector forward ever runs here.
"""

from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from nodule_ai.bundle_runtime import (
    AFFINE_LPS_TO_RAS,
    RESAMPLING_OVERRIDE_KEY,
    BundleError,
    _build_parser,
    check_bundle,
    load_bundle,
    normalize_device,
    validate_device,
)

HAS_TORCH = importlib.util.find_spec("torch") is not None
HAS_MONAI = importlib.util.find_spec("monai") is not None
HAS_NIBABEL = importlib.util.find_spec("nibabel") is not None
BUNDLE_DIR = Path(__file__).resolve().parents[1] / "models" / "lung_nodule_ct_detection" / "0.6.11"
HAS_BUNDLE = (BUNDLE_DIR / "configs" / "inference.json").is_file()


def _write_bundle(root: Path, *, version: str = "0.6.11", config: dict | None = None, weights: bool = True) -> Path:
    (root / "configs").mkdir(parents=True, exist_ok=True)
    (root / "configs" / "metadata.json").write_text(json.dumps({"version": version}), encoding="utf-8")
    (root / "configs" / "inference.json").write_text(json.dumps(config or {}), encoding="utf-8")
    if weights:
        (root / "models").mkdir(parents=True, exist_ok=True)
        (root / "models" / "model.pt").write_bytes(b"placeholder")
    return root


class BundleGuardTests(unittest.TestCase):
    def test_rejects_missing_or_wrong_version_bundle(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            with self.assertRaises(BundleError) as raised:
                check_bundle(root / "absent")
            self.assertEqual(raised.exception.code, "BUNDLE_INVALID")

            wrong = _write_bundle(root / "wrong", version="9.9.9")
            with self.assertRaises(BundleError) as raised:
                check_bundle(wrong)
            self.assertEqual(raised.exception.code, "BUNDLE_INVALID")

    def test_rejects_missing_checkpoint_when_required(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = _write_bundle(Path(tmp) / "bundle", weights=False)
            check_bundle(root, require_weights=False)
            with self.assertRaises(BundleError) as raised:
                check_bundle(root, require_weights=True)
            self.assertEqual(raised.exception.code, "BUNDLE_INVALID")

    def test_load_bundle_validates_before_touching_torch(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(BundleError) as raised:
                load_bundle(Path(tmp) / "absent", "cpu")
            self.assertEqual(raised.exception.code, "BUNDLE_INVALID")

    def test_invalid_devices_are_rejected(self):
        for device in ("", "tpu", "cuda:", "cuda:-1", "CUDA:0", "cpu:0", None):
            with self.subTest(device=device):
                with self.assertRaises(BundleError) as raised:
                    normalize_device(device)
                self.assertEqual(raised.exception.code, "DEVICE_UNAVAILABLE")
        self.assertEqual(normalize_device("cpu"), "cpu")
        self.assertEqual(normalize_device("cuda"), "cuda:0")
        self.assertEqual(normalize_device("cuda:3"), "cuda:3")

    @unittest.skipUnless(HAS_TORCH, "requires torch for device availability checks")
    def test_cuda_unavailable_does_not_fallback(self):
        import torch

        if torch.cuda.is_available():
            self.skipTest("host has CUDA; the no-fallback path is not exercised here")
        for device in ("cuda", "cuda:0"):
            with self.subTest(device=device):
                with self.assertRaises(BundleError) as raised:
                    validate_device(device)
                self.assertEqual(raised.exception.code, "DEVICE_UNAVAILABLE")
        self.assertEqual(validate_device("cpu"), "cpu")

    @unittest.skipUnless(HAS_TORCH, "requires torch for device availability checks")
    def test_nonexistent_cuda_index_is_rejected(self):
        import torch

        if not torch.cuda.is_available():
            self.skipTest("no CUDA device to index")
        with self.assertRaises(BundleError) as raised:
            validate_device(f"cuda:{torch.cuda.device_count() + 1}")
        self.assertEqual(raised.exception.code, "DEVICE_UNAVAILABLE")


@unittest.skipUnless(HAS_TORCH, "requires the AI environment (torch/monai)")
class StrictLoaderTests(unittest.TestCase):
    def test_strict_loader_rejects_incompatible_weights(self):
        import torch
        from monai.handlers.checkpoint_loader import CheckpointLoader

        from nodule_ai.bundle_runtime import engine_stub

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            model = torch.nn.Linear(3, 2)
            mismatched = torch.nn.Linear(4, 2)
            incompatible = root / "incompatible.pt"
            torch.save(mismatched.state_dict(), incompatible)

            loader = CheckpointLoader(load_path=str(incompatible), load_dict={"model": model})
            with self.assertRaises(Exception):
                loader(engine_stub())

            matching = root / "matching.pt"
            torch.save(model.state_dict(), matching)
            loader = CheckpointLoader(load_path=str(matching), load_dict={"model": model})
            loader(engine_stub())  # strict load of matching weights succeeds

    def test_engine_stub_satisfies_the_documented_handler_contract(self):
        from nodule_ai.bundle_runtime import engine_stub

        stub = engine_stub()
        self.assertIsNone(stub.state.max_epochs)
        self.assertEqual(stub.state.epoch, 0)


@unittest.skipUnless(HAS_MONAI, "requires the AI environment (monai)")
class OverrideTests(unittest.TestCase):
    def _config(self) -> dict:
        return {
            "imports": ["$import torch"],
            "bundle_root": ".",
            "device": "cpu",
            "amp": True,
            "detector_ops": [
                "$@detector.set_target_keys(box_key='box', label_key='label')",
                "$@detector.set_box_selector_parameters(score_thresh=0.02,topk_candidates_per_level=1000,nms_thresh=0.22,detections_per_img=300)",
            ],
            # Same transform layout as the pinned config: Spacingd lives at index 4
            # and is disabled there because the bundle expects pre-resampled input.
            "preprocessing": {
                "_target_": "Compose",
                "transforms": [
                    {"_target_": "LoadImaged", "keys": "image"},
                    {"_target_": "LoadImaged", "keys": "image", "reader": "itkreader", "_disabled_": True},
                    {"_target_": "EnsureChannelFirstd", "keys": "image"},
                    {"_target_": "Orientationd", "keys": "image", "axcodes": "RAS"},
                    {"_target_": "Spacingd", "keys": "image", "pixdim": [0.703125, 0.703125, 1.25], "_disabled_": True},
                    {"_target_": "ScaleIntensityRanged", "keys": "image", "a_min": -1024.0, "a_max": 300.0,
                     "b_min": 0.0, "b_max": 1.0, "clip": True},
                    {"_target_": "EnsureTyped", "keys": "image"},
                ],
            },
        }

    def test_resampling_override_enables_the_existing_spacing_transform(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = _write_bundle(Path(tmp) / "bundle", config=self._config())
            parser = _build_parser(root, "cpu", 0.25, resample=True)
            transforms = parser.get_parsed_content("preprocessing").transforms
            names = [type(t).__name__ for t in transforms if t is not None]
            self.assertEqual(names.count("Spacingd"), 1, "the override must enable the existing Spacingd")
            # The resampling effect itself is asserted by the transform probe below.

    def test_threshold_override_replaces_only_the_selector_floor(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = _write_bundle(Path(tmp) / "bundle", config=self._config())
            parser = _build_parser(root, "cpu", 0.25, resample=True)
            entry = parser.config["detector_ops"][1]
            self.assertIn("score_thresh=0.25", entry)
            self.assertIn("nms_thresh=0.22", entry)
            self.assertIn("topk_candidates_per_level=1000", entry)
            self.assertIn("detections_per_img=300", entry)

    def test_verifier_path_keeps_the_untouched_pinned_configuration(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = _write_bundle(Path(tmp) / "bundle", config=self._config())
            parser = _build_parser(root, "cpu", 0.5, resample=False)
            entry = parser.config["detector_ops"][1]
            self.assertIn("score_thresh=0.02", entry)
            transforms = parser.get_parsed_content("preprocessing").transforms
            names = [type(t).__name__ for t in transforms if t is not None]
            self.assertEqual(names.count("Spacingd"), 0, "the verifier keeps the pinned configuration untouched")

    def test_invalid_threshold_is_rejected(self):
        for threshold in (float("nan"), -0.1, 1.5, "abc"):
            with self.subTest(threshold=threshold):
                with self.assertRaises(BundleError) as raised:
                    load_bundle(Path("nope"), "cpu", score_threshold=threshold)
                self.assertEqual(raised.exception.code, "BUNDLE_INVALID")


@unittest.skipUnless(HAS_MONAI and HAS_NIBABEL and HAS_BUNDLE, "requires MONAI and the provisioned bundle")
class ResampledGeometryProbeTests(unittest.TestCase):
    """Transform-only proof of the resampled path: no network, no checkpoint."""

    def _nifti(self, root: Path) -> np.ndarray:
        import nibabel as nib

        from nodule_ai.dicom_geometry import lps_to_ras_affine

        # Oblique, anisotropic, non-zero origin, asymmetric volume.
        direction = np.array([[0.6, 0.8, 0.0], [-0.8, 0.6, 0.0], [0.0, 0.0, 1.0]])
        spacing = np.array([0.7, 1.3, 1.2])
        origin = np.array([-100.0, 40.0, -30.0])
        lps_affine = np.eye(4)
        lps_affine[:3, :3] = direction @ np.diag(spacing)
        lps_affine[:3, 3] = origin
        shape = (7, 9, 6)
        data = np.arange(np.prod(shape), dtype=np.float32).reshape(shape)  # (x, y, z)
        path = root / "oblique.nii.gz"
        nib.save(nib.Nifti1Image(data, lps_to_ras_affine(lps_affine)), str(path))
        return lps_affine

    def _nifti_axis_aligned(self, root: Path) -> None:
        import nibabel as nib

        from nodule_ai.dicom_geometry import lps_to_ras_affine

        lps_affine = np.eye(4)
        lps_affine[:3, :3] = np.diag([0.7, 1.3, 1.2])
        lps_affine[:3, 3] = [-100.0, 40.0, -30.0]
        data = np.arange(7 * 9 * 6, dtype=np.float32).reshape(7, 9, 6)
        nib.save(nib.Nifti1Image(data, lps_to_ras_affine(lps_affine)), str(root / "axis.nii.gz"))

    def test_resampled_oblique_boxes_match_lps(self):
        import torch

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self._nifti(root)
            parser = _build_parser(BUNDLE_DIR, "cpu", 0.5, resample=True)
            data = parser.get_parsed_content("preprocessing")({"image": str(root / "oblique.nii.gz")})
            image = data["image"]
            meta_affine = np.asarray(image.meta["affine"], dtype=float)

            # Spacingd must have resampled onto the configured isotropic grid,
            # which the untouched pinned configuration leaves disabled.
            spacing = np.linalg.norm(meta_affine[:3, :3], axis=0)
            np.testing.assert_allclose(spacing, [0.703125, 0.703125, 1.25], rtol=0, atol=1e-4)

            lps_affine = np.diag([-1.0, -1.0, 1.0, 1.0]) @ meta_affine
            clip = parser.get_parsed_content("postprocessing#transforms#0")
            to_world = parser.get_parsed_content("postprocessing#transforms#1")
            to_mode = parser.get_parsed_content("postprocessing#transforms#2")

            index = np.array(image.shape[1:], dtype=float) / 2.0
            expected_lps = (lps_affine @ np.append(index, 1.0))[:3]
            self.assertLess(expected_lps[0], 0.0)  # asymmetric left side
            self.assertNotAlmostEqual(expected_lps[0], 0.0, places=1)

            half = 2.0
            box = torch.tensor(
                [[index[0] - half, index[1] - half, index[2] - half, index[0] + half, index[1] + half, index[2] + half]]
            )
            staged = clip({"image": image, "box": box, "label": torch.tensor([0])})
            staged = to_world(staged)
            world = np.asarray(staged["box"], dtype=float)[0]
            centre = (world[:3] + world[3:]) / 2.0
            np.testing.assert_allclose(centre, expected_lps, rtol=0, atol=1e-3)

            converted = np.asarray(to_mode(dict(staged))["box"], dtype=float)[0]
            np.testing.assert_allclose(converted[:3], centre, rtol=0, atol=1e-3)

            # An empty box set stays empty and is not invented into a detection.
            empty = clip({"image": image, "box": torch.zeros((0, 6)), "label": torch.zeros((0,), dtype=torch.long)})
            self.assertEqual(int(empty["box"].shape[0]), 0)

    def test_axis_aligned_extents_follow_resampled_spacing(self):
        import torch

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self._nifti_axis_aligned(root)
            parser = _build_parser(BUNDLE_DIR, "cpu", 0.5, resample=True)
            image = parser.get_parsed_content("preprocessing")({"image": str(root / "axis.nii.gz")})["image"]
            meta_affine = np.asarray(image.meta["affine"], dtype=float)
            spacing = np.linalg.norm(meta_affine[:3, :3], axis=0)
            np.testing.assert_allclose(spacing, [0.703125, 0.703125, 1.25], rtol=0, atol=1e-4)

            half = 2.0
            index = np.array(image.shape[1:], dtype=float) / 2.0
            box = torch.tensor(
                [[index[0] - half, index[1] - half, index[2] - half, index[0] + half, index[1] + half, index[2] + half]]
            )
            clip = parser.get_parsed_content("postprocessing#transforms#0")
            to_world = parser.get_parsed_content("postprocessing#transforms#1")
            world = np.asarray(to_world(clip({"image": image, "box": box, "label": torch.tensor([0])}))["box"], dtype=float)[0]
            extents = np.abs(world[3:] - world[:3])
            # If Spacingd had been skipped the y extent would be 4*1.3 = 5.2 mm
            # instead of 4*0.703125 mm.
            np.testing.assert_allclose(extents, 2 * half * spacing, rtol=1e-3, atol=1e-3)

    def test_probe_uses_lps_not_ras(self):
        import torch

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self._nifti(root)
            parser = _build_parser(BUNDLE_DIR, "cpu", 0.5, resample=True)
            image = parser.get_parsed_content("preprocessing")({"image": str(root / "oblique.nii.gz")})["image"]
            meta_affine = np.asarray(image.meta["affine"], dtype=float)
            ras_affine = meta_affine
            lps_affine = np.diag([-1.0, -1.0, 1.0, 1.0]) @ meta_affine
            assert AFFINE_LPS_TO_RAS is True

            index = np.array(image.shape[1:], dtype=float) / 2.0
            box = torch.tensor([[index[0] - 1, index[1] - 1, index[2] - 1, index[0] + 1, index[1] + 1, index[2] + 1]])
            clip = parser.get_parsed_content("postprocessing#transforms#0")
            to_world = parser.get_parsed_content("postprocessing#transforms#1")
            world = np.asarray(to_world(clip({"image": image, "box": box, "label": torch.tensor([0])}))["box"], dtype=float)[0]
            centre = (world[:3] + world[3:]) / 2.0
            expected_lps = (lps_affine @ np.append(index, 1.0))[:3]
            expected_ras = (ras_affine @ np.append(index, 1.0))[:3]
            self.assertLess(float(np.linalg.norm(centre - expected_lps)), 1e-3)
            self.assertGreater(float(np.linalg.norm(centre - expected_ras)), 1.0)


if __name__ == "__main__":
    unittest.main()
