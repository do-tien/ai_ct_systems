import test from 'node:test'
import assert from 'node:assert/strict'

import { edgeLabels } from '../src/viewer/orientation.ts'

test('labelsRadiologicalAxes', () => {
  const labels = edgeLabels([1, 0, 0], [0, 1, 0])
  assert.deepEqual(labels, { left: 'R', right: 'L', top: 'A', bottom: 'P' })
})

test('oppositeVectorsGiveOppositeLabels', () => {
  const reversed = edgeLabels([-1, 0, 0], [0, -1, 0])
  assert.deepEqual(reversed, { left: 'L', right: 'R', top: 'P', bottom: 'A' })
})

test('rotatedInPlaneVectorsGiveCompoundDirections', () => {
  const s = Math.SQRT1_2
  const rotated = edgeLabels([s, s, 0], [-s, s, 0])
  assert.equal(rotated.right, 'LP')
  assert.equal(rotated.left, 'RA')
  assert.equal(rotated.bottom, 'RP')
  assert.equal(rotated.top, 'LA')
})

test('superiorAndInferiorLabels', () => {
  // Sagittal-like basis: screen-right points posterior, screen-down points inferior.
  const labels = edgeLabels([0, 1, 0], [0, 0, -1])
  assert.equal(labels.right, 'P')
  assert.equal(labels.bottom, 'I')
  assert.equal(labels.left, 'A')
  assert.equal(labels.top, 'S')
})

test('rejectsDegenerateDirections', () => {
  assert.throws(() => edgeLabels([0, 0, 0], [0, 1, 0]), /direction/i)
  assert.throws(() => edgeLabels([1, 0, 0], [Number.NaN, 1, 0]), /direction/i)
})
