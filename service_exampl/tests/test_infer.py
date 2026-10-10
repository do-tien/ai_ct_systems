"""Inference CLI lifecycle: real subprocess error contract and output safety."""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
import pydicom

from nodule_ai.infer import PeakRssSampler

HAS_SITK = importlib.util.find_spec("SimpleITK") is not None
HAS_PYDICOM = importlib.util.find_spec("pydicom") is not None

try:  # the fixture writer is shared with test_volume
    from test_volume import make_series
except ImportError:  # pragma: no cover - discovery top-level dir differs
    make_series = None


def _success_document() -> dict:
    return {
        "schema_version": "1",
        "series_instance_uid": "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000001",
        "frame_of_reference_uid": "1.3.6.1.4.1.14519.5.2.1.6279.6001.300000000000000000000000000001",
        "coordinate_system": "DICOM_LPS_mm",
        "model": {
            "bundle": "lung_nodule_ct_detection",
            "bundle_version": "0.6.11",
            "monai_version": "1.4.0",
            "torch_version": "2.4.1+cpu",
            "affine_lps_to_ras": True,
            "score_threshold": 0.5,
        },
        "input": {"num_slices": 3, "spacing_mm": [0.7, 0.7, 1.0], "size_voxels": [4, 4, 3]},
        "candidates": [],
        "timing_s": {"load": 1.0, "inference": 1.0, "total": 2.0},
        "resources": {"device": "cpu", "peak_gpu_mem_mb": 0.0, "peak_rss_mb": 100.0},
    }


