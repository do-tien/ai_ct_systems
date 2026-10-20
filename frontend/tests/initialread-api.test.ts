import test from 'node:test'
import assert from 'node:assert/strict'

import {
  InitialApiError,
  parseInitialMarks,
  parseInitialRead,
  parseSubmitResult,
} from '../src/initialread/api.ts'

const MARK = {
  p1_mm: [-102.3, 45.1, -180.5],
  p2_mm: [-90.2, 33.0, -180.5],
  center_mm: [-96.25, 39.05, -180.5],
  diameter_mm: 17,
  origin: 'reader',
  ai_candidate_id: null,
  view_plane_normal: [0, 0, 1],
  note: null,
}

const READ = {
  id: 1,
  case_id: 4,
  kind: 'initial',
  reader: 'Bác sĩ A',
  locked_at: null,
  marks: [MARK],
}

test('parsesUnlockedAndLockedReadPreservingValues', () => {
  const unlocked = parseInitialRead({ ...READ }, 4)
  assert.equal(unlocked.id, 1)
  assert.equal(unlocked.case_id, 4)
  assert.equal(unlocked.reader, 'Bác sĩ A')
  assert.equal(unlocked.locked_at, null)
  assert.deepEqual(unlocked.marks[0].p1_mm, [-102.3, 45.1, -180.5])
  assert.equal(unlocked.marks[0].note, null)

  const locked = parseInitialRead({ ...READ, locked_at: '2026-10-11T00:00:00Z' }, 4)
  assert.equal(locked.locked_at, '2026-10-11T00:00:00Z')
})

test('preservesDuplicateOwnerDisplayNames', () => {
  // Two different users may share a display name; ownership lives in the
  // server session, so the parser must preserve both names unchanged.
  const first = parseInitialRead({ ...READ, reader: 'Bác sĩ A' }, 4)
  const second = parseInitialRead({ ...READ, reader: 'Bác sĩ A' }, 4)
  assert.equal(first.reader, 'Bác sĩ A')
  assert.equal(second.reader, 'Bác sĩ A')
  assert.equal(parseInitialRead({ ...READ, reader: '  Bác sĩ A  ' }, 4).reader, '  Bác sĩ A  ')
})

test('parsesEmptyMarkList', () => {
  assert.deepEqual(parseInitialRead({ ...READ, marks: [] }, 4).marks, [])
  assert.deepEqual(parseInitialMarks([]), [])
})

test('rejectsWrongCaseKindReader', () => {
  assert.throws(() => parseInitialRead({ ...READ }, 5), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, kind: 'final' }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, reader: '' }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, reader: '   ' }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, reader: 7 }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, reader: null }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, id: 0 }, 4), InitialApiError)
  assert.throws(() => parseInitialRead([READ], 4), InitialApiError)
})

test('rejectsMalformedGeometry', () => {
  const bad = (patch: Record<string, unknown>) => ({ ...READ, marks: [{ ...MARK, ...patch }] })
  assert.throws(() => parseInitialRead(bad({ p1_mm: [1, 2] }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ p1_mm: [1, null, 3] }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ center_mm: 'x' }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ view_plane_normal: [0, 0] }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ view_plane_normal: null }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ diameter_mm: 0 }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ diameter_mm: Number.POSITIVE_INFINITY }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ p1_mm: [1, 2, 3], p2_mm: [1, 2, 3] }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ note: 7 }), 4), InitialApiError)
  assert.throws(() => parseInitialRead(bad({ view_plane_normal: [0, 0, 0] }), 4), InitialApiError)
})

test('rejectsNonReaderOriginAndCandidateLinkage', () => {
  assert.throws(() => parseInitialRead({ ...READ, marks: [{ ...MARK, origin: 'from_ai_candidate' }] }, 4), InitialApiError)
  assert.throws(() => parseInitialRead({ ...READ, marks: [{ ...MARK, ai_candidate_id: 12 }] }, 4), InitialApiError)
})

test('preservesOrderNegativeCoordinatesAndNoteText', () => {
  const second = { ...MARK, p1_mm: [-5, -8, -9], note: 'nốt trái' }
  const parsed = parseInitialRead({ ...READ, marks: [MARK, second] }, 4)
  assert.equal(parsed.marks.length, 2)
  assert.deepEqual(parsed.marks[1].p1_mm, [-5, -8, -9])
  assert.equal(parsed.marks[1].note, 'nốt trái')
})

test('acceptsBareMarkArrayButNotReadEnvelope', () => {
  assert.equal(parseInitialMarks([MARK]).length, 1)
  assert.throws(() => parseInitialMarks({ marks: [MARK] }), InitialApiError)
})

test('parsesSubmitResultAndRejectsMalformed', () => {
  const ok = parseSubmitResult({ status: 'initial_locked', locked_at: '2026-10-11T00:00:00Z', final_read_id: 2 })
  assert.equal(ok.final_read_id, 2)
  assert.throws(() => parseSubmitResult({ status: 'other', locked_at: 'x', final_read_id: 2 }), InitialApiError)
  assert.throws(() => parseSubmitResult({ status: 'initial_locked', locked_at: '', final_read_id: 2 }), InitialApiError)
  assert.throws(() => parseSubmitResult({ status: 'initial_locked', locked_at: 'x', final_read_id: 0 }), InitialApiError)
  assert.throws(() => parseSubmitResult(null), InitialApiError)
})
