import test from 'node:test'
import assert from 'node:assert/strict'

import { UploadApiError, messageForCode, parseUploadCaseResult, parseUploadSession } from '../src/upload/api.ts'
import type { UploadSession } from '../src/upload/types.ts'

function validSession(): Record<string, unknown> {
  return {
    id: 'token-1',
    expires_at: '2026-10-14T12:00:00Z',
    skipped_files: 3,
    series: [
      { id: 'k1', index: 1, modality: 'CT', instance_count: 120, suitable: true, reason_code: null },
      { id: 'k2', index: 2, modality: 'CT', instance_count: 1, suitable: false, reason_code: 'SINGLE_SLICE' },
    ],
    selected_series_id: null,
    case_id: null,
  }
}

function mutate(change: (payload: Record<string, unknown>) => void): unknown {
  const payload = validSession()
  change(payload)
  return payload
}

test('parses the documented session allowlist', () => {
  const session: UploadSession = parseUploadSession(validSession())
  assert.equal(session.id, 'token-1')
  assert.equal(session.skipped_files, 3)
  assert.equal(session.series.length, 2)
  assert.deepEqual(session.series[1], {
    id: 'k2',
    index: 2,
    modality: 'CT',
    instance_count: 1,
    suitable: false,
    reason_code: 'SINGLE_SLICE',
  })
  assert.equal(session.selected_series_id, null)
  assert.equal(session.case_id, null)
})

test('keeps a confirmed session identity', () => {
  const session = parseUploadSession(mutate((payload) => {
    payload.selected_series_id = 'k1'
    payload.case_id = 42
  }))
  assert.equal(session.selected_series_id, 'k1')
  assert.equal(session.case_id, 42)
})

test('rejects a malformed session payload', () => {
  const cases: Array<[string, unknown]> = [
    ['not an object', 'nope'],
    ['missing id', mutate((payload) => delete payload.id)],
    ['empty id', mutate((payload) => { payload.id = '' })],
    ['missing expiry', mutate((payload) => delete payload.expires_at)],
    ['negative skips', mutate((payload) => { payload.skipped_files = -1 })],
    ['non-numeric skips', mutate((payload) => { payload.skipped_files = '0' })],
    ['series not an array', mutate((payload) => { payload.series = {} })],
    ['option missing key', mutate((payload) => { (payload.series as unknown[])[0] = { index: 1 } })],
    ['option bad count', mutate((payload) => { (payload.series as Record<string, unknown>[])[0].instance_count = -2 })],
    ['option bad suitability', mutate((payload) => { (payload.series as Record<string, unknown>[])[0].suitable = 'yes' })],
    ['option numeric reason', mutate((payload) => { (payload.series as Record<string, unknown>[])[0].reason_code = 7 })],
    ['bad selection', mutate((payload) => { payload.selected_series_id = 5 })],
    ['zero case id', mutate((payload) => { payload.case_id = 0 })],
  ]
  for (const [label, payload] of cases) {
    assert.throws(() => parseUploadSession(payload), UploadApiError, label)
  }
})

test('parses and rejects the confirm result', () => {
  assert.deepEqual(parseUploadCaseResult({ case_id: 7, instance_count: 158, existing: true }), {
    case_id: 7,
    instance_count: 158,
    existing: true,
  })
  for (const payload of [
    {},
    { case_id: 0, instance_count: 1, existing: false },
    { case_id: 7, instance_count: -1, existing: false },
    { case_id: 7, instance_count: 1, existing: 'no' },
    'nope',
  ]) {
    assert.throws(() => parseUploadCaseResult(payload), UploadApiError)
  }
})

test('maps known codes to safe messages and everything else to the generic one', () => {
  assert.match(messageForCode('UPLOAD_EXPIRED'), /hết hạn/)
  assert.match(messageForCode('UNSUITABLE_SERIES'), /không phù hợp/)
  const generic = messageForCode('SOMETHING_NEW')
  assert.equal(messageForCode(null), generic)
})
