"""Behaviour tests for LIDC reading-session matching and reference export.

Geometry fixtures use synthetic DICOM-free metadata; the XML fixtures are
hand-built from the TCIA LIDC annotation schema. They prove the mapping and
serialisation contracts, not real CT coordinates.
"""

from __future__ import annotations

import json
import math
import tempfile
import unittest
from pathlib import Path

import numpy as np

from nodule_ai.dicom_geometry import (
    DataError,
    mean_contour_center_lps,
    pixel_to_lps,
)
from nodule_ai.reference_nodules import (
    build_reference,
    export_reference,
    match_reading_sessions,
    select_case,
    write_reference,
)

STUDY_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.100000000000000000000000000001"
SERIES_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000001"
FRAME_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.300000000000000000000000000001"
SOP0 = "1.3.6.1.4.1.14519.5.2.1.6279.6001.400000000000000000000000000000"
SOP1 = "1.3.6.1.4.1.14519.5.2.1.6279.6001.500000000000000000000000000000"

IOP_IDENTITY = [1.0, 0.0, 0.0, 0.0, 1.0, 0.0]
SPACING = [1.0, 1.0]


# --- synthetic pylidc-like objects -------------------------------------------


class FakeContour:
    def __init__(self, z, points, inclusion=True):
        self.image_z_position = float(z)
        self.inclusion = bool(inclusion)
        self._points = np.array(points, dtype=float)
        # pylidc stores (x,y) text and reverses to (row,column) on to_matrix.
        self.coords = "\n".join(f"{int(col)},{int(row)}" for row, col in points)

    def to_matrix(self, include_k=False):
        assert include_k is False
        return self._points.copy()


class FakeAnnotation:
    def __init__(self, annotation_id, nodule_id, contours, diameter):
        self.id = annotation_id
        self._nodule_id = nodule_id
        self.contours = contours
        self.diameter = float(diameter)


class FakeScan:
    def __init__(self, annotations, clusters=None, series_uid=SERIES_UID, study_uid=STUDY_UID):
        self.study_instance_uid = study_uid
        self.series_instance_uid = series_uid
        self.annotations = annotations
        self._clusters = clusters
        self.pixel_spacing = 1.0
        self.slice_thickness = 1.0

    def cluster_annotations(self, **kwargs):
        return self._clusters if self._clusters is not None else [list(self.annotations)]


def nodule_xml(sessions, series_uid=SERIES_UID, study_uid=STUDY_UID) -> str:
    """Build an XML document from ``sessions``: list of list of nodule dicts."""
    parts = [
        '<?xml version="1.0" encoding="UTF-8"?>',
        '<LidcReadMessage xmlns="http://www.nih.gov">',
        " <ResponseHeader>",
        f"  <SeriesInstanceUid>{series_uid}</SeriesInstanceUid>",
        f"  <StudyInstanceUID>{study_uid}</StudyInstanceUID>",
        " </ResponseHeader>",
    ]
    for session in sessions:
        parts.append(" <readingSession>")
        parts.append("  <servicingRadiologistID>anonymous</servicingRadiologistID>")
        for nodule in session:
            parts.append("  <unblindedReadNodule>")
            parts.append(f"   <noduleID>{nodule['nodule_id']}</noduleID>")
            parts.append(
                "   <characteristics><subtlety>3</subtlety><malignancy>3</malignancy></characteristics>"
            )
            for roi in nodule["rois"]:
                parts.append("   <roi>")
                parts.append(f"    <imageZposition>{roi['z']}</imageZposition>")
                parts.append(f"    <imageSOP_UID>{roi['sop']}</imageSOP_UID>")
                parts.append(f"    <inclusion>{'TRUE' if roi.get('inclusion', True) else 'FALSE'}</inclusion>")
                for row, col in roi["points"]:
                    parts.append(f"    <edgeMap><xCoord>{col}</xCoord><yCoord>{row}</yCoord></edgeMap>")
                parts.append("   </roi>")
            parts.append("  </unblindedReadNodule>")
        parts.append(" </readingSession>")
    parts.append("</LidcReadMessage>")
    return "\n".join(parts)


