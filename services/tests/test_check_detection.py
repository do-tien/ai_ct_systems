"""Frozen-target nearest-centre comparison for LC1-06 (location QA only).

All fixtures below are synthetic QA documents, not detector output: a passing
comparison proves the comparator's behaviour, never that the real model placed
a candidate.
"""

from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from module_ai.check_detection import compare_detection

SERIES_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000001"
FRAME_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.300000000000000000000000000001"


def reference_document(**overrides) -> dict:
    document = {
        "schema_version": "1",
        "coordinate_system": "DICOM_LPS_mm",
        "series_instance_uid": SERIES_UID,
        "frame_of_reference_uid": FRAME_UID,
        "target_reference_id": "nodule-target",
        "nodules": [
            {"id": "nodule-target", "center_mm": [-90.0, 60.0, 1500.0], "diameter_mm": 10.0},
            {"id": "nodule-other", "center_mm": [10.0, -20.0, 800.0], "diameter_mm": 12.0},
        ],
    }
    document.update(overrides)
    return document


def result_document(candidates: list[dict] | None = None, **overrides) -> dict:
    document = {
        "schema_version": "1",
        "series_instance_uid": SERIES_UID,
        "frame_of_reference_uid": FRAME_UID,
        "coordinate_system": "DICOM_LPS_mm",
        "model": {
            "bundle": "lung_nodule_ct_detection",
            "bundle_version": "0.6.11",
            "monai_version": "1.4.0",
            "torch_version": "2.4.1+cpu",
            "affine_lps_to_ras": True,
            "score_threshold": 0.5,
        },
        "input": {"num_slices": 158, "spacing_mm": [0.698, 0.698, 2.0], "size_voxels": [512, 512, 158]},
        "candidates": [] if candidates is None else candidates,
        "timing_s": {"load": 1.5, "inference": 20.0, "total": 22.0},
        "resources": {"device": "cpu", "peak_gpu_mem_mb": 0.0, "peak_rss_mb": 1234.5},
    }
    document.update(overrides)
    return document


def candidate(cid: str, center, size=(9.0, 9.0, 9.0), score=0.9) -> dict:
    return {"id": cid, "center_mm": list(center), "size_mm": list(size), "score": score}


class NearestCandidateTests(unittest.TestCase):
    def test_nearest_candidate_uses_reference_radius_in_lps(self):
        result = result_document(
            candidates=[
                candidate("c1", [-90.0, 60.0, 1506.0], size=(30.0, 30.0, 30.0), score=0.99),
                candidate("c2", [-87.0, 64.0, 1500.0], size=(2.0, 2.0, 2.0), score=0.51),
            ]
        )
        comparison = compare_detection(result, reference_document())
        self.assertEqual(comparison["nearest_candidate"]["id"], "c2")
        self.assertAlmostEqual(comparison["distance_mm"], 5.0)
        self.assertEqual(comparison["target"]["radius_mm"], 5.0)
        self.assertEqual(comparison["location_status"], "passed")
        self.assertEqual(comparison["reason"], "WITHIN_RADIUS")

    def test_radius_boundary_and_outside(self):
        for offset, expected in ((4.999, "passed"), (5.0, "passed"), (5.001, "failed")):
            with self.subTest(offset=offset):
                result = result_document(candidates=[candidate("c1", [-90.0, 60.0, 1500.0 + offset])])
                comparison = compare_detection(result, reference_document())
                self.assertEqual(comparison["location_status"], expected)

    def test_empty_real_result_is_location_failure(self):
        comparison = compare_detection(result_document(candidates=[]), reference_document())
        self.assertEqual(comparison["location_status"], "failed")
        self.assertEqual(comparison["reason"], "NO_CANDIDATES")
        self.assertIsNone(comparison["nearest_candidate"])
        self.assertIsNone(comparison["distance_mm"])

    def test_equal_distance_uses_candidate_id(self):
        left = candidate("cB", [-87.0, 64.0, 1500.0])  # distance 5
        right = candidate("cA", [-95.0, 60.0, 1500.0])  # distance 5
        for order in ([left, right], [right, left]):
            with self.subTest(order=[c["id"] for c in order]):
                comparison = compare_detection(result_document(candidates=order), reference_document())
                self.assertEqual(comparison["nearest_candidate"]["id"], "cA")

    def test_frozen_target_not_other_nodule(self):
        # A perfect candidate for the non-selected reference nodule cannot
        # substitute for the frozen target.
        result = result_document(candidates=[candidate("c1", [10.0, -20.0, 800.0])])
        comparison = compare_detection(result, reference_document())
        self.assertEqual(comparison["target"]["id"], "nodule-target")
        self.assertEqual(comparison["nearest_candidate"]["id"], "c1")
        self.assertEqual(comparison["location_status"], "failed")
        self.assertEqual(comparison["reason"], "OUTSIDE_RADIUS")


