import test from 'node:test'
import assert from 'node:assert/strict'

import {
  AUTH_REQUIRED_EVENT,
  MAX_TIMER_DELAY_MS,
  authenticatedFetch,
  createAuthGenerationGate,
  notifyAuthRequired,
  scheduleSessionExpiry,
} from '../src/auth/session.ts'
import type { AuthTransportDeps, ExpiryScheduler } from '../src/auth/session.ts'

interface Timer {
  id: number
  handler: () => void
  delay: number
}

function createScheduler(): { scheduler: ExpiryScheduler; timers: Timer[] } {
  const timers: Timer[] = []
  let nextId = 1
  return {
    timers,
    scheduler: {
      setTimeout: (handler, delay) => {
        const id = nextId
        nextId += 1
        timers.push({ id, handler, delay })
        return id
      },
      clearTimeout: (handle) => {
        const index = timers.findIndex((timer) => timer.id === handle)
        if (index >= 0) {
          timers.splice(index, 1)
        }
      },
    },
  }
}

test('delegates the request and the response unchanged', async () => {
  const init: RequestInit = {
    method: 'POST',
    body: '{"x":1}',
    headers: { Accept: 'application/json' },
    cache: 'no-store',
  }
  let seenInput: RequestInfo | URL | null = null
  let seenInit: RequestInit | undefined
  let dispatches = 0
  const response = new Response('payload', { status: 200 })
  const deps: AuthTransportDeps = {
    fetch: async (input, requestInit) => {
      seenInput = input
      seenInit = requestInit
      return response
    },
    dispatch: () => {
      dispatches += 1
    },
  }

  const result = await authenticatedFetch('/api/cases/1', init, deps)
  assert.equal(result, response)
  assert.equal(seenInput, '/api/cases/1')
  assert.equal(seenInit, init)
  assert.equal(dispatches, 0)
  assert.equal(await result.text(), 'payload')
})

test('notifies exactly once for a 401 response', async () => {
  const events: Event[] = []
  const deps: AuthTransportDeps = {
    fetch: async () => new Response(null, { status: 401 }),
    dispatch: (event) => events.push(event),
  }
  const response = await authenticatedFetch('/api/cases/1', undefined, deps)
  assert.equal(response.status, 401)
  assert.equal(events.length, 1)
  assert.equal(events[0].type, AUTH_REQUIRED_EVENT)
})

test('never notifies for other responses', async () => {
  for (const status of [200, 204, 403, 404, 500]) {
    let dispatches = 0
    const deps: AuthTransportDeps = {
      fetch: async () => new Response(null, { status }),
      dispatch: () => {
        dispatches += 1
      },
    }
    const response = await authenticatedFetch('/api/cases/1', undefined, deps)
    assert.equal(response.status, status)
    assert.equal(dispatches, 0, String(status))
  }
})

test('propagates transport failures without notifying', async () => {
  let dispatches = 0
  const deps: AuthTransportDeps = {
    fetch: async () => {
      throw new Error('network down')
    },
    dispatch: () => {
      dispatches += 1
    },
  }
  await assert.rejects(() => authenticatedFetch('/api/cases/1', undefined, deps), /network down/)
  assert.equal(dispatches, 0)
})

test('notifyAuthRequired is safe without a window', () => {
  assert.equal(typeof window, 'undefined')
  assert.doesNotThrow(() => notifyAuthRequired())
})

test('notifyAuthRequired dispatches the single app event through a seam', () => {
  const events: Event[] = []
  notifyAuthRequired({ dispatch: (event) => events.push(event) })
  assert.equal(events.length, 1)
  assert.equal(events[0].type, AUTH_REQUIRED_EVENT)
})