def nodule(nodule_id, points, z=0.0, sop=SOP0, inclusion=True):
    return {"nodule_id": nodule_id, "rois": [{"z": z, "sop": sop, "points": points, "inclusion": inclusion}]}


def series_metadata(num_slices=2) -> dict:
    positions = [[0.0, 0.0, float(5 * i)] for i in range(num_slices)]
    return {
        "study_instance_uid": STUDY_UID,
        "series_instance_uid": SERIES_UID,
        "frame_of_reference_uid": FRAME_UID,
        "num_slices": num_slices,
        "rows": 512,
        "columns": 512,
        "pixel_spacing_mm": list(SPACING),
        "image_orientation_patient": list(IOP_IDENTITY),
        "slice_normal_lps": [0.0, 0.0, 1.0],
        "median_slice_spacing_mm": 5.0,
        "slice_spacing_mm": [5.0] * (num_slices - 1),
        "slice_thickness_mm": [1.0] * num_slices,
        "slice_positions_mm": [5.0 * i for i in range(num_slices)],
        "image_position_patient": positions,
        "sop_instance_uids": [SOP0, SOP1][:num_slices],
    }


def selection_doc(clusters) -> dict:
    return {
        "schema_version": "1",
        "source": {
            "collection_id": "lidc_idri",
            "collection_doi": "10.7937/K9/TCIA.2015.LO9QL9SX",
            "license": "CC BY 3.0",
            "idc_version": "v25",
            "idc_index_version": "0.13.0",
            "idc_index_data_version": "25.0.0",
        },
        "study_instance_uid": STUDY_UID,
        "series_instance_uid": SERIES_UID,
        "frame_of_reference_uid": FRAME_UID,
        "expected_instance_count": 2,
        "slice_thickness_mm": 1.0,
        "target_reference_id": clusters[0]["id"],
        "clusters": clusters,
    }


# --- geometry ----------------------------------------------------------------


class PixelToLpsTests(unittest.TestCase):
    def test_pixel_to_lps_respects_row_column_spacing_and_orientation(self):
        result = pixel_to_lps(4, 5, [10.0, 20.0, 30.0], [0.0, 1.0, 0.0, -1.0, 0.0, 0.0], [2.0, 3.0])
        self.assertEqual(len(result), 3)
        for got, expected in zip(result, [2.0, 35.0, 30.0]):
            self.assertLessEqual(abs(got - expected), 1e-6)

    def test_identity_orientation_keeps_lps_signs(self):
        result = pixel_to_lps(3, 7, [100.0, 200.0, 300.0], IOP_IDENTITY, SPACING)
        for got, expected in zip(result, [107.0, 203.0, 300.0]):
            self.assertLessEqual(abs(got - expected), 1e-6)

    def test_mean_contour_world_center_matches_affine_on_uniform_stack(self):
        metadata = series_metadata(num_slices=3)
        orientation = metadata["image_orientation_patient"]
        spacing = metadata["pixel_spacing_mm"]

        # Fractional-point contours on three slices, deliberately out of order.
        raw = [
            (np.array([[1.5, 2.0], [3.5, 4.0]]), metadata["image_position_patient"][2]),
            (np.array([[0.5, 0.5]]), metadata["image_position_patient"][0]),
            (np.array([[2.0, 3.0]]), metadata["image_position_patient"][1]),
        ]
        center = np.array(
            mean_contour_center_lps(
                [rc for rc, _ in raw], [pos for _, pos in raw], orientation, spacing
            )
        )

        # Independent expectation built from the raw DICOM definition:
        #   position = IPP + col*PixelSpacing[1]*IOP[:3] + row*PixelSpacing[0]*IOP[3:]
        points = []
        for rc, position in raw:
            for row, col in rc:
                x = position[0] + col * spacing[1] * orientation[0] + row * spacing[0] * orientation[3]
                y = position[1] + col * spacing[1] * orientation[1] + row * spacing[0] * orientation[4]
                z = position[2] + col * spacing[1] * orientation[2] + row * spacing[0] * orientation[5]
                points.append([x, y, z])
        expected = np.mean(points, axis=0)
        self.assertLessEqual(float(np.linalg.norm(center - expected)), 1e-6)

        # Reversing slice order keeps the same centre.
        reversed_center = np.array(
            mean_contour_center_lps(
                [rc for rc, _ in reversed(raw)],
                [pos for _, pos in reversed(raw)],
                orientation,
                spacing,
            )
        )
        self.assertLessEqual(float(np.linalg.norm(center - reversed_center)), 1e-9)


