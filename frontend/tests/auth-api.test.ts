import test from 'node:test'
import assert from 'node:assert/strict'

import {
  AUTH_GENERIC_MESSAGE,
  AUTH_REQUEST_TIMEOUT_MS,
  AuthApiError,
  DEPENDENCY_UNAVAILABLE_MESSAGE,
  INVALID_CREDENTIALS_MESSAGE,
  INVALID_PIN_MESSAGE,
  TOO_MANY_ATTEMPTS_MESSAGE,
  loadSession,
  login,
  logout,
  parseAuthSession,
} from '../src/auth/api.ts'
import type { AuthRequestDeps } from '../src/auth/api.ts'

const SESSION_BODY = {
  user: { id: 1, display_name: 'Bác sĩ An' },
  expires_at: '2026-10-21T12:00:00Z',
}

interface Recorded {
  input: RequestInfo | URL
  init?: RequestInit
}

function header(init: RequestInit | undefined, name: string): string | null {
  const headers = init?.headers
  if (headers === undefined) {
    return null
  }
  if (headers instanceof Headers) {
    return headers.get(name)
  }
  if (Array.isArray(headers)) {
    return null
  }
  return (headers as Record<string, string>)[name] ?? null
}

function jsonDeps(status: number, body: unknown): { deps: AuthRequestDeps; calls: Recorded[] } {
  const calls: Recorded[] = []
  return {
    calls,
    deps: {
      fetch: async (input, init) => {
        calls.push({ input, init })
        return new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        })
      },
    },
  }
}

function authError(error: unknown): AuthApiError {
  assert.ok(error instanceof AuthApiError, `expected AuthApiError, got ${String(error)}`)
  return error
}

test('parses the documented session allowlist and ignores extra fields', () => {
  const session = parseAuthSession({
    ...SESSION_BODY,
    user: { ...SESSION_BODY.user, role: 'admin' },
    extra: true,
  })
  assert.deepEqual(session, {
    user: { id: 1, displayName: 'Bác sĩ An' },
    expiresAt: '2026-10-21T12:00:00Z',
  })
})

test('accepts an RFC3339 expiry with an offset', () => {
  const session = parseAuthSession({ ...SESSION_BODY, expires_at: '2026-10-21T19:00:00.5+07:00' })
  assert.equal(session.expiresAt, '2026-10-21T19:00:00.5+07:00')
})

test('rejects malformed session payloads', () => {
  const malformed: Array<[string, unknown]> = [
    ['null', null],
    ['array', []],
    ['string', 'session'],
    ['missing user', { expires_at: SESSION_BODY.expires_at }],
    ['user not an object', { user: 'x', expires_at: SESSION_BODY.expires_at }],
    ['zero id', { user: { id: 0, display_name: 'A' }, expires_at: SESSION_BODY.expires_at }],
    ['negative id', { user: { id: -1, display_name: 'A' }, expires_at: SESSION_BODY.expires_at }],
    ['fractional id', { user: { id: 1.5, display_name: 'A' }, expires_at: SESSION_BODY.expires_at }],
    ['string id', { user: { id: '1', display_name: 'A' }, expires_at: SESSION_BODY.expires_at }],
    ['missing display name', { user: { id: 1 }, expires_at: SESSION_BODY.expires_at }],
    ['empty display name', { user: { id: 1, display_name: '' }, expires_at: SESSION_BODY.expires_at }],
    ['blank display name', { user: { id: 1, display_name: '   ' }, expires_at: SESSION_BODY.expires_at }],
    ['missing expiry', { user: SESSION_BODY.user }],
    ['numeric expiry', { user: SESSION_BODY.user, expires_at: 42 }],
    ['date-only expiry', { user: SESSION_BODY.user, expires_at: '2026-10-21' }],
    ['unparseable expiry', { user: SESSION_BODY.user, expires_at: 'not-a-timestamp' }],
    ['impossible date', { user: SESSION_BODY.user, expires_at: '2026-13-40T00:00:00Z' }],
  ]
  for (const [label, payload] of malformed) {
    assert.throws(() => parseAuthSession(payload), AuthApiError, label)
  }
})

test('loads the session from the relative public endpoint', async () => {
  const { deps, calls } = jsonDeps(200, SESSION_BODY)
  const session = await loadSession(new AbortController().signal, deps)
  assert.deepEqual(session, {
    user: { id: 1, displayName: 'Bác sĩ An' },
    expiresAt: '2026-10-21T12:00:00Z',
  })
  assert.equal(calls.length, 1)
  assert.equal(calls[0].input, '/api/auth/session')
  assert.equal(calls[0].init?.method, 'GET')
  assert.equal(calls[0].init?.cache, 'no-store')
  assert.equal(header(calls[0].init, 'Accept'), 'application/json')
})

test('treats a 401 session response as anonymous', async () => {
  const { deps, calls } = jsonDeps(401, {
    error: { code: 'AUTH_REQUIRED', message: 'raw server text' },
  })
  const session = await loadSession(new AbortController().signal, deps)
  assert.equal(session, null)
  assert.equal(calls.length, 1)
})

test('maps a malformed session success to a fixed safe message', async () => {
  for (const body of [{ user: null }, { user: SESSION_BODY.user, expires_at: 'yesterday' }, null]) {
    const { deps } = jsonDeps(200, body)
    await assert.rejects(
      () => loadSession(new AbortController().signal, deps),
      (error: unknown) => authError(error).message === AUTH_GENERIC_MESSAGE && !authError(error).message.includes('raw'),
    )
  }
})

