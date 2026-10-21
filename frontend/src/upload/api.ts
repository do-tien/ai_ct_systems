// Upload HTTP boundary: strict payload parsers plus bounded relative requests
// against the Go upload routes. The upload itself uses XHR because only XHR
// exposes upload progress; every other call is a short bounded fetch. No raw
// server message, path or upstream body is ever surfaced.

import { isFiniteNumber, isRecord } from '../reads/http.ts'
import type { UploadCaseResult, UploadSeriesOption, UploadSession } from './types.ts'

// The transfer budget the backend also enforces end to end. A real CT batch is
// far larger than a JSON read, so this is deliberately not the 5-second read
// budget.
export const UPLOAD_TIMEOUT_MS = 15 * 60 * 1000
// Confirmation waits for one transaction, never for inference.
export const CONFIRM_TIMEOUT_MS = 60 * 1000
// Session reads and discards are small metadata calls.
export const SESSION_TIMEOUT_MS = 5000

// Safe, neutral Vietnamese messages for the fixed server error codes.
const SAFE_MESSAGES: Record<string, string> = {
  INVALID_REQUEST: 'Yêu cầu tải lên không hợp lệ.',
  INVALID_UPLOAD: 'Bộ dữ liệu tải lên không hợp lệ.',
  ORIGIN_NOT_ALLOWED: 'Yêu cầu không xuất phát từ trang hợp lệ.',
  UPLOAD_NOT_FOUND: 'Không tìm thấy phiên tải lên này.',
  UPLOAD_EXPIRED: 'Phiên tải lên đã hết hạn; cần tải lại dữ liệu.',
  UPLOAD_ALREADY_CONFIRMED: 'Phiên tải lên đã được xác nhận với series khác.',
  UPLOAD_TOO_LARGE: 'Bộ dữ liệu tải lên vượt quá giới hạn cho phép.',
  UNSUPPORTED_UPLOAD_FORMAT: 'Định dạng tải lên không được hỗ trợ.',
  UNSUITABLE_SERIES: 'Series đã chọn không phù hợp để mở ca.',
  UPLOAD_BUSY: 'Đang có một lượt tải lên khác; thử lại sau.',
  UPLOAD_CAPACITY: 'Đã đạt số phiên tải lên tối đa; thử lại sau.',
  DEPENDENCY_UNAVAILABLE: 'Dịch vụ tạm thời không khả dụng. Thử lại sau.',
  COMMIT_OUTCOME_UNKNOWN: 'Chưa xác định được kết quả tạo ca; cần kiểm tra lại.',
}

const GENERIC_MESSAGE = 'Không hoàn tất được thao tác tải lên.'

export class UploadApiError extends Error {
  readonly status: number | null
  readonly code: string | null
  // The request was dispatched but no definitive answer arrived: the server may
  // already have created the case.
  readonly uncertain: boolean
  // The caller aborted the request; nothing should be reported as a failure.
  readonly canceled: boolean

  constructor(
    message: string,
    options: { status?: number | null; code?: string | null; uncertain?: boolean; canceled?: boolean } = {},
  ) {
    super(message)
    this.name = 'UploadApiError'
    this.status = options.status ?? null
    this.code = options.code ?? null
    this.uncertain = options.uncertain ?? false
    this.canceled = options.canceled ?? false
  }
}

export function messageForCode(code: string | null): string {
  if (code !== null && code in SAFE_MESSAGES) {
    return SAFE_MESSAGES[code]
  }
  return GENERIC_MESSAGE
}

function parseSeriesOption(payload: unknown, index: number): UploadSeriesOption {
  if (!isRecord(payload)) {
    throw new UploadApiError(`series option ${index} is not an object`)
  }
  const { id, index: position, modality, instance_count: count, suitable, reason_code: reason } = payload
  if (typeof id !== 'string' || id === '') {
    throw new UploadApiError(`series option ${index} has no session key`)
  }
  if (!isFiniteNumber(position) || !isFiniteNumber(count) || count < 0) {
    throw new UploadApiError(`series option ${index} has invalid numbers`)
  }
  if (typeof modality !== 'string' || typeof suitable !== 'boolean') {
    throw new UploadApiError(`series option ${index} has invalid modality or suitability`)
  }
  if (reason !== null && typeof reason !== 'string') {
    throw new UploadApiError(`series option ${index} has an invalid reason code`)
  }
  return {
    id,
    index: position,
    modality,
    instance_count: count,
    suitable,
    reason_code: reason,
  }
}

