import test from 'node:test'
import assert from 'node:assert/strict'
import { setImmediate as setImmediateAsync } from 'node:timers/promises'

import { ReadApiError } from '../src/reads/http.ts'
import { measureEndpoints } from '../src/reads/geometry.ts'
import { createFinalWorkflow } from '../src/finalread/workflow.ts'
import type { FinalReloadSnapshot, FinalWorkflowDependencies } from '../src/finalread/workflow.ts'
import type { CandidateDecision, FinalMark, FinalRead, FinalWorkflowState, FinalizeResult } from '../src/finalread/types.ts'
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
  return setImmediateAsync()
}

function readerMark(x: number): FinalMark {
  return { ...measureEndpoints([x, 0, 0], [x + 3, 4, 0], [0, 0, 1], null), origin: 'reader_after_ai', ai_candidate_id: null }
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

function finalRead(marks: FinalMark[], decisions: CandidateDecision[] = [], lockedAt: string | null = null): FinalRead {
  return { id: 2, case_id: 4, kind: 'final', reader: 'reader', locked_at: lockedAt, marks, candidate_decisions: decisions }
}

const FINALIZE_OK: FinalizeResult = { status: 'finalized', finalized_at: '2026-10-11T03:00:00Z', ai_status: 'queued' }

interface Harness {
  deps: FinalWorkflowDependencies
  states: FinalWorkflowState[]
  puts: () => Deferred<FinalRead>[]
  posts: () => Deferred<FinalizeResult>[]
  reloads: () => Deferred<FinalReloadSnapshot>[]
  putCalls: Array<{ marks: FinalMark[]; candidate_decisions: CandidateDecision[] }>
  postCalls: number
  reloadCalls: number
}

function harness(initial: FinalRead): Harness {
  const states: FinalWorkflowState[] = []
  const putCalls: Array<{ marks: FinalMark[]; candidate_decisions: CandidateDecision[] }> = []
  const putQueue: Deferred<FinalRead>[] = []
  const postQueue: Deferred<FinalizeResult>[] = []
  const reloadQueue: Deferred<FinalReloadSnapshot>[] = []
  let postCalls = 0
  let reloadCalls = 0

  const deps: FinalWorkflowDependencies = {
    initial,
    replace(value) {
      putCalls.push({ marks: value.marks, candidate_decisions: value.candidate_decisions })
      const pending = deferred<FinalRead>()
      putQueue.push(pending)
      return pending.promise
    },
    finalize() {
      postCalls += 1
      const pending = deferred<FinalizeResult>()
      postQueue.push(pending)
      return pending.promise
    },
    reload() {
      reloadCalls += 1
      const pending = deferred<FinalReloadSnapshot>()
      reloadQueue.push(pending)
      return pending.promise
    },
    onState(state) {
      states.push(state)
    },
  }

  return {
    deps,
    states,
    puts: () => putQueue,
    posts: () => postQueue,
    reloads: () => reloadQueue,
    putCalls,
    get postCalls() {
      return postCalls
    },
    get reloadCalls() {
      return reloadCalls
    },
  }
}

function last(h: Harness): FinalWorkflowState {
  return h.states[h.states.length - 1]
}

const ACCEPTED: CandidateDecision = { candidate_id: 'c1', decision: 'accepted', dismiss_reason: null }
const DISMISSED_C2: CandidateDecision = { candidate_id: 'c2', decision: 'dismissed', dismiss_reason: 'lý do' }

test('oldAcknowledgementDoesNotOverwriteNewerFinalDraft', async () => {
  const a0 = readerMark(0)
  const a1 = readerMark(10)
  const h = harness(finalRead([a0]))
  const flow = createFinalWorkflow(h.deps)

  flow.replaceDraft([
    { localId: 'l0', mark: a0 },
    { localId: 'l1', mark: a1 },
  ])
  await tick()
  assert.equal(h.putCalls.length, 1)
  assert.equal(h.putCalls[0].marks.length, 2)

  flow.dismiss('c2', 'lý do')
  await tick()
  assert.equal(h.putCalls.length, 1, 'no concurrent PUT')

  h.puts()[0].resolve(finalRead(h.putCalls[0].marks, h.putCalls[0].candidate_decisions))
  await tick()
  assert.equal(h.putCalls.length, 2, 'the newest full snapshot is written next')
  assert.equal(h.putCalls[1].marks.length, 2)
  assert.deepEqual(h.putCalls[1].candidate_decisions, [DISMISSED_C2])
  assert.equal(last(h).decisions.length, 1)

  h.puts()[1].resolve(finalRead(h.putCalls[1].marks, h.putCalls[1].candidate_decisions))
  await tick()
  assert.equal(last(h).savedRevision, 2)
  flow.dispose()
})

test('decisionOrderDoesNotCauseFalseConflict', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.dismiss('c1', 'a')
  await tick()
  flow.dismiss('c2', 'b')
  await tick()
  assert.equal(h.putCalls.length, 1, 'one PUT at a time')

  // Each acknowledgement returns the decisions ordered by candidate id, not in
  // request order; that must not look like a conflicting snapshot.
  h.puts()[0].resolve(finalRead(h.putCalls[0].marks, [...h.putCalls[0].candidate_decisions].reverse()))
  await tick()
  assert.equal(h.putCalls.length, 2)
  h.puts()[1].resolve(finalRead(h.putCalls[1].marks, [...h.putCalls[1].candidate_decisions].reverse()))
  await tick()
  assert.equal(last(h).phase, 'ready')
  assert.equal(last(h).savedRevision, 2)
  flow.dispose()
})

