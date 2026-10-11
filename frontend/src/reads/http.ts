// Shared, bounded same-origin JSON request for both clinical reads. Both reads
// use the identical five-second client budget, cancellation forwarding and
// safe-message mapping; only the response parsers differ, and those live with
// each read's own wire module. No raw server message text is ever surfaced.

// The same 5-second client budget the case and viewer requests use.
export const READ_REQUEST_TIMEOUT_MS = 5000

// Safe, neutral Vietnamese messages for the fixed server error codes.
const SAFE_MESSAGES: Record<string, string> = {
  CASE_NOT_FOUND: 'Không tìm thấy ca này.',
  INVALID_REQUEST: 'Dữ liệu gửi lên không hợp lệ.',
  INITIAL_READ_LOCKED: 'Bản đọc đầu đã bị khóa và không sửa được nữa.',
  INITIAL_READ_CONFLICT: 'Trạng thái bản đọc không nhất quán; cần tải lại.',
  INITIAL_READ_REQUIRED: 'Cần khóa bản đọc đầu độc lập trước khi xem dữ liệu AI.',
  FINAL_READ_LOCKED: 'Bản đọc cuối đã khóa và không sửa được nữa.',
  FINAL_READ_CONFLICT: 'Trạng thái bản đọc cuối không nhất quán; cần tải lại.',
  REQUEST_TOO_LARGE: 'Danh sách gửi lên vượt quá giới hạn cho phép.',
  UNSUPPORTED_MEDIA_TYPE: 'Yêu cầu không được máy chủ chấp nhận.',
  DEPENDENCY_UNAVAILABLE: 'Dịch vụ tạm thời không khả dụng. Thử lại sau.',
  COMMIT_OUTCOME_UNKNOWN: 'Chưa xác định được kết quả lưu; cần đối chiếu với máy chủ.',
  INTERNAL_ERROR: 'Máy chủ gặp lỗi khi xử lý yêu cầu.',
}

const GENERIC_MESSAGE = 'Không hoàn tất được thao tác với bản đọc.'

export class ReadApiError extends Error {
  readonly status: number | null
  readonly code: string | null
  // The request was dispatched but no definitive answer arrived (network
  // failure, timeout, malformed success after a write, COMMIT_OUTCOME_UNKNOWN):
  // the server may already have applied the change.
  readonly uncertain: boolean
  // The caller aborted the request; nothing should be shown as a failure.
  readonly canceled: boolean

  constructor(
    message: string,
    options: { status?: number | null; code?: string | null; uncertain?: boolean; canceled?: boolean } = {},
  ) {
    super(message)
    this.name = 'ReadApiError'
    this.status = options.status ?? null
    this.code = options.code ?? null
    this.uncertain = options.uncertain ?? false
    this.canceled = options.canceled ?? false
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}

export function isFiniteVector(value: unknown): value is number[] {
  return Array.isArray(value) && value.length === 3 && value.every(isFiniteNumber)
}

export function parseVec3(payload: unknown, label: string): [number, number, number] {
  if (!isFiniteVector(payload)) {
    throw new ReadApiError(`${label} is not a finite triple`)
  }
  return [payload[0], payload[1], payload[2]]
}

export function parseNote(value: unknown): string | null {
  if (value === undefined || value === null) {
    return null
  }
  if (typeof value !== 'string') {
    throw new ReadApiError('mark note is not a string or null')
  }
  return value
}

function parseErrorCode(payload: unknown): string | null {
  if (!isRecord(payload)) {
    return null
  }
  const error = payload.error
  if (!isRecord(error) || typeof error.code !== 'string') {
    return null
  }
  return error.code
}

export function messageForCode(code: string | null): string {
  if (code !== null && code in SAFE_MESSAGES) {
    return SAFE_MESSAGES[code]
  }
  return GENERIC_MESSAGE
}

export interface ReadRequestOptions {
  method: 'GET' | 'PUT' | 'POST'
  body?: string
  signal: AbortSignal
  // Whether a dispatched-but-unanswered request may already have been applied.
  uncertainOnMalformed: boolean
}

// One bounded request. A caller abort is reported as canceled; a network
// failure, timeout or (for writes) a malformed success is reported as
// uncertain so the workflow can reconcile instead of replaying.
export async function requestReadJson(path: string, options: ReadRequestOptions): Promise<unknown> {
  const controller = new AbortController()
  const forwardAbort = (): void => controller.abort()
  if (options.signal.aborted) {
    throw new ReadApiError('request canceled', { canceled: true })
  }
  options.signal.addEventListener('abort', forwardAbort, { once: true })
  const timeoutId = setTimeout(() => controller.abort(), READ_REQUEST_TIMEOUT_MS)

  const headers: Record<string, string> = { Accept: 'application/json' }
  if (options.method === 'PUT') {
    headers['Content-Type'] = 'application/json'
  }

  let response: Response
  try {
    response = await fetch(path, {
      method: options.method,
      headers,
      body: options.body,
      signal: controller.signal,
      cache: 'no-store',
    })
  } catch {
    clearTimeout(timeoutId)
    options.signal.removeEventListener('abort', forwardAbort)
    if (options.signal.aborted) {
      throw new ReadApiError('request canceled', { canceled: true })
    }
    throw new ReadApiError('network request failed', { uncertain: true })
  }

  let text: string | null = null
  try {
    text = await response.text()
  } catch {
    text = null
  }
  clearTimeout(timeoutId)
  options.signal.removeEventListener('abort', forwardAbort)

  if (!response.ok) {
    let code: string | null = null
    if (text !== null && text.length > 0) {
      try {
        code = parseErrorCode(JSON.parse(text) as unknown)
      } catch {
        code = null
      }
    }
    const uncertain = code === 'COMMIT_OUTCOME_UNKNOWN'
    throw new ReadApiError(messageForCode(code), {
      status: response.status,
      code,
      uncertain,
    })
  }

  if (text === null) {
    throw new ReadApiError('response body could not be read', { uncertain: options.uncertainOnMalformed })
  }
  try {
    return JSON.parse(text) as unknown
  } catch {
    throw new ReadApiError('response was not valid JSON', { uncertain: options.uncertainOnMalformed })
  }
}