class InputRejectionTests(unittest.TestCase):
    def assert_rejected(self, result: dict, reference: dict):
        with self.assertRaises(ValueError):
            compare_detection(result, reference)

    def test_identity_mismatch(self):
        self.assert_rejected(
            result_document(series_instance_uid="1.2.3"), reference_document()
        )
        self.assert_rejected(
            result_document(frame_of_reference_uid="1.2.3"), reference_document()
        )

    def test_reference_version_and_system(self):
        self.assert_rejected(result_document(), reference_document(schema_version="2"))
        self.assert_rejected(result_document(), reference_document(coordinate_system="RAS"))

    def test_target_missing_or_duplicated(self):
        self.assert_rejected(result_document(), reference_document(target_reference_id="nodule-missing"))
        duplicated = reference_document(
            nodules=[
                {"id": "nodule-target", "center_mm": [-90.0, 60.0, 1500.0], "diameter_mm": 10.0},
                {"id": "nodule-target", "center_mm": [-90.0, 60.0, 1500.0], "diameter_mm": 10.0},
            ]
        )
        self.assert_rejected(result_document(), duplicated)
        duplicate_other = reference_document(
            nodules=[
                {"id": "nodule-target", "center_mm": [-90.0, 60.0, 1500.0], "diameter_mm": 10.0},
                {"id": "nodule-x", "center_mm": [1.0, 1.0, 1.0], "diameter_mm": 5.0},
                {"id": "nodule-x", "center_mm": [2.0, 2.0, 2.0], "diameter_mm": 5.0},
            ]
        )
        self.assert_rejected(result_document(), duplicate_other)

    def test_malformed_target_geometry(self):
        bad_targets = [
            {"id": "nodule-target", "center_mm": [1.0, 2.0], "diameter_mm": 10.0},
            {"id": "nodule-target", "center_mm": [True, 2.0, 3.0], "diameter_mm": 10.0},
            {"id": "nodule-target", "center_mm": [float("nan"), 2.0, 3.0], "diameter_mm": 10.0},
            {"id": "nodule-target", "center_mm": [float("inf"), 2.0, 3.0], "diameter_mm": 10.0},
            {"id": "nodule-target", "center_mm": [1.0, 2.0, 3.0], "diameter_mm": 0.0},
            {"id": "nodule-target", "center_mm": [1.0, 2.0, 3.0], "diameter_mm": float("nan")},
            {"id": "", "center_mm": [1.0, 2.0, 3.0], "diameter_mm": 10.0},
        ]
        for target in bad_targets:
            with self.subTest(target=target):
                reference = reference_document(
                    nodules=[target, {"id": "nodule-other", "center_mm": [0.0, 0.0, 0.0], "diameter_mm": 5.0}]
                )
                self.assert_rejected(result_document(), reference)

    def test_malformed_lc1_05_result(self):
        broken = result_document()
        del broken["model"]
        self.assert_rejected(broken, reference_document())
        self.assert_rejected(
            {"schema_version": "1", "error": {"code": "MODEL_OOM", "message": "x"}}, reference_document()
        )

    def test_candidate_below_declared_threshold_is_inconsistent(self):
        result = result_document(candidates=[candidate("c1", [-90.0, 60.0, 1500.0], score=0.4)])
        self.assert_rejected(result, reference_document())

    def test_overflowing_distance_is_rejected(self):
        # Finite coordinates whose separation overflows to a non-finite distance.
        reference = reference_document(
            nodules=[
                {"id": "nodule-target", "center_mm": [1e308, 0.0, 0.0], "diameter_mm": 10.0},
                {"id": "nodule-other", "center_mm": [0.0, 0.0, 0.0], "diameter_mm": 5.0},
            ]
        )
        result = result_document(candidates=[candidate("c1", [-1e308, 0.0, 0.0])])
        self.assert_rejected(result, reference)