test('maps non-401 session failures without surfacing server text', async () => {
  const { deps } = jsonDeps(500, { error: { code: 'INTERNAL_ERROR', message: 'raw server text' } })
  await assert.rejects(
    () => loadSession(new AbortController().signal, deps),
    (error: unknown) => {
      const failure = authError(error)
      assert.equal(failure.status, 500)
      assert.equal(failure.message, AUTH_GENERIC_MESSAGE)
      assert.equal(failure.message.includes('raw server text'), false)
      return true
    },
  )
})

test('rejects caller cancellation before dispatching', async () => {
  const controller = new AbortController()
  controller.abort()
  let called = false
  const deps: AuthRequestDeps = {
    fetch: async () => {
      called = true
      return new Response(null, { status: 200 })
    },
  }
  await assert.rejects(
    () => loadSession(controller.signal, deps),
    (error: unknown) => {
      const failure = authError(error)
      assert.equal(failure.canceled, true)
      assert.equal(failure.timedOut, false)
      return true
    },
  )
  assert.equal(called, false)
})

test('bounds every auth request with a five second timeout', async () => {
  let delay: number | null = null
  const deps: AuthRequestDeps = {
    setTimeout: (handler, ms) => {
      delay = ms
      handler()
      return 1
    },
    clearTimeout: () => undefined,
    fetch: async (_input, init) => {
      assert.equal(init?.signal?.aborted, true)
      throw new DOMException('Aborted', 'AbortError')
    },
  }
  await assert.rejects(
    () => loadSession(new AbortController().signal, deps),
    (error: unknown) => {
      const failure = authError(error)
      assert.equal(failure.timedOut, true)
      assert.equal(failure.canceled, false)
      assert.equal(failure.message, AUTH_GENERIC_MESSAGE)
      return true
    },
  )
  assert.equal(delay, AUTH_REQUEST_TIMEOUT_MS)
  assert.equal(AUTH_REQUEST_TIMEOUT_MS, 5000)
})

test('posts exactly one eight digit PIN without transforming it', async () => {
  const { deps, calls } = jsonDeps(200, SESSION_BODY)
  const session = await login('00000000', new AbortController().signal, deps)
  assert.equal(session.user.displayName, 'Bác sĩ An')
  assert.equal(calls.length, 1)
  assert.equal(calls[0].input, '/api/auth/login')
  assert.equal(calls[0].init?.method, 'POST')
  assert.equal(calls[0].init?.cache, 'no-store')
  assert.equal(header(calls[0].init, 'Content-Type'), 'application/json')
  assert.equal(header(calls[0].init, 'Accept'), 'application/json')
  assert.equal(calls[0].init?.body, '{"pin":"00000000"}')
})

test('rejects anything that is not exactly eight ASCII digits before dispatch', async () => {
  const invalid = ['', '1234567', '123456789', '1234 678', 'abcdefgh', '1234567a', ' 12345678', '12345678 ', '١٢٣٤٥٦٧٨']
  for (const pin of invalid) {
    let called = false
    const deps: AuthRequestDeps = {
      fetch: async () => {
        called = true
        return new Response(null, { status: 200 })
      },
    }
    await assert.rejects(
      () => login(pin, new AbortController().signal, deps),
      (error: unknown) => authError(error).message === INVALID_PIN_MESSAGE,
      pin,
    )
    assert.equal(called, false, pin)
  }
})

test('maps login failures to fixed safe messages', async () => {
  const cases: Array<[number, string]> = [
    [401, INVALID_CREDENTIALS_MESSAGE],
    [429, TOO_MANY_ATTEMPTS_MESSAGE],
    [503, DEPENDENCY_UNAVAILABLE_MESSAGE],
    [500, AUTH_GENERIC_MESSAGE],
  ]
  for (const [status, expected] of cases) {
    const { deps } = jsonDeps(status, { error: { code: 'X', message: 'raw server text' } })
    await assert.rejects(
      () => login('00000000', new AbortController().signal, deps),
      (error: unknown) => {
        const failure = authError(error)
        assert.equal(failure.status, status)
        assert.equal(failure.message, expected)
        assert.equal(failure.message.includes('raw server text'), false)
        return true
      },
      String(status),
    )
  }
})

test('maps a malformed login success to a fixed safe message', async () => {
  const { deps } = jsonDeps(200, { user: { id: 1 }, expires_at: 5 })
  await assert.rejects(
    () => login('00000000', new AbortController().signal, deps),
    (error: unknown) => authError(error).message === AUTH_GENERIC_MESSAGE,
  )
})

test('logout resolves even when the session is already gone', async () => {
  const calls: Recorded[] = []
  const deps: AuthRequestDeps = {
    fetch: async (input, init) => {
      calls.push({ input, init })
      return new Response(null, { status: 204 })
    },
  }
  await logout(new AbortController().signal, deps)
  assert.equal(calls[0].input, '/api/auth/logout')
  assert.equal(calls[0].init?.method, 'POST')

  const { deps: unauthorized } = jsonDeps(401, { error: { code: 'AUTH_REQUIRED', message: 'gone' } })
  await assert.doesNotReject(() => logout(new AbortController().signal, unauthorized))
})

test('logout stays best effort on network failure and cancellation', async () => {
  const failing: AuthRequestDeps = {
    fetch: async () => {
      throw new Error('network down')
    },
  }
  await assert.doesNotReject(() => logout(new AbortController().signal, failing))

  const controller = new AbortController()
  controller.abort()
  let called = false
  const deps: AuthRequestDeps = {
    fetch: async () => {
      called = true
      return new Response(null, { status: 204 })
    },
  }
  await assert.doesNotReject(() => logout(controller.signal, deps))
  assert.equal(called, false)
})
