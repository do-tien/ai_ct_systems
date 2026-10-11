import test from 'node:test'
import assert from 'node:assert/strict'

import { findMarkSlice, markListsEqual, measureInitialMark, MarkGeometryError } from '../src/initialread/geometry.ts'
import type { InitialMark } from '../src/initialread/types.ts'
import type { SeriesPlan, Vec3 } from '../src/viewer/series.ts'

// A rotated axial series whose normal is +x, so a test that silently sorts by
// z or assumes a [0,0,1] normal fails. Slice planes are x = 0, 10, 20.
function xNormalSeries(): SeriesPlan {
  const make = (x: number, index: number) => ({
    imageId: `wadors:http://example.test/frames/${index}`,
    sopInstanceUID: `1.2.3.${index}`,
    positionMM: x,
    imagePositionPatient: [x, 0, 0] as Vec3,
    metadata: {},
  })
  return {
    slices: [make(0, 0), make(10, 1), make(20, 2)],
    frameOfReferenceUID: '1.2.3',
    rowDirection: [0, 1, 0],
    columnDirection: [0, 0, 1],
    normal: [1, 0, 0],
  }
}

function markWith(p1: Vec3, p2: Vec3, normal: Vec3): InitialMark {
  return measureInitialMark(p1, p2, normal, null)
}

test('measuresWorldEndpointsWithoutRounding', () => {
  const mark = measureInitialMark([-100, 40, 1500], [-94, 48, 1500], [0, 0, 1], 'ghi chú')
  assert.deepEqual(mark.center_mm, [-97, 44, 1500])
  assert.equal(mark.diameter_mm, 10)
  assert.equal(mark.origin, 'reader')
  assert.equal(mark.ai_candidate_id, null)
  assert.deepEqual(mark.view_plane_normal, [0, 0, 1])
  assert.equal(mark.note, 'ghi chú')
  assert.deepEqual(mark.p1_mm, [-100, 40, 1500])
})

test('rejectsDegenerateNonfiniteAndZeroNormal', () => {
  assert.throws(() => measureInitialMark([1, 2, 3], [1, 2, 3], [0, 0, 1], null), MarkGeometryError)
  assert.throws(() => measureInitialMark([1, 2, Number.NaN], [4, 5, 6], [0, 0, 1], null), MarkGeometryError)
  assert.throws(() => measureInitialMark([1, 2, 3], [4, 5, Number.POSITIVE_INFINITY], [0, 0, 1], null), MarkGeometryError)
  assert.throws(() => measureInitialMark([1, 2, 3], [4, 5, 6], [0, 0, 0], null), MarkGeometryError)
  // Rejections also hold when the bad triples are passed as unknown arrays.
  assert.throws(
    () => measureInitialMark([1, 2] as unknown as Vec3, [4, 5, 6], [0, 0, 1], null),
    MarkGeometryError,
  )
})

test('matchesProjectedPlaneWithSignedNormal', () => {
  const series = xNormalSeries()
  const forward = markWith([10, 4, 5], [10, -4, 5], [1, 0, 0])
  assert.equal(findMarkSlice(forward, series), 1)
  // Antiparallel stored normal must still resolve to the same slice.
  const anti = markWith([10, 4, 5], [10, -4, 5], [-1, 0, 0])
  assert.equal(findMarkSlice(anti, series), 1)
  // Endpoints within the plane tolerance match, not snap to a neighbouring slice.
  const offset = markWith([10.005, 4, 5], [9.995, -4, 5], [1, 0, 0])
  assert.equal(findMarkSlice(offset, series), 1)
})

test('rejectsOffPlaneOrUnsupportedMark', () => {
  const series = xNormalSeries()
  // Endpoints on two different slice planes.
  assert.throws(() => findMarkSlice(markWith([0, 4, 5], [10, -4, 5], [1, 0, 0]), series), MarkGeometryError)
  // Center between slices: no plane within tolerance.
  assert.throws(() => findMarkSlice(markWith([5, 4, 5], [5, -4, 5], [1, 0, 0]), series), MarkGeometryError)
  // Out of stack.
  assert.throws(() => findMarkSlice(markWith([100, 4, 5], [100, -4, 5], [1, 0, 0]), series), MarkGeometryError)
  // Nonparallel normal.
  assert.throws(() => findMarkSlice(markWith([10, 4, 5], [10, -4, 5], [0, 0, 1]), series), MarkGeometryError)
})

test('comparesClinicalFieldsOnly', () => {
  const a = measureInitialMark([0, 0, 0], [3, 4, 0], [0, 0, 1], null)
  const b = measureInitialMark([0, 0, 0], [3, 4, 0], [0, 0, 1], null)
  const moved = measureInitialMark([0, 0, 0], [4, 4, 0], [0, 0, 1], null)
  const noted = measureInitialMark([0, 0, 0], [3, 4, 0], [0, 0, 1], 'x')
  assert.equal(markListsEqual([a], [b]), true)
  assert.equal(markListsEqual([a], [moved]), false)
  assert.equal(markListsEqual([a], [noted]), false)
  assert.equal(markListsEqual([a], []), false)
})