class CliTests(unittest.TestCase):
    """Real subprocess contract for the offline comparison CLI."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = Path(self._tmp.name)

    def _write(self, name: str, document: dict) -> Path:
        path = self.root / name
        path.write_text(json.dumps(document), encoding="utf-8")
        return path

    def _run(self, *args) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, "-m", "module_ai.check_detection", *map(str, args)],
            capture_output=True,
            text=True,
            cwd=str(Path(__file__).resolve().parents[1]),
        )

    def test_passing_boundary_publishes_evidence(self):
        result = self._write("result.json", result_document(candidates=[candidate("c1", [-90.0, 60.0, 1505.0])]))
        reference = self._write("reference.json", reference_document())
        out = self.root / "comparison.json"
        completed = self._run("--result", result, "--reference", reference, "--out", out)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["scope"], "lc1-06-location-only")
        self.assertEqual(evidence["location_status"], "passed")
        self.assertEqual(evidence["distance_mm"], 5.0)
        self.assertEqual(evidence["source_sha256"]["result"], hashlib.sha256(result.read_bytes()).hexdigest())
        self.assertEqual(evidence["source_sha256"]["reference"], hashlib.sha256(reference.read_bytes()).hexdigest())

    def test_outside_and_empty_are_failed_comparisons(self):
        reference = self._write("reference.json", reference_document())
        out = self.root / "comparison.json"
        outside = self._write("outside.json", result_document(candidates=[candidate("c1", [-90.0, 60.0, 1520.0])]))
        completed = self._run("--result", outside, "--reference", reference, "--out", out)
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["reason"], "OUTSIDE_RADIUS")

        empty = self._write("empty.json", result_document(candidates=[]))
        completed = self._run("--result", empty, "--reference", reference, "--out", out)
        self.assertEqual(completed.returncode, 1)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["location_status"], "failed")
        self.assertEqual(evidence["reason"], "NO_CANDIDATES")
        self.assertIsNone(evidence["nearest_candidate"])
        self.assertIsNone(evidence["distance_mm"])
        self.assertIn("source_sha256", evidence)

    def test_identity_mismatch_is_input_error(self):
        result = self._write("result.json", result_document(series_instance_uid="1.2.3"))
        reference = self._write("reference.json", reference_document())
        out = self.root / "comparison.json"
        completed = self._run("--result", result, "--reference", reference, "--out", out)
        self.assertEqual(completed.returncode, 2)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["location_status"], "invalid")
        self.assertEqual(evidence["error"]["code"], "INPUT_IDENTITY_MISMATCH")

    def test_malformed_json_is_input_error(self):
        result = self.root / "result.json"
        result.write_text("{ not json", encoding="utf-8")
        reference = self._write("reference.json", reference_document())
        out = self.root / "comparison.json"
        completed = self._run("--result", result, "--reference", reference, "--out", out)
        self.assertEqual(completed.returncode, 2)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["location_status"], "invalid")
        self.assertEqual(evidence["error"]["code"], "INVALID_INPUT")
        self.assertNotIn(str(self.root), completed.stderr)

    def test_failed_rerun_replaces_previous_pass(self):
        reference = self._write("reference.json", reference_document())
        result = self._write("result.json", result_document(candidates=[candidate("c1", [-90.0, 60.0, 1500.0])]))
        out = self.root / "comparison.json"
        self.assertEqual(self._run("--result", result, "--reference", reference, "--out", out).returncode, 0)
        self.assertEqual(json.loads(out.read_text(encoding="utf-8"))["location_status"], "passed")

        bad = self.root / "bad.json"
        bad.write_text("{ nope", encoding="utf-8")
        self.assertEqual(self._run("--result", result, "--reference", bad, "--out", out).returncode, 2)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["location_status"], "invalid")
        self.assertNotIn("nearest_candidate", evidence)

        # A passing run again, then a parser failure must also clear it.
        self.assertEqual(self._run("--result", result, "--reference", reference, "--out", out).returncode, 0)
        completed = self._run("--out", out)
        self.assertEqual(completed.returncode, 2)
        evidence = json.loads(out.read_text(encoding="utf-8"))
        self.assertEqual(evidence["location_status"], "invalid")

    def test_output_cannot_overwrite_source(self):
        reference = self._write("reference.json", reference_document())
        result = self._write("result.json", result_document(candidates=[candidate("c1", [-90.0, 60.0, 1500.0])]))
        result_before = result.read_bytes()
        reference_before = reference.read_bytes()

        completed = self._run("--result", result, "--reference", reference, "--out", result)
        self.assertEqual(completed.returncode, 2)
        self.assertIn("OUTPUT_COLLISION", completed.stderr)
        self.assertEqual(result.read_bytes(), result_before)

        completed = self._run("--result", result, "--reference", reference, "--out", reference)
        self.assertEqual(completed.returncode, 2)
        self.assertIn("OUTPUT_COLLISION", completed.stderr)
        self.assertEqual(reference.read_bytes(), reference_before)

    def test_abbreviated_option_cannot_delete_source(self):
        # An unrecognised/abbreviated option may not cause the source to be
        # deleted before the collision check.
        reference = self._write("reference.json", reference_document())
        result = self._write("result.json", result_document(candidates=[candidate("c1", [-90.0, 60.0, 1500.0])]))
        before = result.read_bytes()
        completed = self._run("--res", result, "--ref", reference, "--out", result)
        self.assertEqual(completed.returncode, 2)
        self.assertEqual(result.read_bytes(), before)
        self.assertTrue(reference.exists())

    def test_help_does_not_delete_existing_output(self):
        out = self.root / "comparison.json"
        out.write_text("previous", encoding="utf-8")
        completed = self._run("--out", out, "--help")
        self.assertEqual(completed.returncode, 0)
        self.assertEqual(out.read_text(encoding="utf-8"), "previous")


if __name__ == "__main__":
    unittest.main()
