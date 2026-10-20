// Auth HTTP boundary: a strict parser for the login/session projection plus
// bounded same-origin requests with five-second budgets. Only fixed safe
// Vietnamese messages leave this module; a raw server body and the PIN are
// never logged, stored or echoed back.

import { isRecord } from '../reads/http.ts'
import { isSessionTimestamp } from './session.ts'
import type { AuthSession } from './types.ts'

export const AUTH_REQUEST_TIMEOUT_MS = 5000

export const AUTH_GENERIC_MESSAGE = 'Không xác thực được phiên làm việc. Thử lại sau.'
export const INVALID_PIN_MESSAGE = 'Mã PIN phải gồm đúng 8 chữ số.'
export const INVALID_CREDENTIALS_MESSAGE = 'Mã PIN không đúng.'
export const TOO_MANY_ATTEMPTS_MESSAGE = 'Đã thử sai quá nhiều lần. Vui lòng chờ rồi thử lại.'
export const DEPENDENCY_UNAVAILABLE_MESSAGE = 'Dịch vụ xác thực tạm thời không khả dụng. Thử lại sau.'

// Exactly eight ASCII digits; leading zeroes are part of a valid PIN.
const PIN_PATTERN = /^[0-9]{8}$/

export type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>

export interface AuthRequestDeps {
  fetch?: FetchLike
  setTimeout?: (handler: () => void, delayMs: number) => unknown
  clearTimeout?: (handle: unknown) => void
}

interface ResolvedDeps {
  fetch: FetchLike
  setTimeout: (handler: () => void, delayMs: number) => unknown
  clearTimeout: (handle: unknown) => void
}

export class AuthApiError extends Error {
  readonly status: number | null
  // The caller aborted; nothing should be shown as a failure.
  readonly canceled: boolean
  // The internal five-second budget elapsed.
  readonly timedOut: boolean

  constructor(
    message: string,
    options: { status?: number | null; canceled?: boolean; timedOut?: boolean } = {},
  ) {
    super(message)
    this.name = 'AuthApiError'
    this.status = options.status ?? null
    this.canceled = options.canceled ?? false
    this.timedOut = options.timedOut ?? false
  }
}

function resolveDeps(deps: AuthRequestDeps): ResolvedDeps {
  return {
    fetch: deps.fetch ?? ((input, init) => fetch(input, init)),
    setTimeout: deps.setTimeout ?? ((handler, delayMs) => setTimeout(handler, delayMs)),
    clearTimeout: deps.clearTimeout ?? ((handle) => clearTimeout(handle as number)),
  }
}

// Accepts only the documented projection. A role, token or any other extra
// field is ignored instead of being carried into the UI.
export function parseAuthSession(payload: unknown): AuthSession {
  if (!isRecord(payload)) {
    throw new AuthApiError('auth session response is not an object')
  }
  const user = payload.user
  if (!isRecord(user)) {
    throw new AuthApiError('auth session user is not an object')
  }
  const id = user.id
  if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0) {
    throw new AuthApiError('auth session user id is not a positive integer')
  }
  const displayName = user.display_name
  if (typeof displayName !== 'string' || displayName.trim() === '') {
    throw new AuthApiError('auth session display name is missing')
  }
  const expiresAt = payload.expires_at
  if (typeof expiresAt !== 'string' || !isSessionTimestamp(expiresAt)) {
    throw new AuthApiError('auth session expiry is not an RFC3339 timestamp')
  }
  return { user: { id, displayName }, expiresAt }
}

function messageForStatus(status: number): string {
  if (status === 401) {
    return INVALID_CREDENTIALS_MESSAGE
  }
  if (status === 429) {
    return TOO_MANY_ATTEMPTS_MESSAGE
  }
  if (status === 503) {
    return DEPENDENCY_UNAVAILABLE_MESSAGE
  }
  return AUTH_GENERIC_MESSAGE
}

// One bounded request. A caller abort is reported as canceled and an internal
// budget expiry as timed out; neither ever carries server text.
async function requestAuth(
  path: string,
  init: { method: 'GET' | 'POST'; headers?: Record<string, string>; body?: string },
  signal: AbortSignal,
  deps: ResolvedDeps,
): Promise<Response> {
  if (signal.aborted) {
    throw new AuthApiError(AUTH_GENERIC_MESSAGE, { canceled: true })
  }
  const controller = new AbortController()
  const forwardAbort = (): void => controller.abort()
  signal.addEventListener('abort', forwardAbort, { once: true })
  const timeoutId = deps.setTimeout(() => controller.abort(), AUTH_REQUEST_TIMEOUT_MS)

  try {
    return await deps.fetch(path, {
      method: init.method,
      headers: { Accept: 'application/json', ...init.headers },
      body: init.body,
      cache: 'no-store',
      signal: controller.signal,
    })
  } catch {
    if (signal.aborted) {
      throw new AuthApiError(AUTH_GENERIC_MESSAGE, { canceled: true })
    }
    if (controller.signal.aborted) {
      throw new AuthApiError(AUTH_GENERIC_MESSAGE, { timedOut: true })
    }
    throw new AuthApiError(AUTH_GENERIC_MESSAGE)
  } finally {
    deps.clearTimeout(timeoutId)
    signal.removeEventListener('abort', forwardAbort)
  }
}

async function readSession(response: Response): Promise<AuthSession> {
  let payload: unknown
  try {
    payload = (await response.json()) as unknown
  } catch {
    throw new AuthApiError(AUTH_GENERIC_MESSAGE, { status: response.status })
  }
  try {
    return parseAuthSession(payload)
  } catch {
    throw new AuthApiError(AUTH_GENERIC_MESSAGE, { status: response.status })
  }
}

// A 401 is the anonymous state, not a failure: the app renders the login form.
export async function loadSession(
  signal: AbortSignal,
  deps: AuthRequestDeps = {},
): Promise<AuthSession | null> {
  const resolved = resolveDeps(deps)
  const response = await requestAuth('/api/auth/session', { method: 'GET' }, signal, resolved)
  if (response.status === 401) {
    return null
  }
  if (!response.ok) {
    throw new AuthApiError(messageForStatus(response.status), { status: response.status })
  }
  return readSession(response)
}

// Validates the PIN locally before dispatching so a malformed value never
// leaves the browser. The value travels only in this one request body.
export async function login(
  pin: string,
  signal: AbortSignal,
  deps: AuthRequestDeps = {},
): Promise<AuthSession> {
  if (!PIN_PATTERN.test(pin)) {
    throw new AuthApiError(INVALID_PIN_MESSAGE)
  }
  const resolved = resolveDeps(deps)
  const response = await requestAuth(
    '/api/auth/login',
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pin }),
    },
    signal,
    resolved,
  )
  if (!response.ok) {
    throw new AuthApiError(messageForStatus(response.status), { status: response.status })
  }
  return readSession(response)
}

// Best effort: 204 and 401 both mean logged out, and a network failure or
// cancellation must still retire the local session.
export async function logout(signal: AbortSignal, deps: AuthRequestDeps = {}): Promise<void> {
  const resolved = resolveDeps(deps)
  try {
    await requestAuth('/api/auth/logout', { method: 'POST' }, signal, resolved)
  } catch {
    return
  }
}