// Accepts only the documented session allowlist. Any missing or mistyped field
// is an invalid response so stale metadata is never kept.
export function parseUploadSession(payload: unknown): UploadSession {
  if (!isRecord(payload)) {
    throw new UploadApiError('session response is not an object')
  }
  const { id, expires_at: expiresAt, skipped_files: skipped, series, selected_series_id: selected, case_id: caseId } = payload
  if (typeof id !== 'string' || id === '' || typeof expiresAt !== 'string') {
    throw new UploadApiError('session response has no valid identity')
  }
  if (!isFiniteNumber(skipped) || skipped < 0) {
    throw new UploadApiError('session response has an invalid skip count')
  }
  if (!Array.isArray(series)) {
    throw new UploadApiError('session response has no series list')
  }
  if (selected !== null && typeof selected !== 'string') {
    throw new UploadApiError('session response has an invalid selection')
  }
  if (caseId !== null && (!isFiniteNumber(caseId) || caseId <= 0)) {
    throw new UploadApiError('session response has an invalid case id')
  }
  return {
    id,
    expires_at: expiresAt,
    skipped_files: skipped,
    series: series.map(parseSeriesOption),
    selected_series_id: selected,
    case_id: caseId,
  }
}

// Accepts only the documented confirm result.
export function parseUploadCaseResult(payload: unknown): UploadCaseResult {
  if (!isRecord(payload)) {
    throw new UploadApiError('confirm response is not an object')
  }
  const { case_id: caseId, instance_count: count, existing } = payload
  if (!isFiniteNumber(caseId) || caseId <= 0 || !isFiniteNumber(count) || count < 0) {
    throw new UploadApiError('confirm response has invalid numbers')
  }
  if (typeof existing !== 'boolean') {
    throw new UploadApiError('confirm response has no existing flag')
  }
  return { case_id: caseId, instance_count: count, existing }
}

function errorCodeFrom(text: string | null): string | null {
  if (text === null || text.length === 0) {
    return null
  }
  try {
    const payload: unknown = JSON.parse(text)
    if (isRecord(payload) && isRecord(payload.error) && typeof payload.error.code === 'string') {
      return payload.error.code
    }
  } catch {
    return null
  }
  return null
}

// One bounded JSON request against the upload routes. A caller abort is
// reported as canceled; a network failure, timeout or malformed success after a
// write is reported as uncertain so the workflow reconciles instead of
// replaying.
async function requestUploadJson(
  path: string,
  options: { method: 'GET' | 'POST' | 'DELETE'; signal: AbortSignal; timeoutMs: number; uncertainOnMalformed: boolean; body?: string },
): Promise<unknown> {
  if (options.signal.aborted) {
    throw new UploadApiError('request canceled', { canceled: true })
  }
  const controller = new AbortController()
  const forward = (): void => controller.abort()
  options.signal.addEventListener('abort', forward, { once: true })
  const timeoutId = setTimeout(() => controller.abort(), options.timeoutMs)

  let response: Response
  try {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
    }
    response = await fetch(path, {
      method: options.method,
      headers,
      body: options.body,
      signal: controller.signal,
      cache: 'no-store',
    })
  } catch {
    clearTimeout(timeoutId)
    options.signal.removeEventListener('abort', forward)
    if (options.signal.aborted) {
      throw new UploadApiError('request canceled', { canceled: true })
    }
    throw new UploadApiError('network request failed', { uncertain: options.uncertainOnMalformed })
  }

  let text: string | null = null
  try {
    text = await response.text()
  } catch {
    text = null
  }
  clearTimeout(timeoutId)
  options.signal.removeEventListener('abort', forward)

  if (!response.ok) {
    const code = errorCodeFrom(text)
    throw new UploadApiError(messageForCode(code), {
      status: response.status,
      code,
      uncertain: code === 'COMMIT_OUTCOME_UNKNOWN',
    })
  }
  if (response.status === 204) {
    return null
  }
  if (text === null || text.length === 0) {
    throw new UploadApiError('response body could not be read', { uncertain: options.uncertainOnMalformed })
  }
  try {
    return JSON.parse(text) as unknown
  } catch {
    throw new UploadApiError('response was not valid JSON', { uncertain: options.uncertainOnMalformed })
  }
}

