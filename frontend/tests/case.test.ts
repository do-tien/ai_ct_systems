import test from 'node:test'
import assert from 'node:assert/strict'

import { parseCaseMetadata } from '../src/case.ts'

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

test('parsesAllowlistedCase', () => {
  const parsed = parseCaseMetadata({ ...VALID }, 4)
  assert.equal(parsed.id, 4)
  assert.equal(parsed.studyInstanceUID, '1.2.3.4.5')
  assert.equal(parsed.seriesInstanceUID, '1.2.3.4.6')
  assert.equal(parsed.frameOfReferenceUID, '1.2.3.4.7')
  assert.equal(parsed.status, 'awaiting_read')
  assert.equal(parsed.finalizedAt, null)
})

test('acceptsLaterStatesAndFinalizedTimestamp', () => {
  const parsed = parseCaseMetadata({ ...VALID, status: 'initial_locked' }, 4)
  assert.equal(parsed.status, 'initial_locked')
  const done = parseCaseMetadata({ ...VALID, status: 'finalized', finalized_at: '2026-10-11T00:00:00Z' }, 4)
  assert.equal(done.finalizedAt, '2026-10-11T00:00:00Z')
})

test('rejectsWrongIdOrUnexpectedIdType', () => {
  assert.throws(() => parseCaseMetadata({ ...VALID }, 5), /id/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, id: '4' }, 4), /id/i)
})

test('rejectsInvalidUidSyntax', () => {
  assert.throws(() => parseCaseMetadata({ ...VALID, study_instance_uid: '1.2.3.4/..' }, 4), /study|uid/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, series_instance_uid: '' }, 4), /series|uid/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, frame_of_reference_uid: '1.2.03.4' }, 4), /frame|uid/i)
})

test('rejectsUnknownStateAndWrongTypes', () => {
  assert.throws(() => parseCaseMetadata({ ...VALID, status: 'ai_revealed' }, 4), /status|state/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, status: 42 }, 4), /status|state/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, source: null }, 4), /source/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, created_at: 7 }, 4), /created/i)
  assert.throws(() => parseCaseMetadata({ ...VALID, finalized_at: 7 }, 4), /finalized/i)
  assert.throws(() => parseCaseMetadata(null, 4), /case/i)
  assert.throws(() => parseCaseMetadata([], 4), /case/i)
})

test('ignoresUnknownFieldsIncludingAnyAiLikeKey', () => {
  const parsed = parseCaseMetadata(
    { ...VALID, ai_status: 'succeeded', candidates: [{ id: 'c1' }], score: 0.9 },
    4,
  )
  assert.deepEqual(Object.keys(parsed).sort(), [
    'createdAt',
    'finalizedAt',
    'frameOfReferenceUID',
    'id',
    'seriesInstanceUID',
    'source',
    'status',
    'studyInstanceUID',
  ])
})
