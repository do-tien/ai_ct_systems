// Owned-case list contract for the dashboard. The envelope carries the same
// allowlisted rows as GET /api/cases/{id}; every row is validated and duplicate
// ids are rejected so a broken response cannot render twice.

import { authenticatedFetch } from './auth/session.ts'
import { CaseError, parseCaseSummary } from './case.ts'
import type { CaseMetadata } from './case.ts'

const LIST_REQUEST_TIMEOUT_MS = 5000

export function parseCaseList(payload: unknown): CaseMetadata[] {
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
    throw new CaseError('case list response is not an object')
  }
  const body = payload as Record<string, unknown>
  if (!Array.isArray(body.cases)) {
    throw new CaseError('case list response has no cases array')
  }
  const seen = new Set<number>()
  return body.cases.map((entry) => {
    const summary = parseCaseSummary(entry)
    if (seen.has(summary.id)) {
      throw new CaseError('case list response repeats a case id')
    }
    seen.add(summary.id)
    return summary
  })
}

// One bounded request for the authenticated reader's cases. The caller owns the
// outer lifetime and cancels through `signal`; the internal timeout only bounds
// a hung request.
export async function loadCases(signal: AbortSignal): Promise<CaseMetadata[]> {
  if (signal.aborted) {
    throw new CaseError('case list request canceled before start')
  }
  const controller = new AbortController()
  const forwardAbort = (): void => controller.abort()
  signal.addEventListener('abort', forwardAbort, { once: true })
  const timeoutId = setTimeout(() => controller.abort(), LIST_REQUEST_TIMEOUT_MS)
  try {
    const response = await authenticatedFetch('/api/cases', {
      signal: controller.signal,
      headers: { Accept: 'application/json' },
      cache: 'no-store',
    })
    if (!response.ok) {
      throw new CaseError('case list request failed', response.status)
    }
    let payload: unknown
    try {
      payload = (await response.json()) as unknown
    } catch {
      throw new CaseError('case list response was not valid JSON')
    }
    return parseCaseList(payload)
  } finally {
    clearTimeout(timeoutId)
    signal.removeEventListener('abort', forwardAbort)
  }
}
