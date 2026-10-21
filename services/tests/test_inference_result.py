"""Consumer-visible candidate rules and safe publication for result.json."""

from __future__ import annotations

import unittest

from nodule_ai.inference_result import (
    candidates_from_boxes,
    error_result,
    validate_success,
    write_result,
)


def success_document(**overrides) -> dict:
    document = {
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
        "input": {"num_slices": 158, "spacing_mm": [0.698, 0.698, 2.0], "size_voxels": [512, 512, 158]},
        "candidates": [
            {
                "id": "c1",
                "center_mm": [-93.4, 60.4, 1523.5],
                "size_mm": [11.0, 9.5, 8.0],
                "score": 0.87,
            }
        ],
        "timing_s": {"load": 1.5, "inference": 20.0, "total": 22.0},
        "resources": {"device": "cpu", "peak_gpu_mem_mb": 0, "peak_rss_mb": 1234.5},
    }
    document.update(overrides)
    return document


class CandidateTests(unittest.TestCase):
    def test_threshold_boundary_and_small_box_are_preserved(self):
        boxes = [
            [0.0, 0.0, 0.0, 3.0, 3.0, 3.0],  # 3 mm extent, well below 10 mm
            [10.0, 20.0, 30.0, 11.0, 9.5, 8.0],
            [-5.0, -6.0, -7.0, 12.0, 12.0, 12.0],
        ]
        scores = [0.49, 0.5, 0.9]
        candidates = candidates_from_boxes(boxes, scores, 0.5)
        self.assertEqual([c["id"] for c in candidates], ["c1", "c2"])
        self.assertEqual(candidates[0]["center_mm"], [10.0, 20.0, 30.0])
        self.assertEqual(candidates[0]["size_mm"], [11.0, 9.5, 8.0])
        self.assertEqual(candidates[0]["score"], 0.5)
        self.assertEqual(candidates[1]["center_mm"], [-5.0, -6.0, -7.0])
        self.assertEqual(candidates[1]["score"], 0.9)

    def test_small_box_below_ten_mm_is_kept(self):
        candidates = candidates_from_boxes([[0.0, 0.0, 0.0, 3.0, 3.0, 3.0]], [1.0], 0.5)
        self.assertEqual(len(candidates), 1)
        self.assertEqual(candidates[0]["size_mm"], [3.0, 3.0, 3.0])

    def test_empty_prediction_is_valid(self):
        self.assertEqual(candidates_from_boxes([], [], 0.5), [])
        document = success_document(candidates=[])
        validate_success(document)

    def test_malformed_prediction_fails_before_filter(self):
        cases = [
            ([[1.0, 2.0, 3.0, 4.0, 5.0]], [0.0], 0.5),  # wrong row length, below threshold
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [0.0, 0.1], 0.5),  # count mismatch
            ([[1.0, 2.0, 3.0, 4.0, 5.0, float("nan")]], [0.0], 0.5),
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [float("inf")], 0.5),
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [-0.1], 0.5),
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [1.1], 0.5),
            ([[1.0, 2.0, 3.0, 0.0, 5.0, 6.0]], [0.0], 0.5),  # zero extent
            ([[1.0, 2.0, 3.0, -4.0, 5.0, 6.0]], [0.0], 0.5),  # negative extent
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [0.0], float("nan")),
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [0.0], 1.5),
            ([[1.0, 2.0, 3.0, 4.0, 5.0, 6.0]], [0.0], -0.1),
        ]
        for boxes, scores, threshold in cases:
            with self.subTest(boxes=boxes, scores=scores, threshold=threshold):
                with self.assertRaises(ValueError):
                    candidates_from_boxes(boxes, scores, threshold)


