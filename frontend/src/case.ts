// Case metadata contract for the axial viewer. The wire shape is the LC1-10
// allowlist (snake_case, eight keys); only those fields are read and anything
// else the backend may later add -- including any AI field -- is ignored.

import { authenticatedFetch } from './auth/session.ts'

export type CaseState = 'awaiting_read' | 'initial_locked' | 'finalized'

export interface CaseMetadata {
  id: number
  source: string
  studyInstanceUID: string
  seriesInstanceUID: string
  frameOfReferenceUID: string
  status: CaseState
  createdAt: string
  finalizedAt: string | null
}

const CASE_STATES: readonly CaseState[] = ['awaiting_read', 'initial_locked', 'finalized']

const UID_PATTERN = /^[0-9]+(\.[0-9]+)*$/

// DICOM UID syntax: dotted decimal components, no empty component, no leading
// zero on a multi-digit component, at most 64 characters.
export function isValidUID(value: unknown): value is string {
  if (typeof value !== 'string' || value.length === 0 || value.length > 64 || !UID_PATTERN.test(value)) {
    return false
  }
  return value.split('.').every((component) => component.length === 1 || !component.startsWith('0'))
}

export class CaseError extends Error {
  readonly status: number | null

  constructor(message: string, status: number | null = null) {
    super(message)
    this.name = 'CaseError'
    this.status = status
  }
}

function requireUID(body: Record<string, unknown>, key: string, label: string): string {
  const value = body[key]
  if (!isValidUID(value)) {
    throw new CaseError(`case ${label} UID is not valid DICOM`)
  }
  return value
}

// Accepts only the documented case contract and requires the row to be the one
// that was requested, so a misrouted or cached response cannot become a case.
export function parseCaseMetadata(payload: unknown, expectedId: number): CaseMetadata {
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
    throw new CaseError('case metadata response is not an object')
  }
  const body = payload as Record<string, unknown>
  if (typeof body.id !== 'number' || !Number.isInteger(body.id) || body.id !== expectedId) {
    throw new CaseError('case metadata id does not match the requested case')
  }
  return parseCaseBody(body)
}

// One row of the owned-case list: the same allowlist as the single-case
// response, validated without a requested id to compare against.
export function parseCaseSummary(payload: unknown): CaseMetadata {
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
    throw new CaseError('case list entry is not an object')
  }
  return parseCaseBody(payload as Record<string, unknown>)
}

function parseCaseBody(body: Record<string, unknown>): CaseMetadata {
  if (typeof body.id !== 'number' || !Number.isInteger(body.id) || body.id <= 0) {
    throw new CaseError('case id is not a positive integer')
  }
  if (typeof body.source !== 'string' || body.source.length === 0) {
    throw new CaseError('case source is missing')
  }
  const studyInstanceUID = requireUID(body, 'study_instance_uid', 'study')
  const seriesInstanceUID = requireUID(body, 'series_instance_uid', 'series')
  const frameOfReferenceUID = requireUID(body, 'frame_of_reference_uid', 'frame of reference')
  const status = body.status
  if (typeof status !== 'string' || !CASE_STATES.includes(status as CaseState)) {
    throw new CaseError('case status is not a known state')
  }
  if (typeof body.created_at !== 'string') {
    throw new CaseError('case created_at is missing')
  }
  const finalizedAt = body.finalized_at
  if (finalizedAt !== null && typeof finalizedAt !== 'string') {
    throw new CaseError('case finalized_at is not a timestamp or null')
  }
  return {
    id: body.id,
    source: body.source,
    studyInstanceUID,
    seriesInstanceUID,
    frameOfReferenceUID,
    status: status as CaseState,
    createdAt: body.created_at,
    finalizedAt,
  }
}

export const CASE_REQUEST_TIMEOUT_MS = 5000

// One bounded request for one case row. The caller owns the outer lifetime and
// cancels through `signal`; the internal timeout only bounds a hung request.
export async function loadCase(caseId: number, signal: AbortSignal): Promise<CaseMetadata> {
  if (!Number.isInteger(caseId) || caseId <= 0) {
    throw new CaseError('case id is not a positive integer')
  }
  const controller = new AbortController()
  const forwardAbort = (): void => controller.abort()
  if (signal.aborted) {
    throw new CaseError('case request canceled before start')
  }
  signal.addEventListener('abort', forwardAbort, { once: true })
  const timeoutId = setTimeout(() => controller.abort(), CASE_REQUEST_TIMEOUT_MS)
  try {
    const response = await authenticatedFetch(`/api/cases/${caseId}`, {
      signal: controller.signal,
      headers: { Accept: 'application/json' },
      cache: 'no-store',
    })
    if (!response.ok) {
      throw new CaseError('case request failed', response.status)
    }
    let payload: unknown
    try {
      payload = (await response.json()) as unknown
    } catch {
      throw new CaseError('case response was not valid JSON')
    }
    return parseCaseMetadata(payload, caseId)
  } finally {
    clearTimeout(timeoutId)
    signal.removeEventListener('abort', forwardAbort)
  }
}
