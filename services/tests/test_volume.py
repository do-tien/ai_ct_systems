"""Volume decode/ordering/NIfTI tests with real synthetic DICOM pixel data."""

from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path

import numpy as np

HAS_PYDICOM = importlib.util.find_spec("pydicom") is not None
if HAS_PYDICOM:
    import pydicom
    from pydicom.dataset import Dataset, FileMetaDataset
    from pydicom.uid import CTImageStorage, ExplicitVRLittleEndian

from nodule_ai.dicom_geometry import lps_to_ras_affine
from nodule_ai.errors import GeometryError

HAS_SITK = importlib.util.find_spec("SimpleITK") is not None
if HAS_SITK:
    import SimpleITK as sitk

    from nodule_ai.volume import read_ct_volume, validate_decoded_pixels, write_nifti

STUDY_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.100000000000000000000000000001"
SERIES_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.200000000000000000000000000001"
FRAME_UID = "1.3.6.1.4.1.14519.5.2.1.6279.6001.300000000000000000000000000001"

IOP_IDENTITY = [1.0, 0.0, 0.0, 0.0, 1.0, 0.0]
IOP_OBLIQUE = [0.6, 0.8, 0.0, -0.8, 0.6, 0.0]


def _sop(sequence: int) -> str:
    return f"1.3.6.1.4.1.14519.5.2.1.6279.6001.4{sequence:d}"


def write_pixel_slice(
    path: Path,
    *,
    sop: str,
    values: np.ndarray,
    z: float,
    ipp_xy=(0.0, 0.0),
    iop=IOP_IDENTITY,
    spacing=(0.7, 0.7),
    thickness=1.0,
    slope: float = 1.0,
    intercept: float = 0.0,
    instance_number: int = 1,
    corrupt: bool = False,
) -> Path:
    rows, columns = values.shape
    ds = Dataset()
    ds.file_meta = FileMetaDataset()
    ds.file_meta.MediaStorageSOPClassUID = CTImageStorage
    ds.file_meta.MediaStorageSOPInstanceUID = sop
    ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    ds.SOPClassUID = CTImageStorage
    ds.SOPInstanceUID = sop
    ds.Modality = "CT"
    ds.PatientID = "LIDC-IDRI-9999"
    ds.StudyInstanceUID = STUDY_UID
    ds.SeriesInstanceUID = SERIES_UID
    ds.FrameOfReferenceUID = FRAME_UID
    ds.ImageOrientationPatient = list(iop)
    ds.ImagePositionPatient = [float(ipp_xy[0]), float(ipp_xy[1]), float(z)]
    ds.PixelSpacing = [float(spacing[0]), float(spacing[1])]
    ds.SliceThickness = float(thickness)
    ds.Rows = rows
    ds.Columns = columns
    ds.InstanceNumber = instance_number
    ds.BitsAllocated = 32 if corrupt else 16
    ds.BitsStored = 16
    ds.HighBit = 15
    ds.PixelRepresentation = 1
    ds.SamplesPerPixel = 1
    ds.PhotometricInterpretation = "MONOCHROME2"
    ds.RescaleSlope = str(slope)
    ds.RescaleIntercept = str(intercept)
    # A damaged payload: the declared 32-bit allocation cannot be decoded as the
    # 16-bit data actually stored.
    ds.PixelData = values.astype("<i2").tobytes()
    path.parent.mkdir(parents=True, exist_ok=True)
    ds.save_as(path, write_like_original=False)
    return path


def make_series(directory: Path, planes, *, iop=IOP_IDENTITY, spacing=(0.7, 0.7), slope=1.0, intercept=0.0, corrupt=False):
    """``planes`` is a list of (z, ndarray, instance_number, filename)."""
    paths = []
    for index, (z, values, instance_number, name) in enumerate(planes):
        paths.append(
            write_pixel_slice(
                directory / name,
                sop=_sop(index),
                values=values,
                z=z,
                iop=iop,
                spacing=spacing,
                slope=slope,
                intercept=intercept,
                instance_number=instance_number,
                corrupt=corrupt and index == len(planes) - 1,
            )
        )
    return paths