# --- session matching --------------------------------------------------------


class SessionMatchingTests(unittest.TestCase):
    def _scan(self, annotations):
        return FakeScan(annotations)

    def test_matches_annotations_to_distinct_sessions(self):
        a1 = FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 12.0)
        a2 = FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [11, 11]])], 11.0)
        scan = self._scan([a1, a2])
        xml = nodule_xml(
            [
                [nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)],
                [nodule("2", [[10, 10], [11, 11]], z=5.0, sop=SOP1)],
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "case.xml"
            path.write_text(xml, encoding="utf-8")
            session_ids, roi_bindings = match_reading_sessions(scan, path)
        self.assertEqual(len(set(session_ids.values())), 2)
        self.assertEqual(roi_bindings[1][0]["sop_instance_uid"], SOP0)
        self.assertEqual(roi_bindings[2][0]["sop_instance_uid"], SOP1)

    def test_ambiguous_xml_match_is_rejected(self):
        a1 = FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 12.0)
        scan = self._scan([a1])
        xml = nodule_xml(
            [
                [nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)],
                [nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)],
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "case.xml"
            path.write_text(xml, encoding="utf-8")
            with self.assertRaises(DataError):
                match_reading_sessions(scan, path)

    def test_unmatched_annotation_is_rejected(self):
        a1 = FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 12.0)
        a2 = FakeAnnotation(2, "2", [FakeContour(9.0, [[7, 7], [8, 8]])], 11.0)
        scan = self._scan([a1, a2])
        xml = nodule_xml([[nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)]])
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "case.xml"
            path.write_text(xml, encoding="utf-8")
            with self.assertRaises(DataError):
                match_reading_sessions(scan, path)


def _eligible_cluster(ids, session_ids, diameters, reference_id="nodule-abc"):
    return {
        "id": reference_id,
        "annotation_count": len(ids),
        "annotation_ids": list(ids),
        "session_ids": list(session_ids),
        "median_diameter_mm": float(np.median(diameters)),
        "measurements": [
            {"annotation_id": i, "session_id": s, "diameter_mm": d}
            for i, s, d in zip(ids, session_ids, diameters)
        ],
    }