test('deletionToEmptyIsAValidReplacement', async () => {
  const h = harness(finalRead([readerMark(0)], [ACCEPTED]))
  const flow = createFinalWorkflow(h.deps)
  flow.remove(last(h).draft[0].localId)
  await tick()
  assert.equal(h.putCalls.length, 1)
  assert.deepEqual(h.putCalls[0].marks, [])
  // The accepted decision without a mark is preserved, not stripped.
  assert.deepEqual(h.putCalls[0].candidate_decisions, [ACCEPTED])
  h.puts()[0].resolve(finalRead([], [ACCEPTED]))
  await tick()
  assert.equal(last(h).savedRevision, 1)
  flow.dispose()
})

test('finalizeFlushesLatestDraftAndPostsOnce', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: readerMark(0) }, { localId: 'l1', mark: readerMark(10) }])
  await tick()
  assert.equal(h.putCalls.length, 1)

  const first = flow.finalize()
  const second = flow.finalize()
  await tick()
  assert.equal(h.postCalls, 0, 'POST waits for the pending save')

  h.puts()[0].resolve(finalRead(h.putCalls[0].marks, []))
  await tick()
  assert.equal(h.postCalls, 1)
  assert.equal(last(h).phase, 'finalizing')

  h.posts()[0].resolve(FINALIZE_OK)
  await Promise.all([first, second])
  assert.equal(last(h).phase, 'completed')
  assert.equal(last(h).editable, false)
  assert.equal(h.postCalls, 1)
  flow.dispose()
})

test('failedSavePreventsFinalize', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: readerMark(0) }, { localId: 'l1', mark: readerMark(10) }])
  await tick()
  h.puts()[0].reject(new ReadApiError('bad request', { status: 400, code: 'INVALID_REQUEST' }))
  await tick()
  assert.equal(last(h).phase, 'error')

  const pending = flow.finalize()
  await tick()
  // finalize retried the flush; it is in flight before any POST.
  assert.equal(h.postCalls, 0)
  assert.equal(h.putCalls.length, 2)
  h.puts()[1].reject(new ReadApiError('bad request', { status: 400, code: 'INVALID_REQUEST' }))
  await pending
  assert.equal(h.postCalls, 0)
  assert.equal(last(h).draft.length, 2, 'unsaved geometry is kept')
  flow.dispose()
})