@unittest.skipUnless(HAS_SITK and HAS_PYDICOM, "requires the geometry environment (SimpleITK + pydicom)")
class VolumeDecodeTests(unittest.TestCase):
    def test_volume_orders_pixels_by_ipp(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            plane0 = np.full((4, 5), -100, dtype=np.int16)
            plane1 = np.full((4, 5), 200, dtype=np.int16)
            plane2 = np.full((4, 5), 700, dtype=np.int16)
            # Filenames and InstanceNumber deliberately contradict slice order.
            make_series(
                directory,
                [
                    (2.0, plane2, 1, "a-last.dcm"),
                    (0.0, plane0, 99, "z-first.dcm"),
                    (1.0, plane1, 50, "m-middle.dcm"),
                ],
            )
            image, metadata = read_ct_volume(directory)
            array = sitk.GetArrayFromImage(image)  # (z, y, x)
            self.assertEqual(array.shape, (3, 4, 5))
            self.assertTrue(np.all(array[0] == -100))
            self.assertTrue(np.all(array[1] == 200))
            self.assertTrue(np.all(array[2] == 700))
            for k, expected_xy in enumerate([(0.0, 0.0), (0.0, 0.0), (0.0, 0.0)]):
                origin = image.TransformIndexToPhysicalPoint((0, 0, k))
                self.assertAlmostEqual(origin[2], float(k), places=6)
            self.assertEqual(metadata["completeness"], "not_verified")
            self.assertEqual(len(metadata["ordered_files"]), 3)

    def test_ct_rescale_to_hu(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            raw = np.array([[-2000, -1000], [0, 1000]], dtype=np.int16)
            write_pixel_slice(directory / "a.dcm", sop=_sop(0), values=raw, z=0.0, slope=2.0, intercept=-1000.0)
            write_pixel_slice(directory / "b.dcm", sop=_sop(1), values=raw, z=1.0, slope=2.0, intercept=-1000.0)
            image, _ = read_ct_volume(directory)
            hu = sitk.GetArrayFromImage(image)
            expected = raw.astype(np.float32) * 2.0 - 1000.0
            np.testing.assert_allclose(hu[0], expected, rtol=0, atol=1e-4)

    def test_inventory_detects_boundary_and_regular_loss(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            plane = np.zeros((3, 3), dtype=np.int16)
            paths = make_series(directory, [(float(z), plane, z, f"s{z}.dcm") for z in range(4)])
            present = sorted(p.stem for p in paths)

            # Every other slice missing: remaining geometry is still regular.
            trimmed = Path(tmp) / "trimmed"
            trimmed.mkdir()
            for path in (paths[0], paths[2]):
                trimmed.joinpath(path.name).write_bytes(path.read_bytes())
            with self.assertRaises(GeometryError) as raised:
                read_ct_volume(trimmed, expected_sop_uids=[_sop(0), _sop(1), _sop(2), _sop(3)])
            self.assertEqual(raised.exception.code, "INCOMPLETE_SERIES")

            # Boundary slice missing.
            boundary = Path(tmp) / "boundary"
            boundary.mkdir()
            for path in paths[1:]:
                boundary.joinpath(path.name).write_bytes(path.read_bytes())
            with self.assertRaises(GeometryError) as raised:
                read_ct_volume(boundary, expected_sop_uids=[_sop(0), _sop(1), _sop(2), _sop(3)])
            self.assertEqual(raised.exception.code, "INCOMPLETE_SERIES")

            # Without inventory the tool must not claim completeness.
            _, metadata = read_ct_volume(trimmed)
            self.assertEqual(metadata["completeness"], "not_verified")

            # With the matching inventory it is verified.
            _, metadata = read_ct_volume(directory, expected_sop_uids=[_sop(i) for i in range(4)])
            self.assertEqual(metadata["completeness"], "verified")
            self.assertEqual(len(present), 4)

    def test_corrupt_pixels_never_produce_nifti(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            plane = np.zeros((4, 4), dtype=np.int16)
            make_series(directory, [(0.0, plane, 1, "a.dcm"), (1.0, plane, 2, "b.dcm")], corrupt=True)
            out = Path(tmp) / "out.nii.gz"
            with self.assertRaises(GeometryError) as raised:
                image, _ = read_ct_volume(directory)
                write_nifti(image, out)
            self.assertIn(raised.exception.code, {"PIXEL_DECODE_FAILED", "INVALID_PIXEL_DATA"})
            self.assertFalse(out.exists())

    def test_nonfinite_pixels_are_rejected(self):
        bad = sitk.GetImageFromArray(np.array([[[np.nan]]], dtype=np.float32))
        with self.assertRaises(GeometryError) as raised:
            validate_decoded_pixels(bad)
        self.assertEqual(raised.exception.code, "INVALID_PIXEL_DATA")


@unittest.skipUnless(HAS_SITK and HAS_PYDICOM, "requires the geometry environment (SimpleITK + pydicom)")
class NiftiTests(unittest.TestCase):
    def test_nifti_oblique_anisotropic_affine_and_pixels(self):
        import nibabel as nib

        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            spacing = (0.7, 1.3)
            planes = []
            for k in range(3):
                values = np.full((4, 6), -50 + k, dtype=np.int16)
                planes.append((float(k) * 1.2, values, k + 1, f"slice-{k}.dcm"))
            make_series(directory, planes, iop=IOP_OBLIQUE, spacing=spacing)
            image, _ = read_ct_volume(directory)

            out = Path(tmp) / "volume.nii.gz"
            write_nifti(image, out)
            self.assertTrue(out.exists())

            nii = nib.load(str(out))
            sitk_array = sitk.GetArrayFromImage(image)  # (z, y, x)
            np.testing.assert_array_equal(np.asarray(nii.dataobj), np.transpose(sitk_array, (2, 1, 0)))

            itk_affine = np.eye(4)
            itk_affine[:3, :3] = np.array(image.GetDirection()).reshape(3, 3) @ np.diag([float(v) for v in image.GetSpacing()])
            itk_affine[:3, 3] = image.GetOrigin()
            expected = lps_to_ras_affine(itk_affine)
            self.assertLessEqual(float(np.max(np.abs(nii.affine - expected))), 1e-6)

            size = image.GetSize()
            # World-coordinate agreement at all corners.
            for i in range(2):
                for j in range(2):
                    for k in range(2):
                        index = (i * (size[0] - 1), j * (size[1] - 1), k * (size[2] - 1))
                        lps = np.array(image.TransformIndexToPhysicalPoint(index))
                        ras = np.array([-lps[0], -lps[1], lps[2], 1.0])
                        recovered = nii.affine @ np.array([index[0], index[1], index[2], 1.0])
                        self.assertLessEqual(float(np.linalg.norm(ras - recovered)), 1e-3)

    def test_failed_write_removes_only_its_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            plane = np.zeros((4, 4), dtype=np.int16)
            make_series(directory, [(0.0, plane, 1, "a.dcm"), (1.0, plane, 2, "b.dcm")])
            image, _ = read_ct_volume(directory)

            sentinel = directory / "sentinel.dcm"
            sentinel.write_bytes(b"unrelated user file")
            out = Path(tmp) / "nested" / "volume.nii.gz"
            out.parent.mkdir()
            with self.assertRaises(GeometryError):
                write_nifti(image, out, expected_shape=(image.GetSize()[0], image.GetSize()[1] + 1, image.GetSize()[2]))
            self.assertFalse(out.exists())
            self.assertTrue(sentinel.exists())


if __name__ == "__main__":
    unittest.main()