class SuccessValidationTests(unittest.TestCase):
    def test_accepts_a_complete_document(self):
        validate_success(success_document())

    def test_rejects_wrong_identity_and_metrics(self):
        mutations = [
            {"schema_version": 1},
            {"coordinate_system": "RAS"},
            {"series_instance_uid": "not-a-uid"},
            {"frame_of_reference_uid": ""},
            {"candidates": [{"id": "c1", "center_mm": [0, 0], "size_mm": [1, 1, 1], "score": 0.5}]},
            {"candidates": [{"id": "c1", "center_mm": [0, 0, 0], "size_mm": [1, 1, 1], "score": 0.5},
                            {"id": "c1", "center_mm": [1, 1, 1], "size_mm": [1, 1, 1], "score": 0.5}]},
            {"candidates": [{"id": "c1", "center_mm": [0, 0, 0], "size_mm": [1, 1, 0], "score": 0.5}]},
            {"candidates": [{"id": "c1", "center_mm": [0, 0, 0], "size_mm": [1, 1, 1], "score": float("nan")}]},
            {"input": {"num_slices": 0, "spacing_mm": [1, 1, 1], "size_voxels": [4, 4, 4]}},
            {"input": {"num_slices": 4, "spacing_mm": [0, 1, 1], "size_voxels": [4, 4, 4]}},
            {"input": {"num_slices": 4, "spacing_mm": [1, 1, 1], "size_voxels": [4, 4, 0]}},
            {"model": {"bundle": "b", "bundle_version": "0.6.11", "monai_version": "1.4.0",
                       "torch_version": "2.4.1+cpu", "affine_lps_to_ras": True}},
            {"model": {"bundle": "b", "bundle_version": "0.6.11", "monai_version": "1.4.0",
                       "torch_version": "2.4.1+cpu", "affine_lps_to_ras": "yes", "score_threshold": 0.5}},
            {"model": {"bundle": "b", "bundle_version": "0.6.11", "monai_version": "1.4.0",
                       "torch_version": "2.4.1+cpu", "affine_lps_to_ras": True, "score_threshold": 2.0}},
            {"timing_s": {"load": -1.0, "inference": 1.0, "total": 2.0}},
            {"timing_s": {"load": 1.0, "inference": 1.0, "total": 1.0}},
            {"timing_s": {"load": 1.0, "inference": 1.0, "total": float("inf")}},
            {"resources": {"device": "cpu", "peak_gpu_mem_mb": -1, "peak_rss_mb": 10.0}},
            {"resources": {"device": "", "peak_gpu_mem_mb": 0, "peak_rss_mb": 10.0}},
            {"resources": {"device": "cpu", "peak_gpu_mem_mb": 0}},
        ]
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                document = success_document(**mutation)
                with self.assertRaises(ValueError):
                    validate_success(document)

    def test_total_must_cover_load_plus_inference(self):
        document = success_document(timing_s={"load": 1.0, "inference": 2.0, "total": 2.5})
        with self.assertRaises(ValueError):
            validate_success(document)
        validate_success(success_document(timing_s={"load": 1.0, "inference": 2.0, "total": 3.0}))
        # documented timer precision tolerance
        validate_success(success_document(timing_s={"load": 1.0, "inference": 2.0, "total": 2.999999}))


class PublicationTests(unittest.TestCase):
    def test_error_result_envelope(self):
        error = error_result("MODEL_OOM", "model ran out of memory")
        self.assertEqual(sorted(error), ["error", "schema_version"])
        self.assertEqual(error["schema_version"], "1")
        self.assertEqual(error["error"], {"code": "MODEL_OOM", "message": "model ran out of memory"})

    def test_writes_strict_json_atomically(self):
        import json
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "result.json"
            document = success_document()
            write_result(document, out)
            self.assertEqual(json.loads(out.read_text(encoding="utf-8")), document)

    def test_nan_is_rejected_and_no_partial_file_remains(self):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "result.json"
            document = success_document()
            document["candidates"][0]["score"] = float("nan")
            with self.assertRaises(ValueError):
                write_result(document, out)
            self.assertFalse(out.exists())
            self.assertEqual(list(Path(tmp).iterdir()), [])

    def test_failed_atomic_replace_preserves_no_partial_json(self):
        import os
        import tempfile
        from pathlib import Path
        from unittest import mock

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "result.json"
            with mock.patch("nodule_ai.inference_result.os.replace", side_effect=OSError("boom")):
                with self.assertRaises(OSError):
                    write_result(success_document(), out)
            self.assertFalse(out.exists())
            self.assertEqual(list(Path(tmp).iterdir()), [])

    def test_replace_failure_does_not_touch_unrelated_files(self):
        import tempfile
        from pathlib import Path
        from unittest import mock

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "result.json"
            sentinel = Path(tmp) / "keep.txt"
            sentinel.write_text("keep", encoding="utf-8")
            with mock.patch("nodule_ai.inference_result.os.replace", side_effect=OSError("boom")):
                with self.assertRaises(OSError):
                    write_result(success_document(), out)
            self.assertEqual(sentinel.read_text(encoding="utf-8"), "keep")
            self.assertFalse(out.exists())


if __name__ == "__main__":
    unittest.main()