test('pendingOrFailedAIDoesNotGateCompletion', async () => {
  // Undecided candidates are absent from the payload and never block finalize.
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  const pending = flow.finalize()
  await tick()
  assert.equal(h.postCalls, 1)
  h.posts()[0].resolve({ ...FINALIZE_OK, ai_status: 'failed' })
  await pending
  assert.equal(last(h).phase, 'completed')
  flow.dispose()
})

test('unknownFinalizeReconcilesWithoutReplay', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  const pending = flow.finalize()
  await tick()
  h.posts()[0].reject(new ReadApiError('network request failed', { uncertain: true }))
  await tick()
  assert.equal(last(h).phase, 'reconciling')
  assert.equal(h.reloadCalls, 1)

  h.reloads()[0].resolve({
    caseMetadata: { ...CASE, status: 'finalized' },
    initialLockedAt: '2026-10-11T00:00:00Z',
    final: finalRead([readerMark(0)], [], '2026-10-11T03:00:00Z'),
  })
  await pending
  assert.equal(last(h).phase, 'completed')
  assert.equal(last(h).editable, false)
  assert.equal(h.postCalls, 1, 'the committed finalize is never replayed')
  flow.dispose()
})

test('unlockedEqualFinalAcknowledgementAdvancesSavedRevision', async () => {
  const a0 = readerMark(0)
  const a1 = readerMark(10)
  const h = harness(finalRead([a0]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: a0 }, { localId: 'l1', mark: a1 }])
  await tick()
  h.puts()[0].reject(new ReadApiError('timed out', { uncertain: true }))
  await tick()
  h.reloads()[0].resolve({
    caseMetadata: CASE,
    initialLockedAt: '2026-10-11T00:00:00Z',
    final: finalRead(h.putCalls[0].marks, h.putCalls[0].candidate_decisions),
  })
  await tick()
  assert.equal(last(h).phase, 'ready')
  assert.equal(last(h).savedRevision, 1)
  flow.dispose()
})

test('unlockedDifferentFinalBecomesConflict', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: readerMark(0) }, { localId: 'l1', mark: readerMark(10) }])
  await tick()
  h.puts()[0].reject(new ReadApiError('network request failed', { uncertain: true }))
  await tick()
  h.reloads()[0].resolve({ caseMetadata: CASE, initialLockedAt: '2026-10-11T00:00:00Z', final: finalRead([readerMark(99)]) })
  await tick()
  assert.equal(last(h).phase, 'conflict')
  assert.equal(last(h).draft.length, 2, 'the local draft is preserved for explicit overwrite')
  flow.dispose()
})

test('lockConflictDuringSaveReconcilesToCompleted', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: readerMark(0) }, { localId: 'l1', mark: readerMark(10) }])
  await tick()
  h.puts()[0].reject(new ReadApiError('locked', { status: 409, code: 'FINAL_READ_LOCKED' }))
  await tick()
  assert.equal(last(h).phase, 'reconciling')
  h.reloads()[0].resolve({
    caseMetadata: { ...CASE, status: 'finalized' },
    initialLockedAt: '2026-10-11T00:00:00Z',
    final: finalRead([readerMark(0)], [], '2026-10-11T03:00:00Z'),
  })
  await tick()
  assert.equal(last(h).phase, 'completed')
  assert.equal(last(h).draft.length, 1, 'the locked server snapshot replaces the local draft')
  flow.dispose()
})

test('acceptsOnlyCompletedMeasuredGeometry', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)

  // A zero-length measurement is refused; the draft and decisions are untouched.
  const degenerate = { ...measureEndpoints([1, 0, 0], [4, 0, 0], [0, 0, 1], null), origin: 'reader_after_ai' as const, ai_candidate_id: null }
  flow.accept('c1', { ...degenerate, p2_mm: degenerate.p1_mm })
  await tick()
  assert.equal(last(h).draft.length, 1)
  assert.equal(last(h).decisions.length, 0)
  assert.equal(h.putCalls.length, 0)

  const measured = { ...measureEndpoints([5, 0, 0], [8, 0, 0], [0, 0, 1], null), origin: 'reader_after_ai' as const, ai_candidate_id: null }
  flow.accept('c1', measured)
  await tick()
  const linked = last(h).draft.find((entry) => entry.mark.ai_candidate_id === 'c1')
  assert.notEqual(linked, undefined)
  assert.equal(linked?.mark.origin, 'from_ai_candidate')
  assert.deepEqual(last(h).decisions, [ACCEPTED])
  flow.dispose()
})

