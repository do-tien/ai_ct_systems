import test from 'node:test'
import assert from 'node:assert/strict'

import { CandidateGeometryError, locateCandidate } from '../src/finalread/geometry.ts'
import { MarkGeometryError, findReadMarkSlice, measureEndpoints } from '../src/reads/geometry.ts'
import type { Candidate } from '../src/finalread/types.ts'
import type { SeriesPlan, Vec3 } from '../src/viewer/series.ts'

// A rotated axial series whose normal is +x, so a test that silently sorts by z
// or assumes a [0,0,1] normal fails. Slice planes are x = 10, 12, 14.
function xSeries(positions: number[] = [10, 12, 14]): SeriesPlan {
  const make = (x: number, index: number) => ({
    imageId: `wadors:http://example.test/frames/${index}`,
    sopInstanceUID: `1.2.3.${index}`,
    positionMM: x,
    imagePositionPatient: [x, 0, 0] as Vec3,
    metadata: {},
  })
  return {
    slices: positions.map((x, index) => make(x, index)),
    frameOfReferenceUID: '1.2.3',
    rowDirection: [0, 1, 0],
    columnDirection: [0, 0, 1],
    normal: [1, 0, 0],
  }
}

function candidate(center: Vec3): Candidate {
  return { id: 'c1', center_mm: center, size_mm: [3, 4, 5], score: 0.7 }
}

test('projectsNearestSliceInPatientSpace', () => {
  const series = xSeries()
  // x=11 ties between planes 10 and 12; the lower index wins.
  const tie = locateCandidate(candidate([11, 5, 7]), series)
  assert.equal(tie.sliceIndex, 0)
  assert.deepEqual(tie.displayCenter, [10, 5, 7])

  // The outer half-spacing expansion admits x=15 (last plane 14 + spacing/2).
  const upper = locateCandidate(candidate([15, 0, 0]), series)
  assert.equal(upper.sliceIndex, 2)
  assert.deepEqual(upper.displayCenter, [14, 0, 0])

  // Slightly inside the lower pad is admitted; outside it is rejected.
  assert.equal(locateCandidate(candidate([9, 0, 0]), series).sliceIndex, 0)
  assert.throws(() => locateCandidate(candidate([15.01, 0, 0]), series), CandidateGeometryError)
  assert.throws(() => locateCandidate(candidate([8.99, 0, 0]), series), CandidateGeometryError)
})

test('handlesNegativeLPSAndSingleSlice', () => {
  // Normal along -x: positionMM = dot(ipp, normal) is positive for negative x.
  const negative: SeriesPlan = {
    slices: [
      { imageId: 'w0', sopInstanceUID: '1.2.3.0', positionMM: 0, imagePositionPatient: [0, 0, 0], metadata: {} },
      { imageId: 'w1', sopInstanceUID: '1.2.3.1', positionMM: 2, imagePositionPatient: [-2, 0, 0], metadata: {} },
      { imageId: 'w2', sopInstanceUID: '1.2.3.2', positionMM: 4, imagePositionPatient: [-4, 0, 0], metadata: {} },
    ],
    frameOfReferenceUID: '1.2.3',
    rowDirection: [0, 1, 0],
    columnDirection: [0, 0, 1],
    normal: [-1, 0, 0],
  }
  const located = locateCandidate(candidate([-1, 0, 0]), negative)
  assert.equal(located.sliceIndex, 0)
  assert.deepEqual(located.displayCenter, [0, 0, 0])

  const single = xSeries([10])
  assert.equal(locateCandidate(candidate([10.005, 1, 2]), single).sliceIndex, 0)
  assert.throws(() => locateCandidate(candidate([10.02, 1, 2]), single), CandidateGeometryError)

  // Nonfinite center is rejected before any projection.
  assert.throws(() => locateCandidate(candidate([Number.NaN, 0, 0]), xSeries()), CandidateGeometryError)
})

test('restoresFinalMarkWithNullNormalByInferringThePlane', () => {
  const series = xSeries()
  const base = {
    p1_mm: [10, 4, 5] as Vec3,
    p2_mm: [10, -4, 5] as Vec3,
    center_mm: [10, 0, 5] as Vec3,
    diameter_mm: 8,
    origin: 'reader_after_ai' as const,
    ai_candidate_id: null,
    note: null,
  }
  // A null stored normal infers exactly one shared slice plane.
  assert.equal(findReadMarkSlice({ ...base, view_plane_normal: null }, series), 0)
  // Endpoints on two planes, or off-plane, are rejected rather than snapped.
  assert.throws(
    () => findReadMarkSlice({ ...base, p2_mm: [12, -4, 5], view_plane_normal: null }, series),
    MarkGeometryError,
  )
  assert.throws(
    () => findReadMarkSlice({ ...base, p1_mm: [11, 4, 5], p2_mm: [11, -4, 5], view_plane_normal: null }, series),
    MarkGeometryError,
  )
  // A supplied nonparallel normal is still rejected.
  assert.throws(() => findReadMarkSlice({ ...base, view_plane_normal: [0, 0, 1] }, series), MarkGeometryError)
})

test('measuresEndpointsWithoutRounding', () => {
  const measured = measureEndpoints([-100, 40, 1500], [-94, 48, 1500], [0, 0, 1], 'ghi chú')
  assert.deepEqual(measured.center_mm, [-97, 44, 1500])
  assert.equal(measured.diameter_mm, 10)
  assert.deepEqual(measured.view_plane_normal, [0, 0, 1])
  assert.equal(measured.note, 'ghi chú')
  assert.throws(() => measureEndpoints([1, 2, 3], [1, 2, 3], [0, 0, 1], null), MarkGeometryError)
  assert.throws(() => measureEndpoints([1, 2, 3], [4, 5, 6], [0, 0, 0], null), MarkGeometryError)
})