export interface UploadTransfer {
  files: File[]
  signal: AbortSignal
  onProgress: (transferred: number, total: number) => void
}

// Streams every selected file as one repeated `files` field and reports real
// transfer progress. Progress is byte transfer only: the server has not
// finished inspecting the series when it reaches 100%.
export function uploadFiles(transfer: UploadTransfer): Promise<UploadSession> {
  return new Promise<UploadSession>((resolve, reject) => {
    if (transfer.signal.aborted) {
      reject(new UploadApiError('request canceled', { canceled: true }))
      return
    }
    const form = new FormData()
    let total = 0
    for (const file of transfer.files) {
      form.append('files', file, file.name)
      total += file.size
    }

    const request = new XMLHttpRequest()
    request.open('POST', '/api/uploads')
    request.responseType = 'text'
    request.timeout = UPLOAD_TIMEOUT_MS
    request.setRequestHeader('Accept', 'application/json')

    const onAbort = (): void => request.abort()
    transfer.signal.addEventListener('abort', onAbort, { once: true })
    const settle = (): void => transfer.signal.removeEventListener('abort', onAbort)

    request.upload.addEventListener('progress', (event) => {
      if (event.lengthComputable) {
        transfer.onProgress(event.loaded, event.total)
      } else {
        transfer.onProgress(event.loaded, total)
      }
    })
    request.addEventListener('error', () => {
      settle()
      reject(new UploadApiError('network request failed'))
    })
    request.addEventListener('timeout', () => {
      settle()
      reject(new UploadApiError('upload timed out'))
    })
    request.addEventListener('abort', () => {
      settle()
      reject(new UploadApiError('request canceled', { canceled: true }))
    })
    request.addEventListener('load', () => {
      settle()
      const text = typeof request.response === 'string' ? request.response : null
      if (request.status < 200 || request.status >= 300) {
        const code = errorCodeFrom(text)
        reject(new UploadApiError(messageForCode(code), { status: request.status, code }))
        return
      }
      try {
        resolve(parseUploadSession(text === null ? null : JSON.parse(text)))
      } catch (error) {
        reject(error instanceof UploadApiError ? error : new UploadApiError('response was not valid JSON'))
      }
    })

    request.send(form)
  })
}

export async function loadUploadSession(id: string, signal: AbortSignal): Promise<UploadSession> {
  const payload = await requestUploadJson(`/api/uploads/${encodeURIComponent(id)}`, {
    method: 'GET',
    signal,
    timeoutMs: SESSION_TIMEOUT_MS,
    uncertainOnMalformed: false,
  })
  return parseUploadSession(payload)
}

export async function confirmUploadSeries(id: string, seriesId: string, signal: AbortSignal): Promise<UploadCaseResult> {
  const payload = await requestUploadJson(`/api/uploads/${encodeURIComponent(id)}/cases`, {
    method: 'POST',
    signal,
    timeoutMs: CONFIRM_TIMEOUT_MS,
    uncertainOnMalformed: true,
    body: JSON.stringify({ series_id: seriesId }),
  })
  return parseUploadCaseResult(payload)
}

export async function discardUploadSession(id: string, signal: AbortSignal): Promise<void> {
  await requestUploadJson(`/api/uploads/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    signal,
    timeoutMs: SESSION_TIMEOUT_MS,
    uncertainOnMalformed: false,
  })
}
