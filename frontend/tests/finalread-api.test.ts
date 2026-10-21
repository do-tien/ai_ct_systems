import test from 'node:test'
import assert from 'node:assert/strict'

import { parseAI, parseFinalRead, parseFinalizeResult, FinalApiError } from '../src/finalread/api.ts'

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

const DECISION = { candidate_id: 'c1', decision: 'accepted', dismiss_reason: null }

const READ = {
  id: 7,
  case_id: 4,
  kind: 'final',
  reader: 'Bác sĩ A',
  locked_at: null,
  marks: [MARK],
  candidate_decisions: [DECISION],
}

function candidate(id: string, patch: Record<string, unknown> = {}): Record<string, unknown> {
  return { id, center_mm: [0, 0, 0], size_mm: [3, 4, 5], score: 0.5, ...patch }
}

const SUCCESS = {
  status: 'succeeded',
  candidates: [candidate('c1')],
  model: { bundle: 'b', bundle_version: '1', score_threshold: 0.5 },
  input: { num_slices: 158, spacing_mm: [1, 1, 2], size_voxels: [512, 512, 158] },
  timing_s: { load: 1.5, inference: 9.25, total: 11 },
  resources: { device: 'cpu' },
}

test('parsesStateSpecificAI', () => {
  assert.deepEqual(parseAI({ status: 'queued', candidates: null, error: null }), {
    status: 'queued',
    candidates: null,
    error: null,
  })
  assert.deepEqual(parseAI({ status: 'running', candidates: null, error: null }), {
    status: 'running',
    candidates: null,
    error: null,
  })
  const failed = parseAI({ status: 'failed', candidates: [], error: { code: 'AI_UNAVAILABLE', message: 'x' } })
  assert.deepEqual(failed, { status: 'failed', candidates: [], error: { code: 'AI_UNAVAILABLE', message: 'x' } })

  const succeeded = parseAI(SUCCESS)
  assert.equal(succeeded.status, 'succeeded')
  assert.equal(succeeded.candidates.length, 1)
  assert.equal(succeeded.candidates[0].id, 'c1')
  assert.equal(succeeded.model.score_threshold, 0.5)
  assert.equal(succeeded.input.num_slices, 158)
  assert.equal(succeeded.timing_s.total, 11)
  assert.equal(succeeded.resources.device, 'cpu')

  // Empty success is a valid result with full provenance and no error member.
  const empty = parseAI({ ...SUCCESS, candidates: [] })
  assert.equal(empty.status, 'succeeded')
  assert.deepEqual(empty.candidates, [])
})

test('rejectsMalformedStateSpecificAI', () => {
  assert.throws(() => parseAI({ status: 'queued', candidates: [candidate('c1')], error: null }), FinalApiError)
  assert.throws(() => parseAI({ status: 'running', candidates: null, error: { code: 'x', message: 'y' } }), FinalApiError)
  assert.throws(() => parseAI({ status: 'failed', candidates: [candidate('c1')], error: { code: 'x', message: 'y' } }), FinalApiError)
  assert.throws(() => parseAI({ status: 'failed', candidates: [], error: null }), FinalApiError)
  assert.throws(() => parseAI({ status: 'unknown', candidates: null, error: null }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, candidates: [candidate('a'), candidate('a')] }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, candidates: [candidate('c1', { center_mm: [1, 2] })] }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, candidates: [candidate('c1', { size_mm: [3, 0, 5] })] }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, candidates: [candidate('c1', { score: 1.2 })] }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, model: { bundle: 'b' } }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, input: { num_slices: 1, spacing_mm: 'x', size_voxels: [] } }), FinalApiError)
  assert.throws(() => parseAI({ ...SUCCESS, timing_s: { load: 1 } }), FinalApiError)
})

