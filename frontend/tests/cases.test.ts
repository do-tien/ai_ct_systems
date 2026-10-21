import test from 'node:test'
import assert from 'node:assert/strict'

import { parseCaseList } from '../src/cases.ts'
import { parseCaseMetadata, parseCaseSummary } from '../src/case.ts'

const VALID = {
  id: 4,
  source: 'LIDC-IDRI/LIDC-IDRI-0001',
  study_instance_uid: '1.2.3.4.5',
  series_instance_uid: '1.2.3.4.6',
  frame_of_reference_uid: '1.2.3.4.7',
  status: 'awaiting_read',
  created_at: '2026-10-10T00:00:00Z',
  finalized_at: null,
}

test('parsesOwnedCaseList', () => {
  const cases = parseCaseList({
    cases: [VALID, { ...VALID, id: 5, source: 'LIDC-IDRI/LIDC-IDRI-0002', status: 'finalized', finalized_at: '2026-10-11T00:00:00Z' }],
  })
  assert.equal(cases.length, 2)
  assert.equal(cases[0].id, 4)
  assert.equal(cases[1].status, 'finalized')
  assert.equal(cases[1].finalizedAt, '2026-10-11T00:00:00Z')
})

test('acceptsAnEmptyCaseList', () => {
  assert.deepEqual(parseCaseList({ cases: [] }), [])
})

test('rejectsMissingOrNonArrayCases', () => {
  assert.throws(() => parseCaseList({}), /cases/i)
  assert.throws(() => parseCaseList({ cases: null }), /cases/i)
  assert.throws(() => parseCaseList({ cases: {} }), /cases/i)
  assert.throws(() => parseCaseList([]), /object/i)
  assert.throws(() => parseCaseList(null), /object/i)
})

test('rejectsDuplicateCaseIds', () => {
  assert.throws(() => parseCaseList({ cases: [VALID, VALID] }), /repeat|duplicate/i)
})

test('rejectsInvalidRowsAndUnknownFieldsAreIgnored', () => {
  assert.throws(() => parseCaseList({ cases: [{ ...VALID, id: '4' }] }), /id/i)
  assert.throws(() => parseCaseList({ cases: [{ ...VALID, id: 0 }] }), /id/i)
  assert.throws(() => parseCaseList({ cases: [{ ...VALID, status: 'signed' }] }), /status/i)
  assert.throws(() => parseCaseList({ cases: [{ ...VALID, series_instance_uid: 'bad uid' }] }), /series|uid/i)
  const parsed = parseCaseList({ cases: [{ ...VALID, ai_status: 'succeeded', candidates: [1] }] })
  assert.equal(parsed.length, 1)
  assert.equal('ai_status' in parsed[0], false)
})

test('summaryParsingMatchesSingleCaseValidation', () => {
  const summary = parseCaseSummary(VALID)
  const single = parseCaseMetadata(VALID, 4)
  assert.deepEqual(summary, single)
})
