import test from 'node:test'
import assert from 'node:assert/strict'

import { createAISession, selectReadMode } from '../src/finalread/aiSession.ts'
import type { AISessionState } from '../src/finalread/aiSession.ts'
import { ReadApiError } from '../src/reads/http.ts'
import type { AIProjection } from '../src/finalread/types.ts'
import type { CaseMetadata } from '../src/case.ts'

interface Deferred<T> {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (error: unknown) => void
}

function deferred<T>(): Deferred<T> {
  return Promise.withResolvers<T>()
}

function tick(): Promise<void> {
  return new Promise<void>((resolve) => {
    queueMicrotask(resolve)
  })
}

const CASE: CaseMetadata = {
  id: 4,
  source: 'synthetic',
  studyInstanceUID: '1.2.3',
  seriesInstanceUID: '1.2.4',
  frameOfReferenceUID: '1.2.5',
  status: 'initial_locked',
  createdAt: '2026-10-11T00:00:00Z',
  finalizedAt: null,
}

const QUEUED: AIProjection = { status: 'queued', candidates: null, error: null }
const RUNNING: AIProjection = { status: 'running', candidates: null, error: null }
const FAILED: AIProjection = { status: 'failed', candidates: [], error: { code: 'AI_UNAVAILABLE', message: 'x' } }
const SUCCEEDED: AIProjection = {
  status: 'succeeded',
  candidates: [],
  model: { bundle: 'b', bundle_version: '1', score_threshold: 0.5 },
  input: { num_slices: 1, spacing_mm: [1, 1, 2], size_voxels: [1, 1, 1] },
  timing_s: { load: 0, inference: 0, total: 0 },
  resources: { device: 'cpu' },
}

test('neverDisclosesBeforeCoherentLock', () => {
  assert.equal(selectReadMode({ ...CASE, status: 'awaiting_read' }, null), 'independent')
  assert.equal(selectReadMode({ ...CASE, status: 'initial_locked' }, '2026-10-11T00:00:00Z'), 'review')
  assert.equal(selectReadMode({ ...CASE, status: 'finalized' }, '2026-10-11T00:00:00Z'), 'completed')
  // The legacy revealed state is still review when the lock is real.
  assert.equal(selectReadMode({ ...CASE, status: 'ai_revealed' as never }, '2026-10-11T00:00:00Z'), 'review')

  // Every inconsistent combination is a conflict, never a silent mode.
  assert.equal(selectReadMode({ ...CASE, status: 'awaiting_read' }, '2026-10-11T00:00:00Z'), 'conflict')
  assert.equal(selectReadMode({ ...CASE, status: 'initial_locked' }, null), 'conflict')
  assert.equal(selectReadMode({ ...CASE, status: 'finalized' }, null), 'conflict')
  assert.equal(selectReadMode({ ...CASE, status: 'ai_revealed' as never }, null), 'conflict')
})

interface Harness {
  session: ReturnType<typeof createAISession>
  states: AISessionState[]
  loads: () => Deferred<AIProjection>[]
  scheduled: () => Array<{ cb: () => void; delay: number; cancelled: boolean }>
}

function harness(): Harness {
  const states: AISessionState[] = []
  const queue: Deferred<AIProjection>[] = []
  const scheduled: Array<{ cb: () => void; delay: number; cancelled: boolean }> = []
  const session = createAISession({
    load() {
      const pending = deferred<AIProjection>()
      queue.push(pending)
      return pending.promise
    },
    schedule(cb, delay) {
      const item = { cb, delay, cancelled: false }
      scheduled.push(item)
      return () => {
        item.cancelled = true
      }
    },
    onState(state) {
      states.push(state)
    },
  })
  return { session, states, loads: () => queue, scheduled: () => scheduled }
}

test('pollsPendingWithoutOverlapAndStopsTerminal', async () => {
  const h = harness()
  h.session.start()
  await tick()
  assert.equal(h.loads().length, 1)
  assert.equal(h.states[h.states.length - 1].pending, true)

  // Settlement with a pending status schedules exactly one poll 2 s later.
  h.loads()[0].resolve(QUEUED)
  await tick()
  assert.equal(h.scheduled().length, 1)
  assert.equal(h.scheduled()[0].delay, 2000)
  assert.equal(h.loads().length, 1, 'no overlap while a poll is only scheduled')
  assert.equal(h.states[h.states.length - 1].pending, true)

  h.scheduled()[0].cb()
  await tick()
  assert.equal(h.loads().length, 2)
  assert.equal(h.scheduled().length, 1, 'no second schedule while a request is in flight')

  h.loads()[1].resolve(RUNNING)
  await tick()
  assert.equal(h.scheduled().length, 2)

  h.scheduled()[1].cb()
  await tick()
  h.loads()[2].resolve(SUCCEEDED)
  await tick()
  assert.equal(h.states[h.states.length - 1].projection?.status, 'succeeded')
  assert.equal(h.states[h.states.length - 1].pending, false)
  assert.equal(h.scheduled().length, 2, 'a terminal status stops the poll')

  h.session.start()
  await tick()
  assert.equal(h.loads().length, 3, 'a terminal session never restarts itself')
  h.session.dispose()
})

test('failedStatusStopsAndErrorRequiresManualRetry', async () => {
  const h = harness()
  h.session.start()
  await tick()
  h.loads()[0].resolve(FAILED)
  await tick()
  assert.equal(h.states[h.states.length - 1].projection?.status, 'failed')
  assert.equal(h.scheduled().length, 0)

  // A request error keeps the last projection, exposes an error and does not
  // schedule anything; only an explicit retry starts a new cycle.
  h.session.retry()
  await tick()
  h.loads()[1].reject(new ReadApiError('mất kết nối', { uncertain: true }))
  await tick()
  const state = h.states[h.states.length - 1]
  assert.equal(state.error !== null, true)
  assert.equal(state.pending, false)
  assert.equal(h.scheduled().length, 0)

  h.session.retry()
  await tick()
  assert.equal(h.loads().length, 3)
  h.loads()[2].resolve(SUCCEEDED)
  await tick()
  assert.equal(h.states[h.states.length - 1].error, null)
  h.session.dispose()
})

test('retiredResponseCannotReviveSession', async () => {
  // A scheduled poll cancelled by dispose never runs.
  const scheduledHarness = harness()
  scheduledHarness.session.start()
  await tick()
  scheduledHarness.loads()[0].resolve(QUEUED)
  await tick()
  assert.equal(scheduledHarness.scheduled().length, 1)
  scheduledHarness.session.dispose()
  assert.equal(scheduledHarness.scheduled()[0].cancelled, true)
  scheduledHarness.scheduled()[0].cb()
  await tick()
  assert.equal(scheduledHarness.loads().length, 1, 'a cancelled schedule never runs')

  // An in-flight response settling after dispose publishes nothing.
  const inFlight = harness()
  inFlight.session.start()
  await tick()
  const published = inFlight.states.length
  inFlight.session.dispose()
  inFlight.loads()[0].resolve(RUNNING)
  await tick()
  assert.equal(inFlight.states.length, published, 'a retired session publishes nothing')
  assert.equal(inFlight.scheduled().length, 0)
})
