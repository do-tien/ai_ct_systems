// Final-read wire boundary: strict payload parsers plus bounded relative
// requests against the existing LC1-13 Go routes. The AI projection parser is
// state-specific so a succeeded document can never carry an error and a
// failed/queued/running one can never carry candidates.

import type { Vec3 } from '../viewer/series.ts'
import { ReadApiError, isFiniteNumber, isRecord, requestReadJson } from '../reads/http.ts'
import type {
  AICandidateStatus,
  AIProjection,
  Candidate,
  CandidateDecision,
  DecisionKind,
  FinalMark,
  FinalRead,
  FinalReplacement,
  FinalizeResult,
  MarkOrigin,
} from './types.ts'

export class FinalApiError extends ReadApiError {}

const ORIGINS: readonly MarkOrigin[] = ['reader', 'reader_after_ai', 'from_ai_candidate']
const DECISIONS: readonly DecisionKind[] = ['accepted', 'dismissed']
const AI_STATUSES: readonly AICandidateStatus[] = ['queued', 'running', 'succeeded', 'failed']

function parseVec3(payload: unknown, label: string): Vec3 {
  if (!Array.isArray(payload) || payload.length !== 3 || !payload.every(isFiniteNumber)) {
    throw new FinalApiError(`${label} is not a finite triple`)
  }
  return [payload[0], payload[1], payload[2]]
}

function parsePositiveVec3(payload: unknown, label: string): Vec3 {
  const vector = parseVec3(payload, label)
  if (!(vector[0] > 0 && vector[1] > 0 && vector[2] > 0)) {
    throw new FinalApiError(`${label} is not a positive triple`)
  }
  return vector
}

function parseFinalMark(payload: unknown, index: number): FinalMark {
  if (!isRecord(payload)) {
    throw new FinalApiError(`mark ${index} is not an object`)
  }
  const origin = payload.origin
  if (typeof origin !== 'string' || !ORIGINS.includes(origin as MarkOrigin)) {
    throw new FinalApiError(`mark ${index} origin is unexpected`)
  }
  const candidateId = payload.ai_candidate_id
  let aiCandidateId: string | null
  if (origin === 'from_ai_candidate') {
    if (typeof candidateId !== 'string' || candidateId.length === 0) {
      throw new FinalApiError(`mark ${index} candidate-linked mark has no candidate id`)
    }
    aiCandidateId = candidateId
  } else {
    if (candidateId !== null && candidateId !== undefined) {
      throw new FinalApiError(`mark ${index} is linked to a candidate without the candidate origin`)
    }
    aiCandidateId = null
  }
  const diameter = payload.diameter_mm
  if (!isFiniteNumber(diameter) || diameter <= 0) {
    throw new FinalApiError(`mark ${index} diameter is not a positive number`)
  }
  const p1 = parseVec3(payload.p1_mm, `mark ${index} p1_mm`)
  const p2 = parseVec3(payload.p2_mm, `mark ${index} p2_mm`)
  if (p1[0] === p2[0] && p1[1] === p2[1] && p1[2] === p2[2]) {
    throw new FinalApiError(`mark ${index} endpoints are identical`)
  }
  let normal: Vec3 | null = null
  if (payload.view_plane_normal !== null && payload.view_plane_normal !== undefined) {
    normal = parseVec3(payload.view_plane_normal, `mark ${index} view_plane_normal`)
    if (normal[0] === 0 && normal[1] === 0 && normal[2] === 0) {
      throw new FinalApiError(`mark ${index} view_plane_normal is the zero vector`)
    }
  }
  let note: string | null = null
  if (payload.note !== null && payload.note !== undefined) {
    if (typeof payload.note !== 'string') {
      throw new FinalApiError(`mark ${index} note is not a string or null`)
    }
    note = payload.note
  }
  return {
    p1_mm: p1,
    p2_mm: p2,
    center_mm: parseVec3(payload.center_mm, `mark ${index} center_mm`),
    diameter_mm: diameter,
    origin: origin as MarkOrigin,
    ai_candidate_id: aiCandidateId,
    view_plane_normal: normal,
    note,
  }
}

function parseDecision(payload: unknown, index: number, seen: Set<string>): CandidateDecision {
  if (!isRecord(payload)) {
    throw new FinalApiError(`decision ${index} is not an object`)
  }
  const candidateId = payload.candidate_id
  if (typeof candidateId !== 'string' || candidateId.length === 0) {
    throw new FinalApiError(`decision ${index} has no candidate id`)
  }
  if (seen.has(candidateId)) {
    throw new FinalApiError(`decision ${index} duplicates a candidate id`)
  }
  seen.add(candidateId)
  const decision = payload.decision
  if (typeof decision !== 'string' || !DECISIONS.includes(decision as DecisionKind)) {
    throw new FinalApiError(`decision ${index} kind is unexpected`)
  }
  const reason = payload.dismiss_reason
  if (decision === 'accepted') {
    if (reason !== null && reason !== undefined) {
      throw new FinalApiError(`decision ${index} accepted with a reason`)
    }
    return { candidate_id: candidateId, decision: 'accepted', dismiss_reason: null }
  }
  if (typeof reason !== 'string' || reason.trim().length === 0) {
    throw new FinalApiError(`decision ${index} dismissed without a reason`)
  }
  return { candidate_id: candidateId, decision: 'dismissed', dismiss_reason: reason }
}