test('retires a generation so late results cannot repopulate the next user', () => {
  const gate = createAuthGenerationGate()
  let applied: string | null = null
  const apply = (generation: number, value: string): void => {
    if (gate.isCurrent(generation)) {
      applied = value
    }
  }

  const sessionLoad = gate.begin()
  assert.equal(gate.isCurrent(sessionLoad), true)

  const login = gate.begin()
  assert.equal(gate.isCurrent(sessionLoad), false)
  assert.equal(gate.isCurrent(login), true)

  gate.retire()
  assert.equal(gate.isCurrent(login), false)
  apply(login, 'previous user')
  assert.equal(applied, null)
})

test('schedules retirement with the absolute delay from expires_at', () => {
  let current = Date.parse('2026-10-21T12:00:00Z')
  const { scheduler, timers } = createScheduler()
  let expired = 0
  const handle = scheduleSessionExpiry({
    expiresAt: '2026-10-21T12:05:00Z',
    now: () => current,
    scheduler,
    onExpire: () => {
      expired += 1
    },
  })
  assert.notEqual(handle, null)
  assert.equal(timers.length, 1)
  assert.equal(timers[0].delay, 300_000)
  assert.equal(expired, 0)

  const timer = timers.shift()
  assert.ok(timer)
  current += timer.delay
  timer.handler()
  assert.equal(expired, 1)
})

test('cancels a retired schedule and reschedules after a new login', () => {
  const now = Date.parse('2026-10-21T12:00:00Z')
  const { scheduler, timers } = createScheduler()
  let expired = 0
  const options = {
    now: () => now,
    scheduler,
    onExpire: () => {
      expired += 1
    },
  }

  const first = scheduleSessionExpiry({ ...options, expiresAt: '2026-10-21T12:00:10Z' })
  first?.cancel()
  assert.equal(timers.length, 0)

  const second = scheduleSessionExpiry({ ...options, expiresAt: '2026-10-21T12:02:00Z' })
  assert.notEqual(second, null)
  assert.equal(timers.length, 1)
  assert.equal(timers[0].delay, 120_000)
  assert.equal(expired, 0)
})

test('fails closed for an already expired timestamp', () => {
  const now = Date.parse('2026-10-21T12:00:00Z')
  const { scheduler, timers } = createScheduler()
  let expired = 0
  const handle = scheduleSessionExpiry({
    expiresAt: '2026-10-21T11:59:00Z',
    now: () => now,
    scheduler,
    onExpire: () => {
      expired += 1
    },
  })
  assert.equal(handle, null)
  assert.equal(expired, 1)
  assert.equal(timers.length, 0)
})

test('fails closed for an invalid timestamp', () => {
  const { scheduler, timers } = createScheduler()
  for (const expiresAt of ['', 'not-a-timestamp', '2026-10-21']) {
    let expired = 0
    const handle = scheduleSessionExpiry({
      expiresAt,
      now: () => 0,
      scheduler,
      onExpire: () => {
        expired += 1
      },
    })
    assert.equal(handle, null, expiresAt)
    assert.equal(expired, 1, expiresAt)
  }
  assert.equal(timers.length, 0)
})

test('chains timers beyond the platform limit at the absolute target', () => {
  const now = Date.parse('2026-10-21T12:00:00Z')
  const target = now + 30 * 24 * 60 * 60 * 1000
  let current = now
  const { scheduler, timers } = createScheduler()
  let expired = 0
  const handle = scheduleSessionExpiry({
    expiresAt: new Date(target).toISOString(),
    now: () => current,
    scheduler,
    onExpire: () => {
      expired += 1
    },
  })
  assert.notEqual(handle, null)
  assert.equal(timers[0].delay, MAX_TIMER_DELAY_MS)

  const first = timers.shift()
  assert.ok(first)
  current += first.delay
  first.handler()
  assert.equal(expired, 0)
  assert.equal(timers.length, 1)
  assert.equal(timers[0].delay, target - current)

  const second = timers.shift()
  assert.ok(second)
  current += second.delay
  second.handler()
  assert.equal(expired, 1)
})