test('parsesFinalEnvelopeWithoutInventingMarks', () => {
  const parsed = parseFinalRead({ ...READ }, 4)
  assert.equal(parsed.kind, 'final')
  assert.equal(parsed.reader, 'Bác sĩ A')
  assert.equal(parsed.marks.length, 1)
  assert.equal(parsed.candidate_decisions.length, 1)

  // Two users may share a display name; the parser preserves it exactly.
  assert.equal(parseFinalRead({ ...READ, reader: 'Bác sĩ A' }, 4).reader, 'Bác sĩ A')
  assert.equal(parseFinalRead({ ...READ, reader: '  Bác sĩ A  ' }, 4).reader, '  Bác sĩ A  ')

  // Wrong identity and a bare mark array are rejected; PUT returns an envelope.
  assert.throws(() => parseFinalRead({ ...READ }, 5), FinalApiError)
  assert.throws(() => parseFinalRead({ ...READ, kind: 'initial' }, 4), FinalApiError)
  assert.throws(() => parseFinalRead({ ...READ, reader: '' }, 4), FinalApiError)
  assert.throws(() => parseFinalRead({ ...READ, reader: '   ' }, 4), FinalApiError)
  assert.throws(() => parseFinalRead({ ...READ, reader: 7 }, 4), FinalApiError)
  assert.throws(() => parseFinalRead({ ...READ, reader: null }, 4), FinalApiError)
  assert.throws(() => parseFinalRead([MARK], 4), FinalApiError)

  // An accepted decision without a linked mark is preserved, not manufactured.
  const acceptedOnly = parseFinalRead({ ...READ, marks: [], candidate_decisions: [DECISION] }, 4)
  assert.deepEqual(acceptedOnly.marks, [])
  assert.equal(acceptedOnly.candidate_decisions[0].candidate_id, 'c1')

  // A null-normal final record is preserved as null, not defaulted.
  const nullNormal = parseFinalRead({ ...READ, marks: [{ ...MARK, view_plane_normal: null }], candidate_decisions: [] }, 4)
  assert.equal(nullNormal.marks[0].view_plane_normal, null)
})

test('rejectsInvalidLinkageAndDismissRules', () => {
  const linked = { ...MARK, origin: 'from_ai_candidate', ai_candidate_id: 'c1' }
  const accepted = { candidate_id: 'c1', decision: 'accepted', dismiss_reason: null }

  // A candidate-linked mark needs its accepted decision in the same payload;
  // the parser still preserves a reader mark that merely lacks the origin.
  assert.throws(() => parseFinalRead({ ...READ, marks: [{ ...MARK, ai_candidate_id: 'c1' }], candidate_decisions: [] }, 4), FinalApiError)
  assert.throws(
    () => parseFinalRead({ ...READ, marks: [linked, { ...linked, p1_mm: [1, 1, 1] }], candidate_decisions: [accepted] }, 4),
    FinalApiError,
  )
  assert.throws(() => parseFinalRead({ ...READ, marks: [linked], candidate_decisions: [] }, 4), FinalApiError)
  assert.throws(
    () => parseFinalRead({ ...READ, marks: [], candidate_decisions: [{ candidate_id: 'c1', decision: 'dismissed', dismiss_reason: '   ' }] }, 4),
    FinalApiError,
  )
  assert.throws(
    () => parseFinalRead({ ...READ, marks: [], candidate_decisions: [{ candidate_id: 'c1', decision: 'accepted', dismiss_reason: 'why' }] }, 4),
    FinalApiError,
  )
  // Duplicate decision candidate ids are rejected.
  assert.throws(
    () => parseFinalRead({ ...READ, marks: [], candidate_decisions: [DECISION, DECISION] }, 4),
    FinalApiError,
  )

  // A dismissal reason is preserved verbatim (only trimmed for the check).
  const reason = '  bờ nốt  '
  const preserved = parseFinalRead(
    { ...READ, marks: [], candidate_decisions: [{ candidate_id: 'c1', decision: 'dismissed', dismiss_reason: reason }] },
    4,
  )
  assert.equal(preserved.candidate_decisions[0].dismiss_reason, reason)
})

test('parsesFinalizeResult', () => {
  assert.deepEqual(parseFinalizeResult({ status: 'finalized', finalized_at: '2026-10-11T02:00:00Z', ai_status: 'queued' }), {
    status: 'finalized',
    finalized_at: '2026-10-11T02:00:00Z',
    ai_status: 'queued',
  })
  assert.throws(() => parseFinalizeResult({ status: 'finalized', finalized_at: '', ai_status: 'queued' }), FinalApiError)
  assert.throws(() => parseFinalizeResult({ status: 'finalized', finalized_at: 'x', ai_status: 'weird' }), FinalApiError)
  assert.throws(() => parseFinalizeResult({ status: 'ok', finalized_at: 'x', ai_status: 'queued' }), FinalApiError)
})