export function parseFinalRead(payload: unknown, caseId: number): FinalRead {
  if (!isRecord(payload)) {
    throw new FinalApiError('final read response is not an object')
  }
  if (!Number.isInteger(payload.id) || (payload.id as number) <= 0) {
    throw new FinalApiError('final read id is not a positive integer')
  }
  if (payload.case_id !== caseId) {
    throw new FinalApiError('final read does not belong to the requested case')
  }
  if (payload.kind !== 'final' || payload.reader !== 'reader') {
    throw new FinalApiError('final read identity is unexpected')
  }
  const lockedAt = payload.locked_at
  if (lockedAt !== null && typeof lockedAt !== 'string') {
    throw new FinalApiError('final read locked_at is not a timestamp or null')
  }
  if (!Array.isArray(payload.marks)) {
    throw new FinalApiError('final read marks is not an array')
  }
  if (!Array.isArray(payload.candidate_decisions)) {
    throw new FinalApiError('final read candidate_decisions is not an array')
  }
  const seen = new Set<string>()
  const decisions = payload.candidate_decisions.map((decision, index) => parseDecision(decision, index, seen))
  const accepted = new Set(
    decisions.filter((decision) => decision.decision === 'accepted').map((decision) => decision.candidate_id),
  )
  const linked = new Set<string>()
  const marks = payload.marks.map((mark, index) => {
    const parsed = parseFinalMark(mark, index)
    if (parsed.ai_candidate_id !== null) {
      if (!accepted.has(parsed.ai_candidate_id)) {
        throw new FinalApiError(`mark ${index} links a candidate without an accepted decision`)
      }
      if (linked.has(parsed.ai_candidate_id)) {
        throw new FinalApiError(`mark ${index} duplicates a candidate link`)
      }
      linked.add(parsed.ai_candidate_id)
    }
    return parsed
  })
  return {
    id: payload.id as number,
    case_id: caseId,
    kind: 'final',
    reader: 'reader',
    locked_at: lockedAt,
    marks,
    candidate_decisions: decisions,
  }
}

function parseCandidate(payload: unknown, index: number, seen: Set<string>): Candidate {
  if (!isRecord(payload)) {
    throw new FinalApiError(`candidate ${index} is not an object`)
  }
  const id = payload.id
  if (typeof id !== 'string' || id.length === 0) {
    throw new FinalApiError(`candidate ${index} has no id`)
  }
  if (seen.has(id)) {
    throw new FinalApiError(`candidate ${index} duplicates an id`)
  }
  seen.add(id)
  const score = payload.score
  if (!isFiniteNumber(score) || score < 0 || score > 1) {
    throw new FinalApiError(`candidate ${index} score is outside [0,1]`)
  }
  return {
    id,
    center_mm: parseVec3(payload.center_mm, `candidate ${index} center_mm`),
    size_mm: parsePositiveVec3(payload.size_mm, `candidate ${index} size_mm`),
    score,
  }
}

function parseModel(payload: unknown): { bundle: string; bundle_version: string; score_threshold: number } {
  if (!isRecord(payload) || typeof payload.bundle !== 'string' || typeof payload.bundle_version !== 'string') {
    throw new FinalApiError('succeeded model provenance is malformed')
  }
  if (!isFiniteNumber(payload.score_threshold)) {
    throw new FinalApiError('succeeded model score_threshold is not a number')
  }
  return { bundle: payload.bundle, bundle_version: payload.bundle_version, score_threshold: payload.score_threshold }
}

function parseInput(payload: unknown): { num_slices: number; spacing_mm: number[]; size_voxels: number[] } {
  if (!isRecord(payload)) {
    throw new FinalApiError('succeeded input provenance is malformed')
  }
  const { num_slices: numSlices, spacing_mm: spacing, size_voxels: sizeVoxels } = payload
  if (!isFiniteNumber(numSlices) || !Number.isInteger(numSlices)) {
    throw new FinalApiError('succeeded num_slices is not an integer')
  }
  if (!Array.isArray(spacing) || !spacing.every(isFiniteNumber)) {
    throw new FinalApiError('succeeded spacing_mm is malformed')
  }
  if (!Array.isArray(sizeVoxels) || !sizeVoxels.every((value) => isFiniteNumber(value) && Number.isInteger(value))) {
    throw new FinalApiError('succeeded size_voxels is malformed')
  }
  return { num_slices: numSlices, spacing_mm: [...(spacing as number[])], size_voxels: [...(sizeVoxels as number[])] }
}

