"""Affine/index/RAS invariants for the shared DICOM geometry module."""

from __future__ import annotations

import unittest

import numpy as np

from module_ai.dicom_geometry import (
    GeometryError,
    lps_to_ras,
    lps_to_ras_affine,
    ras_to_lps,
    voxel_to_lps_affine,
    voxel_to_world,
    world_to_voxel,
)


def metadata(iop=(0.0, 1.0, 0.0, -1.0, 0.0, 0.0), spacing=(2.0, 3.0), step=1.5, origin=(10.0, 20.0, 30.0)) -> dict:
    normal = np.cross(np.array(iop[:3]), np.array(iop[3:]))
    normal = normal / np.linalg.norm(normal)
    return {
        "image_orientation_patient": list(iop),
        "pixel_spacing_mm": [float(spacing[0]), float(spacing[1])],
        "median_slice_spacing_mm": float(step),
        "slice_normal_lps": normal.tolist(),
        "image_position_patient": [list(origin)],
        "num_slices": 2,
    }


class VoxelAffineTests(unittest.TestCase):
    def test_voxel_affine_respects_column_row_order(self):
        affine = voxel_to_lps_affine(metadata())
        self.assertEqual(affine.shape, (4, 4))

        lps = voxel_to_world((5.0, 4.0, 2.0), affine)
        for got, expected in zip(lps, (2.0, 35.0, 33.0)):
            self.assertLessEqual(abs(got - expected), 1e-6)

        ras = lps_to_ras(lps)
        for got, expected in zip(ras, (-2.0, -35.0, 33.0)):
            self.assertLessEqual(abs(got - expected), 1e-6)

        back = world_to_voxel(lps, affine)
        for got, expected in zip(back, (5.0, 4.0, 2.0)):
            self.assertLessEqual(abs(got - expected), 1e-6)

    def test_oblique_affine_round_trip_preserves_patient_side(self):
        # Orthonormal oblique orientation with anisotropic spacing and off-origin.
        raw = np.array([[0.6, 0.8, 0.0], [-0.8, 0.6, 0.0], [0.0, 0.0, 1.0]])
        iop = list(raw[0]) + list(raw[1])
        affine = voxel_to_lps_affine(metadata(iop=iop, spacing=(2.5, 0.7), step=1.2, origin=(-120.0, 33.0, -50.0)))

        index = np.array([12.25, 3.5, 7.0])
        lps = np.array(voxel_to_world(index, affine))
        # Independent construction from the raw DICOM definition.
        expected = np.array([-120.0, 33.0, -50.0]) + index[0] * 0.7 * raw[0] + index[1] * 2.5 * raw[1] + index[2] * 1.2 * raw[2]
        self.assertLessEqual(float(np.linalg.norm(lps - expected)), 1e-6)

        back = np.array(world_to_voxel(lps, affine))
        self.assertLessEqual(float(np.linalg.norm(back - index)), 1e-6)

        self.assertLess(lps[0], 0.0)
        self.assertGreater(lps_to_ras(lps)[0], 0.0)  # x is flipped in RAS
        round_tripped = np.array(ras_to_lps(lps_to_ras(lps)))
        self.assertLess(round_tripped[0], 0.0)
        self.assertLessEqual(float(np.linalg.norm(round_tripped - lps)), 1e-9)

    def test_lps_to_ras_affine_is_a_world_flip(self):
        affine = voxel_to_lps_affine(metadata())
        ras_affine = lps_to_ras_affine(affine)
        expected = np.diag([-1.0, -1.0, 1.0, 1.0]) @ affine
        self.assertLessEqual(float(np.max(np.abs(ras_affine - expected))), 1e-12)

    def test_rejects_singular_or_nonfinite_affine(self):
        singular = np.zeros((4, 4))
        with self.assertRaises(GeometryError) as raised:
            voxel_to_world((0.0, 0.0, 0.0), singular)
        self.assertEqual(raised.exception.code, "INVALID_GEOMETRY")

        nonfinite = np.eye(4)
        nonfinite[0, 3] = np.nan
        with self.assertRaises(GeometryError) as raised:
            world_to_voxel((0.0, 0.0, 0.0), nonfinite)
        self.assertEqual(raised.exception.code, "INVALID_GEOMETRY")

        with self.assertRaises(GeometryError):
            voxel_to_world((0.0, 0.0), np.eye(4))

    def test_rejects_nonfinite_point(self):
        with self.assertRaises(GeometryError) as raised:
            voxel_to_world((0.0, np.inf, 0.0), np.eye(4))
        self.assertEqual(raised.exception.code, "INVALID_GEOMETRY")


if __name__ == "__main__":
    unittest.main()