@unittest.skipUnless(HAS_SITK and HAS_PYDICOM, "requires the geometry environment (SimpleITK + pydicom)")
class InferCliTests(unittest.TestCase):
    def _run(self, args: list[str]) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, "-m", "nodule_ai.infer", *args],
            capture_output=True,
            text=True,
            cwd=str(Path(__file__).resolve().parents[1]),
        )

    def _series(self, root: Path, *, modality="CT", thickness=1.0, frame_uid=None, drop_interior=False, slices=3) -> Path:
        directory = root / "series"
        plane = np.zeros((4, 4), dtype=np.int16)
        make_series(directory, [(float(k), plane + k, k + 1, f"s{k}.dcm") for k in range(slices)])
        if modality != "CT":
            for path in sorted(directory.iterdir()):
                ds = pydicom.dcmread(path)
                ds.Modality = modality
                ds.save_as(path, write_like_original=False)
        if thickness > 2.5:
            for path in sorted(directory.iterdir()):
                ds = pydicom.dcmread(path)
                ds.SliceThickness = "3.0"
                ds.save_as(path, write_like_original=False)
        if frame_uid:
            target = sorted(directory.iterdir())[1]
            ds = pydicom.dcmread(target)
            ds.FrameOfReferenceUID = frame_uid
            ds.save_as(target, write_like_original=False)
        if drop_interior:
            sorted(directory.iterdir())[1].unlink()
        return directory

    def test_geometry_is_validated_before_any_bundle_or_model_work(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bundle = root / "no-bundle"
            # Valid series: geometry passes, so the missing bundle is reached
            # next (exit 3) without importing torch in this environment.
            good = self._series(root)
            out = root / "result.json"
            completed = self._run(
                ["--series-dir", str(good), "--out", str(out), "--bundle-dir", str(bundle), "--device", "cpu"]
            )
            self.assertEqual(completed.returncode, 3, completed.stderr)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "BUNDLE_INVALID")

            # Unsuitable series: rejected by geometry (exit 2) even though the
            # bundle is also missing, proving geometry runs first.
            other = root / "other"
            directory = self._series(other, modality="MR")
            out.write_text(json.dumps(_success_document()), encoding="utf-8")
            completed = self._run(
                ["--series-dir", str(directory), "--out", str(out), "--bundle-dir", str(bundle), "--device", "cpu"]
            )
            self.assertEqual(completed.returncode, 2, completed.stderr)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "NOT_CT")

    def test_unsuitable_series_exit_codes_and_envelope(self):
        cases = [
            ("not-ct", {"modality": "MR"}, "NOT_CT"),
            ("thick", {"thickness": 3.0}, "SLICE_TOO_THICK"),
            (
                "frame-mix",
                {"frame_uid": "1.3.6.1.4.1.14519.5.2.1.6279.6001.999999999999999999999999999999"},
                "UID_MISMATCH",
            ),
            ("gap", {"drop_interior": True, "slices": 4}, "NONUNIFORM_SPACING"),
        ]
        for label, kwargs, expected in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                directory = self._series(root, **kwargs)
                out = root / "result.json"
                out.write_text(json.dumps(_success_document()), encoding="utf-8")

                completed = self._run(
                    ["--series-dir", str(directory), "--out", str(out),
                     "--bundle-dir", str(root / "no-bundle"), "--device", "cpu"]
                )
                self.assertEqual(completed.returncode, 2, completed.stderr)
                document = json.loads(out.read_text(encoding="utf-8"))
                self.assertEqual(sorted(document), ["error", "schema_version"])
                self.assertEqual(document["schema_version"], "1")
                self.assertEqual(document["error"]["code"], expected)
                self.assertNotIn("candidates", document)

    def test_argument_failures_replace_previous_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root)
            for args in (
                ["--score-threshold", "nan"],
                ["--score-threshold", "inf"],
                ["--score-threshold", "-0.1"],
                ["--score-threshold", "1.5"],
                ["--device", "tpu"],
            ):
                with self.subTest(args=args):
                    out = root / "result.json"
                    out.write_text(json.dumps(_success_document()), encoding="utf-8")
                    completed = self._run(
                        ["--series-dir", str(directory), "--out", str(out),
                         "--bundle-dir", str(root / "no-bundle"), *args]
                    )
                    self.assertEqual(completed.returncode, 2, completed.stderr)
                    document = json.loads(out.read_text(encoding="utf-8"))
                    self.assertEqual(document["error"]["code"], "INVALID_ARGUMENT")

    def test_unknown_flag_and_missing_option_are_exit_2(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root)
            out = root / "result.json"
            out.write_text(json.dumps(_success_document()), encoding="utf-8")

            completed = self._run(
                ["--series-dir", str(directory), "--out", str(out), "--bundle-dir", str(root / "b"),
                 "--unknown-flag", "1"]
            )
            self.assertEqual(completed.returncode, 2)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "INVALID_ARGUMENT")

            out.write_text(json.dumps(_success_document()), encoding="utf-8")
            completed = self._run(["--series-dir", str(directory), "--out", str(out)])
            self.assertEqual(completed.returncode, 2)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "INVALID_ARGUMENT")

    def test_duplicate_out_is_rejected_without_writing(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root)
            first = root / "first.json"
            second = root / "second.json"
            first.write_text(json.dumps(_success_document()), encoding="utf-8")
            completed = self._run(
                ["--series-dir", str(directory), "--out", str(first), "--out", str(second),
                 "--bundle-dir", str(root / "no-bundle")]
            )
            self.assertEqual(completed.returncode, 2)
            self.assertIn("ambiguous", completed.stderr)
            self.assertFalse(second.exists())

    def test_result_path_cannot_destroy_input(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root)
            before = sorted((path.name, path.stat().st_size) for path in directory.iterdir())
            out = directory / "result.json"

            completed = self._run(
                ["--series-dir", str(directory), "--out", str(out),
                 "--bundle-dir", str(root / "no-bundle"), "--device", "cpu"]
            )
            self.assertEqual(completed.returncode, 1, completed.stderr)
            self.assertIn("INVALID_OUTPUT_PATH", completed.stderr)
            self.assertFalse(out.exists())
            self.assertEqual(sorted((path.name, path.stat().st_size) for path in directory.iterdir()), before)

    def test_error_publication_failure_is_nonzero_and_leaves_no_stale_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root, modality="MR")
            blocker = root / "blocker"
            blocker.write_text("not a directory", encoding="utf-8")
            out = blocker / "result.json"

            completed = self._run(
                ["--series-dir", str(directory), "--out", str(out),
                 "--bundle-dir", str(root / "no-bundle"), "--device", "cpu"]
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertFalse(out.exists())

    def test_failure_cleanup_removes_only_owned_temporary_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = self._series(root)
            out = root / "result.json"
            unrelated = root / "notes.txt"
            unrelated.write_text("keep", encoding="utf-8")

            self._run(["--series-dir", str(directory), "--out", str(out),
                       "--bundle-dir", str(root / "no-bundle"), "--device", "cpu"])
            leftovers = [path.name for path in root.iterdir() if path.name.endswith(".tmp")]
            self.assertEqual(leftovers, [])
            self.assertEqual(unrelated.read_text(encoding="utf-8"), "keep")


@unittest.skipUnless(HAS_SITK and HAS_PYDICOM, "requires the geometry environment (SimpleITK + pydicom)")
class DependencyFailureTests(unittest.TestCase):
    @unittest.skipIf(importlib.util.find_spec("torch") is not None, "torch is importable here")
    def test_missing_torch_is_an_operational_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "series"
            plane = np.zeros((4, 4), dtype=np.int16)
            make_series(directory, [(float(k), plane + k, k + 1, f"s{k}.dcm") for k in range(3)])
            out = root / "result.json"
            completed = subprocess.run(
                [sys.executable, "-m", "nodule_ai.infer", "--series-dir", str(directory), "--out", str(out),
                 "--bundle-dir", str(root / "no-bundle"), "--device", "cuda"],
                capture_output=True, text=True, cwd=str(Path(__file__).resolve().parents[1]),
            )
            self.assertEqual(completed.returncode, 1, completed.stderr)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["error"]["code"], "TOOL_ERROR")


class PeakRssSamplerTests(unittest.TestCase):
    def test_peak_uses_maximum_and_reports_mib(self):
        samples = iter([100 * 1024**2, 300 * 1024**2, 200 * 1024**2] + [150 * 1024**2] * 10)
        sampler = PeakRssSampler(interval=0.001, rss_reader=lambda: next(samples, 150 * 1024**2))
        with sampler:
            pass
        self.assertEqual(sampler.peak_bytes, 300 * 1024**2)
        self.assertAlmostEqual(sampler.peak_mb, 300.0, places=6)

    def test_thread_is_joined_even_when_the_body_raises(self):
        sampler = PeakRssSampler(interval=0.001, rss_reader=lambda: 64 * 1024**2)
        with self.assertRaises(RuntimeError):
            with sampler:
                raise RuntimeError("boom")
        self.assertFalse(sampler._thread.is_alive())
        self.assertAlmostEqual(sampler.peak_mb, 64.0, places=6)


if __name__ == "__main__":
    unittest.main()