function parseTiming(payload: unknown): { load: number; inference: number; total: number } {
  if (!isRecord(payload)) {
    throw new FinalApiError('succeeded timing_s is malformed')
  }
  const { load, inference, total } = payload
  if (!isFiniteNumber(load) || !isFiniteNumber(inference) || !isFiniteNumber(total)) {
    throw new FinalApiError('succeeded timing_s is not numeric')
  }
  return { load, inference, total }
}

// Parses the state-specific AI projection. Pending states require null
// candidates/error; failed requires an empty candidate list and a fixed error;
// succeeded requires the full allowlisted provenance and no error member.
export function parseAI(payload: unknown): AIProjection {
  if (!isRecord(payload)) {
    throw new FinalApiError('AI response is not an object')
  }
  const status = payload.status
  if (typeof status !== 'string' || !AI_STATUSES.includes(status as AICandidateStatus)) {
    throw new FinalApiError('AI status is unexpected')
  }
  if (status === 'queued' || status === 'running') {
    if (payload.candidates !== null) {
      throw new FinalApiError('pending AI response carries candidates')
    }
    if (payload.error !== null) {
      throw new FinalApiError('pending AI response carries an error')
    }
    return { status, candidates: null, error: null }
  }
  if (status === 'failed') {
    if (!Array.isArray(payload.candidates) || payload.candidates.length !== 0) {
      throw new FinalApiError('failed AI response carries candidates')
    }
    const error = payload.error
    if (!isRecord(error) || typeof error.code !== 'string' || typeof error.message !== 'string') {
      throw new FinalApiError('failed AI response has no fixed error')
    }
    return { status: 'failed', candidates: [], error: { code: error.code, message: error.message } }
  }
  if (!Array.isArray(payload.candidates)) {
    throw new FinalApiError('succeeded AI response has no candidate list')
  }
  const seen = new Set<string>()
  const candidates = payload.candidates.map((candidate, index) => parseCandidate(candidate, index, seen))
  if (!isRecord(payload.resources) || typeof payload.resources.device !== 'string') {
    throw new FinalApiError('succeeded resources provenance is malformed')
  }
  return {
    status: 'succeeded',
    candidates,
    model: parseModel(payload.model),
    input: parseInput(payload.input),
    timing_s: parseTiming(payload.timing_s),
    resources: { device: payload.resources.device },
  }
}

export function parseFinalizeResult(payload: unknown): FinalizeResult {
  if (!isRecord(payload)) {
    throw new FinalApiError('finalize response is not an object')
  }
  if (payload.status !== 'finalized') {
    throw new FinalApiError('finalize response status is unexpected')
  }
  if (typeof payload.finalized_at !== 'string' || payload.finalized_at.length === 0) {
    throw new FinalApiError('finalize response finalized_at is missing')
  }
  const aiStatus = payload.ai_status
  if (typeof aiStatus !== 'string' || !AI_STATUSES.includes(aiStatus as AICandidateStatus)) {
    throw new FinalApiError('finalize response ai_status is unexpected')
  }
  return { status: 'finalized', finalized_at: payload.finalized_at, ai_status: aiStatus as AICandidateStatus }
}

// An explicit payload so a null normal and a null candidate id are written as
// JSON null, never dropped.
function serializeReplacement(value: FinalReplacement): string {
  return JSON.stringify({
    marks: value.marks.map((mark) => ({
      p1_mm: mark.p1_mm,
      p2_mm: mark.p2_mm,
      center_mm: mark.center_mm,
      diameter_mm: mark.diameter_mm,
      origin: mark.origin,
      ai_candidate_id: mark.ai_candidate_id,
      view_plane_normal: mark.view_plane_normal,
      note: mark.note,
    })),
    candidate_decisions: value.candidate_decisions.map((decision) => ({
      candidate_id: decision.candidate_id,
      decision: decision.decision,
      dismiss_reason: decision.dismiss_reason,
    })),
  })
}

export async function loadFinalRead(caseId: number, signal: AbortSignal): Promise<FinalRead> {
  const payload = await requestReadJson(`/api/cases/${caseId}/reads/final`, {
    method: 'GET',
    signal,
    uncertainOnMalformed: false,
  })
  return parseFinalRead(payload, caseId)
}

export async function loadAI(caseId: number, signal: AbortSignal): Promise<AIProjection> {
  const payload = await requestReadJson(`/api/cases/${caseId}/ai`, {
    method: 'GET',
    signal,
    uncertainOnMalformed: false,
  })
  return parseAI(payload)
}

export async function replaceFinalRead(
  caseId: number,
  value: FinalReplacement,
  signal: AbortSignal,
): Promise<FinalRead> {
  const payload = await requestReadJson(`/api/cases/${caseId}/reads/final`, {
    method: 'PUT',
    body: serializeReplacement(value),
    signal,
    uncertainOnMalformed: true,
  })
  return parseFinalRead(payload, caseId)
}

export async function finalizeCase(caseId: number, signal: AbortSignal): Promise<FinalizeResult> {
  const payload = await requestReadJson(`/api/cases/${caseId}/finalize`, {
    method: 'POST',
    signal,
    uncertainOnMalformed: true,
  })
  return parseFinalizeResult(payload)
}