class SelectCaseTests(unittest.TestCase):
    def _client(self, instance_count=2):
        return FakeIDC(
            [
                {
                    "SeriesInstanceUID": SERIES_UID,
                    "StudyInstanceUID": STUDY_UID,
                    "collection_id": "lidc_idri",
                    "source_DOI": "10.7937/K9/TCIA.2015.LO9QL9SX",
                    "license_short_name": "CC BY 3.0",
                    "Modality": "CT",
                    "instanceCount": instance_count,
                }
            ]
        )

    def _three_annotations(self):
        return [
            FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 11.0),
            FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [11, 11]])], 10.0),
            FakeAnnotation(3, "3", [FakeContour(10.0, [[20, 20], [21, 21]])], 9.0),
        ]

    def test_selects_exact_series_not_other_timepoint(self):
        other_series = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000009"
        annotations = [
            FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 11.0),
            FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [11, 11]])], 10.0),
            FakeAnnotation(3, "3", [FakeContour(10.0, [[20, 20], [21, 21]])], 12.0),
        ]
        # Same study, other timepoint: no eligible cluster and no matching XML.
        other_timepoint = FakeScan([], clusters=[], series_uid=other_series)
        scan = FakeScan(annotations, clusters=[annotations])
        xml = nodule_xml(
            [
                [nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)],
                [nodule("2", [[10, 10], [11, 11]], z=5.0, sop=SOP1)],
                [nodule("3", [[20, 20], [21, 21]], z=10.0, sop=SOP0)],
            ]
        )
        client = FakeIDC(
            [
                {
                    "SeriesInstanceUID": SERIES_UID,
                    "StudyInstanceUID": STUDY_UID,
                    "collection_id": "lidc_idri",
                    "source_DOI": "10.7937/K9/TCIA.2015.LO9QL9SX",
                    "license_short_name": "CC BY 3.0",
                    "Modality": "CT",
                    "instanceCount": 3,
                }
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            annotations_dir = Path(tmp)
            (annotations_dir / "case.xml").write_text(xml, encoding="utf-8")
            selection = select_case(client, [other_timepoint, scan], annotations_dir)
        self.assertEqual(selection["series_instance_uid"], SERIES_UID)
        self.assertEqual(selection["study_instance_uid"], STUDY_UID)
        self.assertEqual(len(selection["clusters"][0]["session_ids"]), 3)

    def test_three_annotations_from_two_sessions_are_ineligible(self):
        annotations = self._three_annotations()
        scan = FakeScan(annotations, clusters=[annotations])
        xml = nodule_xml(
            [
                [
                    nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0),
                    nodule("2", [[10, 10], [11, 11]], z=5.0, sop=SOP1),
                ],
                [nodule("3", [[20, 20], [21, 21]], z=10.0, sop=SOP0)],
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            annotations_dir = Path(tmp)
            (annotations_dir / "case.xml").write_text(xml, encoding="utf-8")
            with self.assertRaises(DataError):
                select_case(self._client(), [scan], annotations_dir)

    def test_median_diameter_boundary(self):
        # median == 10 with three distinct sessions is eligible
        annotations = [
            FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 12.0),
            FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [11, 11]])], 10.0),
            FakeAnnotation(3, "3", [FakeContour(10.0, [[20, 20], [21, 21]])], 8.0),
        ]
        scan = FakeScan(annotations, clusters=[annotations])
        xml = nodule_xml(
            [
                [nodule("1", [[1, 1], [2, 2]], z=0.0, sop=SOP0)],
                [nodule("2", [[10, 10], [11, 11]], z=5.0, sop=SOP1)],
                [nodule("3", [[20, 20], [21, 21]], z=10.0, sop=SOP0)],
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            annotations_dir = Path(tmp)
            (annotations_dir / "case.xml").write_text(xml, encoding="utf-8")
            selection = select_case(self._client(), [scan], annotations_dir)
        self.assertEqual(selection["series_instance_uid"], SERIES_UID)
        self.assertEqual(selection["expected_instance_count"], 2)
        self.assertEqual(len(selection["clusters"]), 1)
        self.assertEqual(selection["clusters"][0]["annotation_count"], 3)
        self.assertAlmostEqual(selection["clusters"][0]["median_diameter_mm"], 10.0)

        # median < 10 is ineligible
        below = [
            FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [2, 2]])], 9.9),
            FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [11, 11]])], 9.0),
            FakeAnnotation(3, "3", [FakeContour(10.0, [[20, 20], [21, 21]])], 8.0),
        ]
        scan_below = FakeScan(below, clusters=[below])
        with tempfile.TemporaryDirectory() as tmp:
            annotations_dir = Path(tmp)
            (annotations_dir / "case.xml").write_text(xml, encoding="utf-8")
            with self.assertRaises(DataError):
                select_case(self._client(), [scan_below], annotations_dir)


class FakeIDC:
    def __init__(self, rows):
        import pandas as pd

        self.index = pd.DataFrame(rows)

    def get_idc_version(self):
        return "v25"


# --- reference build / export ------------------------------------------------


