import test from 'node:test'
import assert from 'node:assert/strict'
import { setImmediate as setImmediateAsync } from 'node:timers/promises'

import { InitialApiError } from '../src/initialread/api.ts'
import { measureInitialMark } from '../src/initialread/geometry.ts'
import { createInitialWorkflow } from '../src/initialread/workflow.ts'
import type { InitialSnapshot, WorkflowDependencies } from '../src/initialread/workflow.ts'
import type { DraftMark, InitialMark, InitialRead, SubmitResult, WorkflowState } from '../src/initialread/types.ts'
import type { CaseMetadata } from '../src/case.ts'

interface Deferred<T> {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (error: unknown) => void
}

function deferred<T>(): Deferred<T> {
  return Promise.withResolvers<T>()
}

// Drains the microtask queue deterministically: the workflow resolves through
// promise chains only, so one macrotask boundary settles every pending step
// without any wall-clock delay.
function tick(): Promise<void> {
  return setImmediateAsync()
}

function mark(x: number): InitialMark {
  return measureInitialMark([x, 0, 0], [x + 3, 4, 0], [0, 0, 1], null)
}

function draft(...marks: InitialMark[]): DraftMark[] {
  return marks.map((value, index) => ({ localId: `l${index}`, mark: value }))
}

const CASE: CaseMetadata = {
  id: 4,
  source: 'synthetic',
  studyInstanceUID: '1.2.3',
  seriesInstanceUID: '1.2.4',
  frameOfReferenceUID: '1.2.5',
  status: 'awaiting_read',
  createdAt: '2026-10-11T00:00:00Z',
  finalizedAt: null,
}

function read(marks: InitialMark[], lockedAt: string | null = null): InitialRead {
  return { id: 1, case_id: 4, kind: 'initial', reader: 'reader', locked_at: lockedAt, marks }
}

const SUBMIT_OK: SubmitResult = { status: 'initial_locked', locked_at: '2026-10-11T01:00:00Z', final_read_id: 2 }

interface Harness {
  deps: WorkflowDependencies
  states: WorkflowState[]
  putCalls: InitialMark[][]
  postCalls: number
  snapshotCalls: number
  puts: () => Deferred<InitialMark[]>[]
  posts: () => Deferred<SubmitResult>[]
  snapshots: () => Deferred<InitialSnapshot>[]
}

function harness(initialMarks: InitialMark[], options: { locked?: boolean; autoResolveSnapshots?: boolean } = {}): Harness {
  const states: WorkflowState[] = []
  const putCalls: InitialMark[][] = []
  const putQueue: Deferred<InitialMark[]>[] = []
  const postQueue: Deferred<SubmitResult>[] = []
  const snapshotQueue: Deferred<InitialSnapshot>[] = []
  let postCalls = 0
  let snapshotCalls = 0

  const deps: WorkflowDependencies = {
    caseMetadata: CASE,
    initialRead: read(initialMarks, options.locked === true ? '2026-10-11T00:00:00Z' : null),
    initialDraft: draft(...initialMarks),
    replaceMarks(marks) {
      putCalls.push(marks.map((value) => value))
      const pending = deferred<InitialMark[]>()
      putQueue.push(pending)
      return pending.promise
    },
    submitRead() {
      postCalls += 1
      const pending = deferred<SubmitResult>()
      postQueue.push(pending)
      return pending.promise
    },
    loadSnapshot() {
      snapshotCalls += 1
      const pending = deferred<InitialSnapshot>()
      snapshotQueue.push(pending)
      return pending.promise
    },
    onState(state) {
      states.push(state)
    },
  }

  return {
    deps,
    states,
    putCalls,
    get postCalls() {
      return postCalls
    },
    get snapshotCalls() {
      return snapshotCalls
    },
    puts: () => putQueue,
    posts: () => postQueue,
    snapshots: () => snapshotQueue,
  }
}

function last(h: Harness): WorkflowState {
  return h.states[h.states.length - 1]
}

test('newerDraftSurvivesOlderAcknowledgement', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const a2 = mark(20)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  assert.equal(h.putCalls.length, 1)
  assert.equal(h.putCalls[0].length, 2)

  // A newer edit arrives while revision 1 is still in flight.
  flow.replaceDraft(draft(a0, a1, a2))
  await tick()
  assert.equal(h.putCalls.length, 1, 'no concurrent PUT')

  h.puts()[0].resolve(h.putCalls[0])
  await tick()
  assert.equal(h.putCalls.length, 2, 'the newest full snapshot is written next')
  const state = last(h)
  assert.equal(state.draft.length, 3, 'older acknowledgement must not replace the newer draft')
  assert.equal(state.savedRevision, 1)
  assert.equal(state.revision, 2)

  h.puts()[1].resolve(h.putCalls[1])
  await tick()
  assert.equal(last(h).savedRevision, 2)
  flow.dispose()
})

test('deletionToEmptyAndDuplicateEventsAreNotResaved', async () => {
  const a0 = mark(0)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft([])
  await tick()
  assert.equal(h.putCalls.length, 1)
  assert.deepEqual(h.putCalls[0], [])
  h.puts()[0].resolve([])
  await tick()
  assert.deepEqual(last(h).draft, [])
  assert.equal(last(h).savedRevision, 1)

  // A duplicate unchanged notification must not enqueue another PUT.
  flow.replaceDraft([])
  await tick()
  assert.equal(h.putCalls.length, 1)
  assert.equal(last(h).revision, 1)
  flow.dispose()
})

