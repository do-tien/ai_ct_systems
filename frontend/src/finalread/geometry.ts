// Pure candidate projection for the axial viewer. Everything works in DICOM
// patient-space LPS mm; the detector center is projected along the series
// normal onto one verified slice for display only, and the source center is
// never modified. No Cornerstone or DOM import.

import type { SeriesPlan, Vec3 } from '../viewer/series.ts'
import { dot, normalizeVec } from '../reads/geometry.ts'
import type { Candidate } from './types.ts'

// A single-slice series has no spacing, so a tiny fixed tolerance stands in for
// the half-spacing expansion of a multi-slice one.
const SINGLE_SLICE_TOLERANCE_MM = 0.01

export class CandidateGeometryError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'CandidateGeometryError'
  }
}

function isFiniteVec3(value: unknown): value is Vec3 {
  return (
    Array.isArray(value) &&
    value.length === 3 &&
    Number.isFinite(value[0]) &&
    Number.isFinite(value[1]) &&
    Number.isFinite(value[2])
  )
}

// Resolves the nearest slice for a candidate center and the center projected
// onto that slice's plane. The center is accepted only when its projection lies
// within the outer slice planes expanded by half the respective end spacing, so
// an out-of-stack center is rejected instead of pinned to an edge. Exact ties
// choose the lower sorted slice index.
export function locateCandidate(
  candidate: Candidate,
  series: SeriesPlan,
): { sliceIndex: number; displayCenter: Vec3 } {
  if (!isFiniteVec3(candidate.center_mm)) {
    throw new CandidateGeometryError('candidate center is not a finite triple')
  }
  if (series.slices.length === 0) {
    throw new CandidateGeometryError('series has no slices')
  }
  const normal = normalizeVec(series.normal)
  const projection = dot(candidate.center_mm, normal)

  const first = series.slices[0].positionMM
  const last = series.slices[series.slices.length - 1].positionMM
  const lowerPad = series.slices.length > 1 ? (series.slices[1].positionMM - first) / 2 : SINGLE_SLICE_TOLERANCE_MM
  const upperPad =
    series.slices.length > 1
      ? (last - series.slices[series.slices.length - 2].positionMM) / 2
      : SINGLE_SLICE_TOLERANCE_MM
  if (projection < first - lowerPad || projection > last + upperPad) {
    throw new CandidateGeometryError('candidate center is outside the series stack')
  }

  let sliceIndex = 0
  let bestDistance = Math.abs(series.slices[0].positionMM - projection)
  for (let index = 1; index < series.slices.length; index += 1) {
    const distance = Math.abs(series.slices[index].positionMM - projection)
    if (distance < bestDistance) {
      bestDistance = distance
      sliceIndex = index
    }
  }

  const plane = series.slices[sliceIndex].positionMM
  const shift = plane - projection
  const displayCenter: Vec3 = [
    candidate.center_mm[0] + normal[0] * shift,
    candidate.center_mm[1] + normal[1] * shift,
    candidate.center_mm[2] + normal[2] * shift,
  ]
  return { sliceIndex, displayCenter }
}