class ReferenceTests(unittest.TestCase):
    def _fixture(self):
        a1 = FakeAnnotation(1, "1", [FakeContour(0.0, [[1, 1], [3, 3]])], 12.0)
        a2 = FakeAnnotation(2, "2", [FakeContour(5.0, [[10, 10], [12, 12]])], 10.0)
        a3 = FakeAnnotation(3, "3", [FakeContour(0.0, [[20, 20], [22, 22]])], 8.0)
        scan = FakeScan([a1, a2, a3])
        session_ids = {1: "reading-session-0", 2: "reading-session-1", 3: "reading-session-2"}
        roi_bindings = {
            1: [{"sop_instance_uid": SOP0, "z": 0.0, "inclusion": True, "coords": a1.contours[0].coords}],
            2: [{"sop_instance_uid": SOP1, "z": 5.0, "inclusion": True, "coords": a2.contours[0].coords}],
            3: [{"sop_instance_uid": SOP0, "z": 0.0, "inclusion": True, "coords": a3.contours[0].coords}],
        }
        clusters = [_eligible_cluster([1, 2, 3], ["reading-session-0", "reading-session-1", "reading-session-2"], [12.0, 10.0, 8.0])]
        return selection_doc(clusters), scan, session_ids, roi_bindings

    def test_reference_geometry_and_measurements(self):
        selection, scan, session_ids, roi_bindings = self._fixture()
        reference = build_reference(selection, series_metadata(), scan, session_ids, roi_bindings)
        self.assertEqual(reference["coordinate_system"], "DICOM_LPS_mm")
        self.assertEqual(reference["target_reference_id"], selection["target_reference_id"])
        self.assertEqual(len(reference["nodules"]), 1)
        nodule_doc = reference["nodules"][0]
        self.assertEqual(nodule_doc["annotation_count"], 3)
        self.assertEqual(len(nodule_doc["center_mm"]), 3)
        for value in nodule_doc["center_mm"]:
            self.assertTrue(math.isfinite(value))
        self.assertAlmostEqual(nodule_doc["diameter_mm"], 10.0)
        self.assertEqual(nodule_doc["annotation_ids"], [1, 2, 3])
        self.assertEqual(
            [m["annotation_id"] for m in nodule_doc["measurements"]], [1, 2, 3]
        )
        self.assertEqual(reference["geometry"]["num_slices"], 2)

    def test_missing_roi_sop_is_rejected(self):
        selection, scan, session_ids, roi_bindings = self._fixture()
        roi_bindings[2] = [{"sop_instance_uid": "1.2.3.4", "z": 5.0, "inclusion": True, "coords": scan.annotations[1].contours[0].coords}]
        with self.assertRaises(DataError):
            build_reference(selection, series_metadata(), scan, session_ids, roi_bindings)

    def test_nonfinite_or_nonpositive_measurement_is_rejected(self):
        selection, scan, session_ids, roi_bindings = self._fixture()
        scan.annotations[1].diameter = 0.0
        with self.assertRaises(DataError):
            build_reference(selection, series_metadata(), scan, session_ids, roi_bindings)

    def test_reference_is_byte_identical_on_rerun_and_permuted_inputs(self):
        selection, scan, session_ids, roi_bindings = self._fixture()
        first = build_reference(selection, series_metadata(), scan, session_ids, roi_bindings)

        permuted_selection, permuted_scan, permuted_sessions, permuted_bindings = self._fixture()
        permuted_selection["clusters"].reverse()
        for cluster in permuted_selection["clusters"]:
            cluster["annotation_ids"].reverse()
            cluster["session_ids"].reverse()
            cluster["measurements"].reverse()
        permuted_bindings = {k: permuted_bindings[k] for k in reversed(list(permuted_bindings))}
        permuted_sessions = {k: permuted_sessions[k] for k in reversed(list(permuted_sessions))}
        second = build_reference(permuted_selection, series_metadata(), permuted_scan, permuted_sessions, permuted_bindings)

        self.assertEqual(
            json.dumps(first, sort_keys=True, allow_nan=False),
            json.dumps(second, sort_keys=True, allow_nan=False),
        )

    def test_failed_export_does_not_leave_current_success(self):
        selection, scan, session_ids, roi_bindings = self._fixture()
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "reference.json"
            unrelated = Path(tmp) / "input.dcm"
            unrelated.write_bytes(b"keep me")
            write_reference(build_reference(selection, series_metadata(), scan, session_ids, roi_bindings), out)
            self.assertTrue(out.exists())

            broken_selection, broken_scan, broken_sessions, broken_bindings = self._fixture()
            broken_scan.annotations[0].diameter = float("nan")
            with self.assertRaises(DataError):
                export_reference(
                    broken_selection,
                    series_metadata(),
                    broken_scan,
                    broken_sessions,
                    broken_bindings,
                    out,
                )
            self.assertFalse(out.exists())
            self.assertEqual(unrelated.read_bytes(), b"keep me")


if __name__ == "__main__":
    unittest.main()