test('saveFailurePreventsSubmitAndRetainsDraft', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  h.puts()[0].reject(new InitialApiError('bad request', { status: 400, code: 'INVALID_REQUEST' }))
  await tick()
  assert.equal(last(h).phase, 'error')
  assert.equal(last(h).draft.length, 2, 'unsaved geometry is kept')

  const submission = flow.submit()
  await tick()
  assert.equal(h.postCalls, 0, 'no POST before the flush resolves')
  // The explicit submit retried the flush; it too fails definitively.
  h.puts()[1].reject(new InitialApiError('bad request', { status: 400, code: 'INVALID_REQUEST' }))
  await submission
  assert.equal(h.postCalls, 0, 'no POST after a failed save')
  assert.equal(last(h).draft.length, 2)
  flow.dispose()
})

test('submitFlushesLatestRevisionBeforeOnePost', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  assert.equal(h.putCalls.length, 1)

  const first = flow.submit()
  const second = flow.submit()
  await tick()
  assert.equal(h.postCalls, 0, 'POST waits for the pending save')
  assert.equal(h.putCalls.length, 1)

  h.puts()[0].resolve(h.putCalls[0])
  await tick()
  assert.equal(h.postCalls, 1)
  h.posts()[0].resolve(SUBMIT_OK)
  await Promise.all([first, second])
  assert.equal(last(h).phase, 'locked')
  assert.equal(last(h).editable, false)
  flow.dispose()
})

test('emptyCleanDraftCanSubmit', async () => {
  const h = harness([])
  const flow = createInitialWorkflow(h.deps)
  const pending = flow.submit()
  await tick()
  assert.equal(h.putCalls.length, 0)
  assert.equal(h.postCalls, 1)
  h.posts()[0].resolve(SUBMIT_OK)
  await pending
  assert.equal(last(h).phase, 'locked')
  flow.dispose()
})

test('unknownSubmitReconcilesWithoutReplay', async () => {
  const a0 = mark(0)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  const pending = flow.submit()
  await tick()
  h.posts()[0].reject(new InitialApiError('network request failed', { uncertain: true }))
  await tick()
  assert.equal(last(h).phase, 'reconciling')
  assert.equal(h.snapshotCalls, 1)

  h.snapshots()[0].resolve({ caseMetadata: { ...CASE, status: 'initial_locked' }, read: read([a0], '2026-10-11T01:00:00Z') })
  await pending
  assert.equal(last(h).phase, 'locked')
  assert.equal(last(h).editable, false)
  assert.equal(h.postCalls, 1, 'the committed submit is never replayed')

  await flow.submit()
  assert.equal(h.postCalls, 1, 'locked workflow never submits again')
  flow.dispose()
})

test('unlockedEqualAcknowledgementAdvancesSavedRevision', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  h.puts()[0].reject(new InitialApiError('timed out', { uncertain: true }))
  await tick()
  h.snapshots()[0].resolve({ caseMetadata: { ...CASE, status: 'awaiting_read' }, read: read(h.putCalls[0]) })
  await tick()
  const state = last(h)
  assert.equal(state.phase, 'ready')
  assert.equal(state.savedRevision, 1)
  assert.equal(state.revision, 1)
  flow.dispose()
})

test('unlockedDifferentMarksBecomeConflict', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  h.puts()[0].reject(new InitialApiError('network request failed', { uncertain: true }))
  await tick()
  h.snapshots()[0].resolve({ caseMetadata: { ...CASE, status: 'awaiting_read' }, read: read([mark(99)]) })
  await tick()
  const state = last(h)
  assert.equal(state.phase, 'conflict')
  assert.equal(state.error !== null, true)
  assert.equal(state.draft.length, 2, 'the local draft is preserved for explicit overwrite')
  flow.dispose()
})

test('failedReconciliationStaysBlocked', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  h.puts()[0].reject(new InitialApiError('network request failed', { uncertain: true }))
  await tick()
  h.snapshots()[0].reject(new InitialApiError('still offline', { uncertain: true }))
  await tick()
  assert.equal(last(h).phase, 'error')
  assert.equal(last(h).draft.length, 2)
  flow.dispose()
})

test('lockConflictDuringSaveReconcilesToLocked', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  h.puts()[0].reject(new InitialApiError('locked', { status: 409, code: 'INITIAL_READ_LOCKED' }))
  await tick()
  assert.equal(last(h).phase, 'reconciling')
  h.snapshots()[0].resolve({ caseMetadata: { ...CASE, status: 'initial_locked' }, read: read([a0], '2026-10-11T01:00:00Z') })
  await tick()
  assert.equal(last(h).phase, 'locked')
  assert.equal(last(h).draft.length, 1, 'the confirmed locked snapshot replaces the local draft')
  assert.equal(last(h).snapshotEpoch, 1)
  flow.dispose()
})

test('disposedWorkflowSuppressesCompletions', async () => {
  const a0 = mark(0)
  const a1 = mark(10)
  const h = harness([a0])
  const flow = createInitialWorkflow(h.deps)

  flow.replaceDraft(draft(a0, a1))
  await tick()
  const before = h.states.length
  flow.dispose()
  h.puts()[0].resolve(h.putCalls[0])
  await tick()
  assert.equal(h.states.length, before, 'no state after dispose')
})

test('lockedInitialReadIsReadOnly', async () => {
  const a0 = mark(0)
  const h = harness([a0], { locked: true })
  const flow = createInitialWorkflow(h.deps)
  assert.equal(flow.getState().phase, 'locked')
  assert.equal(flow.getState().editable, false)
  flow.replaceDraft(draft(a0, mark(5)))
  await tick()
  assert.equal(flow.getState().revision, 0)
  assert.equal(h.putCalls.length, 0)
  flow.dispose()
})
