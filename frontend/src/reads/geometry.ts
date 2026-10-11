// Shared measurement and slice-matching geometry for both clinical reads. It
// works only in DICOM patient-space LPS mm: no LPS/RAS conversion, no sign flip
// and no display rounding. Display formatting lives in the UI. The initial read
// and the final read use the same functions; the only difference is that a
// final mark may legally carry a null stored normal, which is inferred onto a
// display plane instead of being treated as a required parallel vector.

import type { SeriesPlan, Vec3 } from '../viewer/series.ts'

// Mirrors the viewer/importer tolerances: direction 1e-5, plane 0.01 mm.
const DIRECTION_TOLERANCE = 1e-5
const PLANE_TOLERANCE_MM = 0.01

export type MarkOrigin = 'reader' | 'reader_after_ai' | 'from_ai_candidate'

// The clinical wire shape a stored mark shares between reads. `origin` and
// `ai_candidate_id` are never changed by geometry; only endpoints, center,
// diameter, normal and note are measurement output.
export interface ReadMark {
  p1_mm: Vec3
  p2_mm: Vec3
  center_mm: Vec3
  diameter_mm: number
  origin: MarkOrigin
  ai_candidate_id: string | null
  view_plane_normal: Vec3 | null
  note: string | null
}

// The measured part of a freshly drawn mark: both endpoints and a required,
// finite, non-zero world normal. Never includes origin/candidate/note.
export interface MeasuredGeometry {
  p1_mm: Vec3
  p2_mm: Vec3
  center_mm: Vec3
  diameter_mm: number
  view_plane_normal: Vec3
  note: string | null
}

export class MarkGeometryError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'MarkGeometryError'
  }
}

export function isFiniteVec3(value: unknown): value is Vec3 {
  return (
    Array.isArray(value) &&
    value.length === 3 &&
    Number.isFinite(value[0]) &&
    Number.isFinite(value[1]) &&
    Number.isFinite(value[2])
  )
}

export function dot(a: Vec3, b: Vec3): number {
  return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]
}

export function normalizeVec(vector: Vec3): Vec3 {
  const length = Math.hypot(vector[0], vector[1], vector[2])
  if (!Number.isFinite(length) || length === 0) {
    throw new MarkGeometryError('view plane normal is not a finite non-zero vector')
  }
  return [vector[0] / length, vector[1] / length, vector[2] / length]
}

// center = midpoint, diameter = distance between endpoints, both in mm and
// straight from the world coordinates the tool reports. Rejects anything that
// must never reach the API: nonfinite endpoints, identical endpoints, a
// missing/zero normal.
export function measureEndpoints(p1: Vec3, p2: Vec3, normal: Vec3, note: string | null): MeasuredGeometry {
  if (!isFiniteVec3(p1) || !isFiniteVec3(p2) || !isFiniteVec3(normal)) {
    throw new MarkGeometryError('measurement endpoints and normal must be finite triples')
  }
  const delta: Vec3 = [p2[0] - p1[0], p2[1] - p1[1], p2[2] - p1[2]]
  const diameter = Math.hypot(delta[0], delta[1], delta[2])
  if (diameter === 0) {
    throw new MarkGeometryError('measurement endpoints are identical')
  }
  normalizeVec(normal)
  const center: Vec3 = [(p1[0] + p2[0]) / 2, (p1[1] + p2[1]) / 2, (p1[2] + p2[2]) / 2]
  return {
    p1_mm: [...p1],
    p2_mm: [...p2],
    center_mm: center,
    diameter_mm: diameter,
    view_plane_normal: [...normal],
    note,
  }
}

function findPlaneIndex(positionMM: number, series: SeriesPlan): number {
  let match = -1
  for (let index = 0; index < series.slices.length; index += 1) {
    if (Math.abs(series.slices[index].positionMM - positionMM) <= PLANE_TOLERANCE_MM) {
      if (match !== -1) {
        throw new MarkGeometryError('mark projection is ambiguous across slice planes')
      }
      match = index
    }
  }
  if (match === -1) {
    throw new MarkGeometryError('mark is not on a slice plane of this series')
  }
  return match
}

// Resolves the rendered slice index for a stored mark, or throws when this
// axial viewer cannot represent it. A mark is representable when both endpoints
// fall on the same slice plane within 0.01 mm. A mark with a stored normal must
// also have that normal parallel/antiparallel to the series normal; a final mark
// with a null normal infers the display plane from the endpoints alone. Off-
// plane, out-of-stack or rotated marks are never snapped to the nearest slice.
export function findReadMarkSlice(mark: ReadMark, series: SeriesPlan): number {
  if (!isFiniteVec3(mark.p1_mm) || !isFiniteVec3(mark.p2_mm) || !isFiniteVec3(series.normal)) {
    throw new MarkGeometryError('mark geometry is not finite')
  }
  const seriesNormal = normalizeVec(series.normal)
  if (mark.view_plane_normal !== null) {
    if (!isFiniteVec3(mark.view_plane_normal)) {
      throw new MarkGeometryError('mark geometry is not finite')
    }
    const storedNormal = normalizeVec(mark.view_plane_normal)
    if (Math.abs(Math.abs(dot(storedNormal, seriesNormal)) - 1) > DIRECTION_TOLERANCE) {
      throw new MarkGeometryError('mark plane is not parallel to the series plane')
    }
  }
  const first = findPlaneIndex(dot(mark.p1_mm, seriesNormal), series)
  const second = findPlaneIndex(dot(mark.p2_mm, seriesNormal), series)
  if (first !== second) {
    throw new MarkGeometryError('mark endpoints do not share one slice plane')
  }
  return first
}

function vectorsEqual(a: Vec3 | null, b: Vec3 | null): boolean {
  if (a === null || b === null) {
    return a === b
  }
  return a[0] === b[0] && a[1] === b[1] && a[2] === b[2]
}

// Confirmed-snapshot equality: compares only the clinical wire fields, never
// local ids or formatted display strings.
export function marksEqual(a: ReadMark, b: ReadMark): boolean {
  return (
    vectorsEqual(a.p1_mm, b.p1_mm) &&
    vectorsEqual(a.p2_mm, b.p2_mm) &&
    vectorsEqual(a.center_mm, b.center_mm) &&
    a.diameter_mm === b.diameter_mm &&
    a.origin === b.origin &&
    a.ai_candidate_id === b.ai_candidate_id &&
    vectorsEqual(a.view_plane_normal, b.view_plane_normal) &&
    a.note === b.note
  )
}

export function markListsEqual(a: readonly ReadMark[], b: readonly ReadMark[]): boolean {
  if (a.length !== b.length) {
    return false
  }
  return a.every((mark, index) => marksEqual(mark, b[index]))
}