test('duplicateAcceptCannotCreateSecondLinkedMark', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  const measured = { ...measureEndpoints([5, 0, 0], [8, 0, 0], [0, 0, 1], null), origin: 'reader_after_ai' as const, ai_candidate_id: null }
  flow.accept('c1', measured)
  await tick()
  flow.accept('c1', { ...measured, p1_mm: [50, 0, 0], p2_mm: [53, 0, 0], center_mm: [51.5, 0, 0] })
  await tick()
  assert.equal(last(h).draft.filter((entry) => entry.mark.ai_candidate_id === 'c1').length, 1)
  flow.dispose()
})

test('dismissRequiresReasonAndPreservesReaderMarks', async () => {
  const reader = readerMark(0)
  const measured = { ...measureEndpoints([5, 0, 0], [8, 0, 0], [0, 0, 1], null), origin: 'reader_after_ai' as const, ai_candidate_id: null }
  const h = harness(finalRead([reader]))
  const flow = createFinalWorkflow(h.deps)
  flow.accept('c1', measured)
  await tick()

  // Whitespace-only reason is refused and changes neither list.
  const before = last(h)
  flow.dismiss('c1', '   ')
  await tick()
  assert.equal(last(h).revision, before.revision)
  assert.equal(last(h).draft.filter((entry) => entry.mark.ai_candidate_id === 'c1').length, 1)

  flow.dismiss('c1', ' bờ nốt ')
  await tick()
  assert.equal(last(h).draft.filter((entry) => entry.mark.ai_candidate_id === 'c1').length, 0)
  assert.equal(last(h).draft.filter((entry) => entry.mark.origin === 'reader_after_ai').length, 1, 'reader marks are preserved')
  const decision = last(h).decisions.find((value) => value.candidate_id === 'c1')
  assert.equal(decision?.decision, 'dismissed')
  assert.equal(decision?.dismiss_reason, ' bờ nốt ')
  flow.dispose()
})

test('linkedRemovalReturnsCandidateToUndecided', async () => {
  const measured = { ...measureEndpoints([5, 0, 0], [8, 0, 0], [0, 0, 1], null), origin: 'reader_after_ai' as const, ai_candidate_id: null }
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.accept('c1', measured)
  await tick()
  const linked = last(h).draft.find((entry) => entry.mark.ai_candidate_id === 'c1')
  flow.remove(linked!.localId)
  await tick()
  assert.equal(last(h).draft.some((entry) => entry.mark.ai_candidate_id === 'c1'), false)
  assert.equal(last(h).decisions.some((value) => value.candidate_id === 'c1'), false)
  flow.dispose()
})

test('disposedWorkflowSuppressesCompletions', async () => {
  const h = harness(finalRead([readerMark(0)]))
  const flow = createFinalWorkflow(h.deps)
  flow.replaceDraft([{ localId: 'l0', mark: readerMark(0) }, { localId: 'l1', mark: readerMark(10) }])
  await tick()
  const published = h.states.length
  flow.dispose()
  h.puts()[0].resolve(finalRead(h.putCalls[0].marks, []))
  await tick()
  assert.equal(h.states.length, published)
})

test('lockedFinalReadIsReadOnly', async () => {
  const h = harness(finalRead([readerMark(0)], [], '2026-10-11T03:00:00Z'))
  const flow = createFinalWorkflow(h.deps)
  assert.equal(last(h).phase, 'completed')
  assert.equal(last(h).editable, false)
  flow.replaceDraft([{ localId: 'x', mark: readerMark(99) }])
  await tick()
  assert.equal(h.putCalls.length, 0)
  flow.dispose()
})
