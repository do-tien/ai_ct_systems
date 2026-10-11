// Initial-read wire boundary: strict payload parsers plus bounded relative
// requests against the existing Go routes. No AI, final or DICOM fields are
// read, and no case loader is duplicated here. The bounded request helper is
// shared with the final read; only this read's parsers live here.

import type { Vec3 } from '../viewer/series.ts'
import { ReadApiError, isFiniteNumber, isRecord, requestReadJson } from '../reads/http.ts'
import type { InitialMark, InitialRead, SubmitResult } from './types.ts'

// The same 5-second client budget shared by every read request.
export const INITIAL_REQUEST_TIMEOUT_MS = 5000

// Wire errors are a read error; the class exists so the initial read keeps a
// stable, module-owned name in tests while the transport helper stays shared.
export class InitialApiError extends ReadApiError {}

function parseVec3(payload: unknown, label: string): Vec3 {
  if (!Array.isArray(payload) || payload.length !== 3 || !payload.every(isFiniteNumber)) {
    throw new InitialApiError(`${label} is not a finite triple`)
  }
  return [payload[0], payload[1], payload[2]]
}

function parseNote(value: unknown): string | null {
  if (value === undefined || value === null) {
    return null
  }
  if (typeof value !== 'string') {
    throw new InitialApiError('mark note is not a string or null')
  }
  return value
}

// Selects only the clinical wire fields; rejects any non-reader origin or
// candidate-linked mark so an AI or final mark can never enter the initial
// draft. Unknown extra keys are ignored.
function parseInitialMark(payload: unknown, index: number): InitialMark {
  if (!isRecord(payload)) {
    throw new InitialApiError(`mark ${index} is not an object`)
  }
  if (payload.origin !== 'reader') {
    throw new InitialApiError(`mark ${index} is not a reader mark`)
  }
  if (payload.ai_candidate_id !== null && payload.ai_candidate_id !== undefined) {
    throw new InitialApiError(`mark ${index} is linked to an AI candidate`)
  }
  const diameter = payload.diameter_mm
  if (!isFiniteNumber(diameter) || diameter <= 0) {
    throw new InitialApiError(`mark ${index} diameter is not a positive number`)
  }
  const p1 = parseVec3(payload.p1_mm, `mark ${index} p1_mm`)
  const p2 = parseVec3(payload.p2_mm, `mark ${index} p2_mm`)
  if (p1[0] === p2[0] && p1[1] === p2[1] && p1[2] === p2[2]) {
    throw new InitialApiError(`mark ${index} endpoints are identical`)
  }
  const normal = parseVec3(payload.view_plane_normal, `mark ${index} view_plane_normal`)
  if (normal[0] === 0 && normal[1] === 0 && normal[2] === 0) {
    throw new InitialApiError(`mark ${index} view_plane_normal is the zero vector`)
  }
  return {
    p1_mm: p1,
    p2_mm: p2,
    center_mm: parseVec3(payload.center_mm, `mark ${index} center_mm`),
    diameter_mm: diameter,
    origin: 'reader',
    ai_candidate_id: null,
    view_plane_normal: normal,
    note: parseNote(payload.note),
  }
}

function parseMarkArray(payload: unknown, label: string): InitialMark[] {
  if (!Array.isArray(payload)) {
    throw new InitialApiError(`${label} is not a mark array`)
  }
  return payload.map((mark, index) => parseInitialMark(mark, index))
}

export function parseInitialMarks(payload: unknown): InitialMark[] {
  return parseMarkArray(payload, 'marks response')
}

export function parseInitialRead(payload: unknown, caseId: number): InitialRead {
  if (!isRecord(payload)) {
    throw new InitialApiError('initial read response is not an object')
  }
  if (!Number.isInteger(payload.id) || (payload.id as number) <= 0) {
    throw new InitialApiError('initial read id is not a positive integer')
  }
  if (payload.case_id !== caseId) {
    throw new InitialApiError('initial read does not belong to the requested case')
  }
  if (payload.kind !== 'initial' || payload.reader !== 'reader') {
    throw new InitialApiError('initial read identity is unexpected')
  }
  const lockedAt = payload.locked_at
  if (lockedAt !== null && typeof lockedAt !== 'string') {
    throw new InitialApiError('initial read locked_at is not a timestamp or null')
  }
  return {
    id: payload.id as number,
    case_id: caseId,
    kind: 'initial',
    reader: 'reader',
    locked_at: lockedAt,
    marks: parseMarkArray(payload.marks, 'initial read marks'),
  }
}

export function parseSubmitResult(payload: unknown): SubmitResult {
  if (!isRecord(payload)) {
    throw new InitialApiError('submit response is not an object')
  }
  if (payload.status !== 'initial_locked') {
    throw new InitialApiError('submit response status is unexpected')
  }
  if (typeof payload.locked_at !== 'string' || payload.locked_at.length === 0) {
    throw new InitialApiError('submit response locked_at is missing')
  }
  if (!Number.isInteger(payload.final_read_id) || (payload.final_read_id as number) <= 0) {
    throw new InitialApiError('submit response final_read_id is not a positive integer')
  }
  return {
    status: 'initial_locked',
    locked_at: payload.locked_at,
    final_read_id: payload.final_read_id as number,
  }
}

export async function loadInitialRead(caseId: number, signal: AbortSignal): Promise<InitialRead> {
  const payload = await requestReadJson(`/api/cases/${caseId}/reads/initial`, {
    method: 'GET',
    signal,
    uncertainOnMalformed: false,
  })
  return parseInitialRead(payload, caseId)
}

export async function replaceInitialMarks(
  caseId: number,
  marks: readonly InitialMark[],
  signal: AbortSignal,
): Promise<InitialMark[]> {
  const payload = await requestReadJson(`/api/cases/${caseId}/reads/initial/marks`, {
    method: 'PUT',
    body: JSON.stringify({ marks }),
    signal,
    uncertainOnMalformed: true,
  })
  return parseInitialMarks(payload)
}

export async function submitInitialRead(caseId: number, signal: AbortSignal): Promise<SubmitResult> {
  const payload = await requestReadJson(`/api/cases/${caseId}/reads/initial/submit`, {
    method: 'POST',
    signal,
    uncertainOnMalformed: true,
  })
  return parseSubmitResult(payload)
}
