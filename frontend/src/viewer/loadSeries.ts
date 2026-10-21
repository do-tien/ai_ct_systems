// Same-origin acquisition of one series: the bounded QIDO instance inventory
// plus the authoritative WADO series metadata, handed to the pure validator.
// Only GET requests reach the existing Go proxy; nothing else is contacted.

import type { CaseMetadata } from '../case.ts'
import { authenticatedFetch } from '../auth/session.ts'
import { SeriesError, buildSeriesPlan } from './series.ts'
import type { SeriesPlan } from './series.ts'

const PAGE_LIMIT = 200
const MAX_PAGES = 50
// Metadata requests are bounded per request; frame transfers are governed by
// the proxy budget and the loader's own XHR timeout.
export const METADATA_REQUEST_TIMEOUT_MS = 30000

async function requestJson(url: string, signal: AbortSignal, timeoutMs: number): Promise<unknown> {
  const controller = new AbortController()
  const forwardAbort = (): void => controller.abort()
  if (signal.aborted) {
    throw new SeriesError('series request canceled before start')
  }
  signal.addEventListener('abort', forwardAbort, { once: true })
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await authenticatedFetch(url, {
      signal: controller.signal,
      headers: { Accept: 'application/dicom+json' },
      cache: 'no-store',
    })
    if (!response.ok) {
      throw new SeriesError(`DICOMweb request failed with status ${response.status}`)
    }
    return (await response.json()) as unknown
  } catch (error) {
    if (signal.aborted) {
      throw new SeriesError('series request canceled')
    }
    if (controller.signal.aborted) {
      throw new SeriesError('DICOMweb request timed out')
    }
    if (error instanceof SeriesError) {
      throw error
    }
    throw new SeriesError('DICOMweb request could not be completed')
  } finally {
    clearTimeout(timeoutId)
    signal.removeEventListener('abort', forwardAbort)
  }
}

export async function loadSeries(caseMetadata: CaseMetadata, signal: AbortSignal): Promise<SeriesPlan> {
  const origin = window.location.origin
  const { studyInstanceUID: study, seriesInstanceUID: series } = caseMetadata
  const instances: unknown[] = []
  for (let page = 0; page < MAX_PAGES; page += 1) {
    const offset = instances.length
    const url = `/dicom-web/studies/${study}/series/${series}/instances?limit=${PAGE_LIMIT}&offset=${offset}`
    const payload = await requestJson(url, signal, METADATA_REQUEST_TIMEOUT_MS)
    if (!Array.isArray(payload)) {
      throw new SeriesError('QIDO response was not an instance list')
    }
    instances.push(...payload)
    if (payload.length < PAGE_LIMIT) {
      break
    }
    if (page === MAX_PAGES - 1) {
      throw new SeriesError('QIDO inventory exceeded the paging bound')
    }
  }
  const metadata = await requestJson(
    `/dicom-web/studies/${study}/series/${series}/metadata`,
    signal,
    METADATA_REQUEST_TIMEOUT_MS,
  )
  return buildSeriesPlan(instances, metadata, caseMetadata, origin)
}
