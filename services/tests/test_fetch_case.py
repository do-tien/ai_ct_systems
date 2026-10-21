"""Behaviour tests for the frozen-selection LIDC CT fetch and geometry validation.

Fixtures are synthetic DICOM headers written to temp directories; they exercise
the validator and the frozen-selection download path, not real IDC content.
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import importlib.util

HAS_PYDICOM = importlib.util.find_spec("pydicom") is not None
if HAS_PYDICOM:
    import pydicom
    from pydicom.dataset import Dataset, FileMetaDataset
    from pydicom.uid import CTImageStorage, ExplicitVRLittleEndian

from module_ai.dicom_geometry import DataError, GeometryError, read_series_metadata
from module_ai.fetch_case import fetch_case

STUDY_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.100000000000000000000000000001"
SERIES_A = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000001"
SERIES_B = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000002"
FRAME_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.300000000000000000000000000001"
PATIENT_ID = "LIDC-IDRI-0001"


def _sop(sequence: int) -> str:
    return f"1.3.6.1.4.1.14519.5.2.1.6279.6001.4{sequence:d}"


def write_slice(
    path: Path,
    *,
    sop_instance_uid: str,
    series_uid: str = SERIES_A,
    study_uid: str = STUDY_UID,
    frame_uid: str = FRAME_UID,
    modality: str = "CT",
    z_mm: float = 0.0,
    ipp_xy: tuple[float, float] = (0.0, 0.0),
    iop: tuple[float, ...] = (1.0, 0.0, 0.0, 0.0, 1.0, 0.0),
    pixel_spacing: tuple[float, float] = (1.0, 1.0),
    rows: int = 4,
    columns: int = 4,
    thickness: float = 1.0,
    number_of_frames: int | None = None,
) -> Path:
    ds = Dataset()
    ds.file_meta = FileMetaDataset()
    ds.file_meta.MediaStorageSOPClassUID = CTImageStorage
    ds.file_meta.MediaStorageSOPInstanceUID = sop_instance_uid
    ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    ds.SOPClassUID = CTImageStorage
    ds.SOPInstanceUID = sop_instance_uid
    ds.Modality = modality
    ds.PatientID = PATIENT_ID
    ds.StudyInstanceUID = study_uid
    ds.SeriesInstanceUID = series_uid
    ds.FrameOfReferenceUID = frame_uid
    ds.ImageOrientationPatient = list(iop)
    ds.ImagePositionPatient = [float(ipp_xy[0]), float(ipp_xy[1]), float(z_mm)]
    ds.PixelSpacing = [float(pixel_spacing[0]), float(pixel_spacing[1])]
    ds.SliceThickness = float(thickness)
    ds.Rows = rows
    ds.Columns = columns
    ds.InstanceNumber = 1
    if number_of_frames is not None:
        ds.NumberOfFrames = number_of_frames
    path.parent.mkdir(parents=True, exist_ok=True)
    ds.save_as(path, write_like_original=False)
    return path


def write_series(directory: Path, *, z_positions, series_uid=SERIES_A, **kwargs) -> list[Path]:
    paths = []
    for index, z in enumerate(z_positions):
        paths.append(
            write_slice(
                directory / f"slice-{index:03d}.dcm",
                sop_instance_uid=_sop(index),
                series_uid=series_uid,
                z_mm=z,
                **kwargs,
            )
        )
    return paths


def selection_for(**overrides) -> dict:
    selection = {
        "schema_version": "1",
        "source": {
            "collection_id": "lidc_idri",
            "idc_version": "v25",
            "idc_index_data_version": "25.0.0",
        },
        "study_instance_uid": STUDY_UID,
        "series_instance_uid": SERIES_A,
        "expected_instance_count": 3,
        "target_reference_id": "cluster-0001",
        "clusters": [],
    }
    selection.update(overrides)
    return selection


class FakeIDCClient:
    """Records the exact SeriesInstanceUID requested and materialises fixtures."""

    def __init__(self, builder):
        self._builder = builder
        self.requested: list[str] = []

    def download_dicom_series(self, seriesInstanceUID, downloadDir, dirTemplate="%SeriesInstanceUID", **kwargs):
        self.requested.append(seriesInstanceUID)
        target = Path(downloadDir) / dirTemplate.replace("%SeriesInstanceUID", seriesInstanceUID)
        self._builder(seriesInstanceUID, target)


@unittest.skipUnless(HAS_PYDICOM, "requires the data/geometry environment (pydicom)")
class ReadSeriesMetadataTests(unittest.TestCase):
    def test_accepts_uniform_stack_and_sorts_by_position(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[2.0, 0.0, 1.0])
            metadata = read_series_metadata(directory)
            self.assertEqual(metadata["num_slices"], 3)
            self.assertEqual(metadata["series_instance_uid"], SERIES_A)
            self.assertEqual(metadata["frame_of_reference_uid"], FRAME_UID)
            self.assertEqual(metadata["pixel_spacing_mm"], [1.0, 1.0])
            self.assertAlmostEqual(metadata["median_slice_spacing_mm"], 1.0, places=6)
            # ordered by IPP projection, not filename
            self.assertEqual(metadata["slice_positions_mm"], [0.0, 1.0, 2.0])
            self.assertEqual(metadata["sop_instance_uids"], [_sop(1), _sop(2), _sop(0)])

    def test_rejects_mixed_series_or_frame(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(directory / "other-series.dcm", sop_instance_uid=_sop(9), series_uid=SERIES_B, z_mm=2.0)
            with self.assertRaises(DataError):
                read_series_metadata(directory)

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(
                directory / "other-frame.dcm",
                sop_instance_uid=_sop(9),
                frame_uid="1.3.6.1.4.1.14519.5.2.1.6279.6001.999999999999999999999999999999",
                z_mm=2.0,
            )
            with self.assertRaises(DataError):
                read_series_metadata(directory)

    def test_rejects_duplicate_sop(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(directory / "dup.dcm", sop_instance_uid=_sop(0), z_mm=2.0)
            with self.assertRaises(DataError):
                read_series_metadata(directory)

    def test_rejects_gap_despite_small_slice_thickness(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0, 5.0], thickness=1.0)
            with self.assertRaises(DataError):
                read_series_metadata(directory)

    def test_rejects_thickness_over_limit(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 3.0], thickness=3.0)
            with self.assertRaises(DataError):
                read_series_metadata(directory)

    def test_rejects_non_ct_or_multiframe(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0], modality="MR")
            with self.assertRaises(DataError):
                read_series_metadata(directory)

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0], number_of_frames=4)
            with self.assertRaises(DataError):
                read_series_metadata(directory)

    def test_rejects_in_plane_origin_drift(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(directory / "drift.dcm", sop_instance_uid=_sop(5), z_mm=2.0, ipp_xy=(0.5, 0.0))
            with self.assertRaises(DataError):
                read_series_metadata(directory)


@unittest.skipUnless(HAS_PYDICOM, "requires the data/geometry environment (pydicom)")
class FetchCaseTests(unittest.TestCase):
    def _builder(self, series_uid, target):
        if series_uid == SERIES_A:
            write_series(target, z_positions=[0.0, 1.0, 2.0])
        elif series_uid == SERIES_B:
            write_series(target, z_positions=[0.0, 1.0, 2.0], series_uid=SERIES_B)

    def test_rejects_missing_instance_against_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            client = FakeIDCClient(self._builder)
            selection = selection_for(expected_instance_count=4)  # fixture only writes 3
            with self.assertRaises(DataError):
                fetch_case(selection, out, client=client)
            self.assertFalse((out / "case-manifest.json").exists())

    def test_fetches_exact_uid_not_patient_timepoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            client = FakeIDCClient(self._builder)
            selection = selection_for(series_instance_uid=SERIES_B, expected_instance_count=3)
            manifest = fetch_case(selection, out, client=client)

            self.assertEqual(client.requested, [SERIES_B])
            self.assertEqual(manifest["series_instance_uid"], SERIES_B)
            self.assertEqual(manifest["study_instance_uid"], STUDY_UID)
            self.assertEqual(manifest["num_slices"], 3)
            self.assertEqual(len(manifest["sop_instance_uids"]), 3)
            self.assertTrue((out / "case-manifest.json").exists())

            written = json.loads((out / "case-manifest.json").read_text(encoding="utf-8"))
            self.assertEqual(written["series_instance_uid"], SERIES_B)
            self.assertEqual(written["schema_version"], "1")

    def test_cache_hit_skips_download_but_validates(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            client = FakeIDCClient(self._builder)
            selection = selection_for(expected_instance_count=3)
            fetch_case(selection, out, client=client)
            client.requested.clear()
            manifest = fetch_case(selection, out, client=client)
            self.assertEqual(client.requested, [])
            self.assertEqual(manifest["num_slices"], 3)

    def test_rejects_incomplete_selection(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            selection = selection_for()
            del selection["series_instance_uid"]
            with self.assertRaises(DataError):
                fetch_case(selection, out, client=FakeIDCClient(self._builder))

    def test_failed_fetch_removes_stale_manifest(self):
        from module_ai.fetch_case import MANIFEST_NAME, main

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            (out / MANIFEST_NAME).write_text('{"stale": true}', encoding="utf-8")
            # Pre-seeded cache with unusable content: no network, validation fails.
            cache = out / "dicom" / SERIES_A
            cache.mkdir(parents=True)
            (cache / "junk.dcm").write_bytes(b"not a dicom")

            selection_path = out / "selection.json"
            selection_path.write_text(json.dumps(selection_for(expected_instance_count=3)), encoding="utf-8")

            code = main(["--selection", str(selection_path), "--out", str(out)])
            self.assertEqual(code, 2)
            self.assertFalse((out / MANIFEST_NAME).exists())


@unittest.skipUnless(HAS_PYDICOM, "requires the data/geometry environment (pydicom)")
class GeometryErrorCodeTests(unittest.TestCase):
    """Pin the stable error codes the LC1-05/06 consumers rely on."""

    def _code(self, directory: Path) -> str:
        with self.assertRaises(GeometryError) as raised:
            read_series_metadata(directory)
        return raised.exception.code

    def test_missing_frame_uid_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0], frame_uid="")
            self.assertEqual(self._code(directory), "INVALID_UID")

    def test_identity_mismatch_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(
                directory / "other-study.dcm",
                sop_instance_uid=_sop(9),
                study_uid="1.3.6.1.4.1.14519.5.2.1.6279.6001.999999999999999999999999999999",
                z_mm=2.0,
            )
            self.assertEqual(self._code(directory), "UID_MISMATCH")

    def test_missing_numeric_tag_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            paths = write_series(directory, z_positions=[0.0, 1.0])
            ds = pydicom.dcmread(paths[0])
            del ds.PixelSpacing
            ds.save_as(paths[0], write_like_original=False)
            self.assertEqual(self._code(directory), "INVALID_GEOMETRY")

    def test_duplicate_position_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(directory / "dupe-pos.dcm", sop_instance_uid=_sop(5), z_mm=1.0)
            self.assertEqual(self._code(directory), "DUPLICATE_POSITION")

    def test_nonuniform_and_too_large_spacing_codes(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0, 2.5])
            self.assertEqual(self._code(directory), "NONUNIFORM_SPACING")

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 4.0], thickness=1.0)
            self.assertEqual(self._code(directory), "SLICE_SPACING_TOO_LARGE")

    def test_thickness_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0], thickness=3.0)
            self.assertEqual(self._code(directory), "SLICE_TOO_THICK")

    def test_tilt_and_modality_codes(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0])
            write_slice(directory / "drift.dcm", sop_instance_uid=_sop(5), z_mm=2.0, ipp_xy=(0.5, 0.0))
            self.assertEqual(self._code(directory), "UNSUPPORTED_GANTRY_TILT")

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0], modality="MR")
            self.assertEqual(self._code(directory), "NOT_CT")

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[0.0, 1.0], number_of_frames=4)
            self.assertEqual(self._code(directory), "UNSUPPORTED_CT")

    def test_mixed_non_uid_headers_keep_their_field_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            paths = write_series(directory, z_positions=[0.0, 1.0])
            ds = pydicom.dcmread(paths[1])
            ds.Rows = ds.Rows + 1
            ds.save_as(paths[1], write_like_original=False)
            self.assertEqual(self._code(directory), "INVALID_GEOMETRY")

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            paths = write_series(directory, z_positions=[0.0, 1.0])
            ds = pydicom.dcmread(paths[1])
            ds.Modality = "MR"
            ds.save_as(paths[1], write_like_original=False)
            self.assertEqual(self._code(directory), "NOT_CT")

    def test_ordered_files_track_sop_order(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_series(directory, z_positions=[2.0, 0.0, 1.0])
            metadata = read_series_metadata(directory)
            self.assertIsInstance(metadata["ordered_files"][0], Path)
            self.assertEqual(len(metadata["ordered_files"]), 3)
            names = [path.name for path in metadata["ordered_files"]]
            self.assertEqual(names, ["slice-001.dcm", "slice-002.dcm", "slice-000.dcm"])


if __name__ == "__main__":
    unittest.main()
