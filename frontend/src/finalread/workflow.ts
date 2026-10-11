// Serialized final-read draft persistence. A page-scoped controller: at most one
// whole-list PUT is in flight, edits coalesce into the latest snapshot, finalize
// flushes the latest revision before its single bodyless POST, and an uncertain
// outcome freezes writes and reconciles with the server instead of replaying.
// Candidate actions keep the origin/linkage/reason invariants of the backend. No
// DOM, no timers, no debounce, no automatic retry.

import type { CaseMetadata } from '../case.ts'
import { ReadApiError } from '../reads/http.ts'
import { isFiniteVec3, markListsEqual } from '../reads/geometry.ts'
import type {
  CandidateDecision,
  FinalDraftMark,
  FinalMark,
  FinalRead,
  FinalReplacement,
  FinalWorkflowState,
  FinalizeResult,
} from './types.ts'

export interface FinalReloadSnapshot {
  caseMetadata: CaseMetadata
  initialLockedAt: string | null
  final: FinalRead
}

export interface FinalWorkflowDependencies {
  initial: FinalRead
  replace(value: FinalReplacement, signal: AbortSignal): Promise<FinalRead>
  finalize(signal: AbortSignal): Promise<FinalizeResult>
  reload(signal: AbortSignal): Promise<FinalReloadSnapshot>
  onState(state: FinalWorkflowState): void
}

export interface FinalWorkflow {
  getState(): FinalWorkflowState
  replaceDraft(draft: FinalDraftMark[]): void
  accept(candidateId: string, mark: FinalMark): void
  dismiss(candidateId: string, reason: string): void
  remove(localId: string): void
  save(): Promise<void>
  finalize(): Promise<void>
  reconcile(): Promise<void>
  dispose(): void
}

let snapshotCounter = 0

function cloneDraft(draft: readonly FinalDraftMark[]): FinalDraftMark[] {
  return draft.map((entry) => ({ localId: entry.localId, mark: entry.mark }))
}

function marksOf(draft: readonly FinalDraftMark[]): FinalMark[] {
  return draft.map((entry) => entry.mark)
}

function sameDraft(a: readonly FinalDraftMark[], b: readonly FinalDraftMark[]): boolean {
  if (a.length !== b.length) {
    return false
  }
  for (let index = 0; index < a.length; index += 1) {
    if (a[index].localId !== b[index].localId) {
      return false
    }
  }
  return markListsEqual(marksOf(a), marksOf(b))
}

// Decisions are compared by candidate id and value, never by request order.
function decisionsKeyed(decisions: readonly CandidateDecision[]): Map<string, CandidateDecision> {
  return new Map(decisions.map((decision) => [decision.candidate_id, decision]))
}

function decisionsEqual(a: readonly CandidateDecision[], b: readonly CandidateDecision[]): boolean {
  if (a.length !== b.length) {
    return false
  }
  const right = decisionsKeyed(b)
  return a.every((decision) => {
    const other = right.get(decision.candidate_id)
    return (
      other !== undefined && other.decision === decision.decision && other.dismiss_reason === decision.dismiss_reason
    )
  })
}

function decisionValid(decision: CandidateDecision): boolean {
  if (decision.candidate_id.length === 0) {
    return false
  }
  if (decision.decision === 'accepted') {
    return decision.dismiss_reason === null
  }
  return typeof decision.dismiss_reason === 'string' && decision.dismiss_reason.trim().length > 0
}

function markValid(mark: FinalMark): boolean {
  if (!isFiniteVec3(mark.p1_mm) || !isFiniteVec3(mark.p2_mm) || !isFiniteVec3(mark.center_mm)) {
    return false
  }
  if (!(mark.diameter_mm > 0) || !Number.isFinite(mark.diameter_mm)) {
    return false
  }
  if (mark.p1_mm[0] === mark.p2_mm[0] && mark.p1_mm[1] === mark.p2_mm[1] && mark.p1_mm[2] === mark.p2_mm[2]) {
    return false
  }
  if (mark.view_plane_normal !== null) {
    if (!isFiniteVec3(mark.view_plane_normal)) {
      return false
    }
    if (mark.view_plane_normal[0] === 0 && mark.view_plane_normal[1] === 0 && mark.view_plane_normal[2] === 0) {
      return false
    }
  }
  if (mark.origin === 'from_ai_candidate') {
    return mark.ai_candidate_id !== null && mark.ai_candidate_id.length > 0
  }
  return mark.ai_candidate_id === null
}

// Whole replacement validation mirroring the backend rules. Returns the first
// fixed, neutral reason or null when the replacement may be dispatched.
function replacementError(draft: readonly FinalDraftMark[], decisions: readonly CandidateDecision[]): string | null {
  const accepted = new Set<string>()
  const seen = new Set<string>()
  for (const decision of decisions) {
    if (!decisionValid(decision) || seen.has(decision.candidate_id)) {
      return 'Quyết định candidate không hợp lệ; kiểm tra lý do bỏ qua.'
    }
    seen.add(decision.candidate_id)
    if (decision.decision === 'accepted') {
      accepted.add(decision.candidate_id)
    }
  }
  const linked = new Set<string>()
  for (const entry of draft) {
    const mark = entry.mark
    if (!markValid(mark)) {
      return 'Phép đo cuối không hợp lệ; vẽ lại hai điểm mút.'
    }
    if (mark.ai_candidate_id !== null) {
      if (!accepted.has(mark.ai_candidate_id)) {
        return 'Dấu gắn candidate cần quyết định chấp nhận trong cùng danh sách.'
      }
      if (linked.has(mark.ai_candidate_id)) {
        return 'Một candidate chỉ được gắn một dấu.'
      }
      linked.add(mark.ai_candidate_id)
    }
  }
  return null
}

function isUncertain(error: ReadApiError): boolean {
  return error.uncertain || (error.status === null && error.code === null)
}

function toApiError(error: unknown): ReadApiError {
  if (error instanceof ReadApiError) {
    return error
  }
  return new ReadApiError('unexpected workflow failure')
}

export function createFinalWorkflow(deps: FinalWorkflowDependencies): FinalWorkflow {
  let draft = cloneDraft(
    deps.initial.marks.map((mark, index) => ({ localId: `srv-${snapshotCounter}-${index}`, mark })),
  )
  const decisions = new Map<string, CandidateDecision>(
    deps.initial.candidate_decisions.map((decision) => [decision.candidate_id, decision]),
  )
  let revision = 0
  let savedRevision = 0
  let editable = deps.initial.locked_at === null
  let phase: FinalWorkflowState['phase'] = editable ? 'ready' : 'completed'
  let error: string | null = null
  let disposed = false
  let halted = false
  let frozen = false
  let loopPromise: Promise<void> | null = null
  let finalizePromise: Promise<void> | null = null
  let controller: AbortController | null = null
  let dispatched: { revision: number; marks: FinalMark[]; decisions: CandidateDecision[] } | null = null
  let lastConfirmedMarks: FinalMark[] = marksOf(draft)
  let lastConfirmedDecisions: CandidateDecision[] = [...decisions.values()]
  let snapshotEpoch = 0

  const snapshot = (): FinalWorkflowState => ({
    draft: cloneDraft(draft),
    decisions: [...decisions.values()],
    revision,
    savedRevision,
    phase,
    editable,
    error,
    snapshotEpoch,
  })

  const publish = (): void => {
    if (!disposed) {
      deps.onState(snapshot())
    }
  }

  const currentReplacement = (): FinalReplacement => ({
    marks: marksOf(draft),
    candidate_decisions: [...decisions.values()],
  })

  function replace(value: FinalRead): void {
    const token = (snapshotCounter += 1)
    draft = value.marks.map((mark, index) => ({ localId: `srv-${token}-${index}`, mark }))
    decisions.clear()
    for (const decision of value.candidate_decisions) {
      decisions.set(decision.candidate_id, decision)
    }
    lastConfirmedMarks = marksOf(draft)
    lastConfirmedDecisions = [...decisions.values()]
    revision = 0
    savedRevision = 0
    editable = value.locked_at === null
    halted = false
    frozen = false
    error = null
    dispatched = null
    snapshotEpoch += 1
    phase = editable ? 'ready' : 'completed'
    publish()
  }

  function markDirty(): void {
    revision += 1
    error = null
    if (phase === 'error' || phase === 'conflict') {
      publish()
      return
    }
    publish()
    if (!halted && !frozen) {
      void ensurePipeline()
    }
  }

  async function reconcileWith(
    compareMarks: FinalMark[],
    compareDecisions: CandidateDecision[],
    targetRevision: number,
  ): Promise<void> {
    const ctrl = new AbortController()
    controller = ctrl
    phase = 'reconciling'
    publish()
    let snapshotValue: FinalReloadSnapshot
    try {
      snapshotValue = await deps.reload(ctrl.signal)
    } catch (raw) {
      if (disposed) {
        return
      }
      const failure = toApiError(raw)
      if (failure.canceled) {
        return
      }
      halted = true
      error = 'Không đối chiếu được với máy chủ; cần tải lại trang.'
      phase = 'error'
      publish()
      return
    } finally {
      if (controller === ctrl) {
        controller = null
      }
    }
    if (disposed) {
      return
    }
    const lockedInitial = snapshotValue.initialLockedAt !== null
    const finalized = snapshotValue.caseMetadata.status === 'finalized'
    if (!lockedInitial) {
      halted = true
      error = 'Bản đọc đầu chưa khóa; trạng thái không nhất quán. Cần tải lại trang.'
      phase = 'error'
      publish()
      return
    }
    if (finalized || snapshotValue.final.locked_at !== null) {
      replace(snapshotValue.final)
      return
    }
    const equal =
      markListsEqual(snapshotValue.final.marks, compareMarks) &&
      decisionsEqual(snapshotValue.final.candidate_decisions, compareDecisions)
    if (equal) {
      savedRevision = Math.max(savedRevision, targetRevision)
      lastConfirmedMarks = compareMarks
      lastConfirmedDecisions = compareDecisions
      halted = false
      error = null
      phase = 'ready'
      publish()
      return
    }
    halted = true
    error = 'Bản cuối trên máy chủ khác với bản đang sửa; chọn “Lưu bản cuối” để ghi đè sau khi xem lại.'
    phase = 'conflict'
    publish()
  }

  async function handleWriteFailure(
    failure: ReadApiError,
    marks: FinalMark[],
    decisionsValue: CandidateDecision[],
    targetRevision: number,
  ): Promise<void> {
    if (failure.code === 'FINAL_READ_LOCKED' || failure.code === 'INITIAL_READ_REQUIRED') {
      await reconcileWith(marks, decisionsValue, targetRevision)
      return
    }
    if (failure.code === 'FINAL_READ_CONFLICT') {
      halted = true
      error = failure.message
      phase = 'conflict'
      publish()
      return
    }
    if (isUncertain(failure)) {
      await reconcileWith(marks, decisionsValue, targetRevision)
      return
    }
    halted = true
    error = failure.message
    phase = 'error'
    publish()
  }

  function responseMatches(value: FinalRead, dispatchedValue: { marks: FinalMark[]; decisions: CandidateDecision[] }): boolean {
    return markListsEqual(value.marks, dispatchedValue.marks) && decisionsEqual(value.candidate_decisions, dispatchedValue.decisions)
  }

  async function runPipeline(): Promise<void> {
    while (!disposed && editable && !halted && revision > savedRevision) {
      const target = revision
      const replacement = currentReplacement()
      const dispatchedValue = { marks: replacement.marks, decisions: replacement.candidate_decisions }
      const failure = replacementError(draft, dispatchedValue.decisions)
      if (failure !== null) {
        halted = true
        error = failure
        phase = 'error'
        publish()
        return
      }
      const ctrl = new AbortController()
      controller = ctrl
      dispatched = { revision: target, ...dispatchedValue }
      phase = 'saving'
      publish()
      try {
        const saved = await deps.replace(replacement, ctrl.signal)
        if (disposed) {
          return
        }
        if (!responseMatches(saved, dispatchedValue)) {
          dispatched = null
          await handleWriteFailure(
            new ReadApiError('save response did not match the request', { uncertain: true }),
            dispatchedValue.marks,
            dispatchedValue.decisions,
            target,
          )
          return
        }
        savedRevision = Math.max(savedRevision, target)
        lastConfirmedMarks = dispatchedValue.marks
        lastConfirmedDecisions = dispatchedValue.decisions
        dispatched = null
        phase = 'ready'
        publish()
      } catch (raw) {
        if (disposed) {
          return
        }
        const apiFailure = toApiError(raw)
        if (apiFailure.canceled) {
          return
        }
        await handleWriteFailure(apiFailure, dispatchedValue.marks, dispatchedValue.decisions, target)
        return
      } finally {
        if (controller === ctrl) {
          controller = null
        }
      }
    }
    if (!disposed && phase === 'saving') {
      phase = editable ? 'ready' : 'completed'
      publish()
    }
  }

  function ensurePipeline(): Promise<void> {
    if (loopPromise === null) {
      loopPromise = runPipeline().finally(() => {
        loopPromise = null
      })
    }
    return loopPromise
  }

  function replaceDraft(next: FinalDraftMark[]): void {
    if (disposed || !editable || frozen) {
      return
    }
    if (phase === 'finalizing' || phase === 'reconciling') {
      return
    }
    if (sameDraft(next, draft)) {
      return
    }
    draft = cloneDraft(next)
    // A linked measurement reported by the viewer implies its accepted decision:
    // the bridge owns the link, so the controller completes the required pair
    // instead of dispatching a payload the backend would reject.
    for (const entry of draft) {
      const candidateId = entry.mark.ai_candidate_id
      if (candidateId !== null && !decisions.has(candidateId)) {
        decisions.set(candidateId, { candidate_id: candidateId, decision: 'accepted', dismiss_reason: null })
      }
    }
    markDirty()
  }

  function accept(candidateId: string, mark: FinalMark): void {
    if (disposed || !editable || frozen) {
      return
    }
    const existingLink = draft.find((entry) => entry.mark.ai_candidate_id === candidateId)
    if (existingLink !== undefined) {
      return
    }
    const linked: FinalMark = { ...mark, origin: 'from_ai_candidate', ai_candidate_id: candidateId }
    if (candidateId.length === 0 || !markValid(linked)) {
      return
    }
    draft = [...draft, { localId: `link-${(snapshotCounter += 1)}`, mark: linked }]
    decisions.set(candidateId, { candidate_id: candidateId, decision: 'accepted', dismiss_reason: null })
    markDirty()
  }

  function dismiss(candidateId: string, reason: string): void {
    if (disposed || !editable || frozen) {
      return
    }
    if (reason.trim().length === 0) {
      return
    }
    draft = draft.filter((entry) => entry.mark.ai_candidate_id !== candidateId)
    decisions.set(candidateId, { candidate_id: candidateId, decision: 'dismissed', dismiss_reason: reason })
    markDirty()
  }

  function remove(localId: string): void {
    if (disposed || !editable || frozen) {
      return
    }
    const entry = draft.find((candidate) => candidate.localId === localId)
    if (entry === undefined) {
      return
    }
    draft = draft.filter((candidate) => candidate.localId !== localId)
    const candidateId = entry.mark.ai_candidate_id
    if (candidateId !== null) {
      decisions.delete(candidateId)
    }
    markDirty()
  }

  async function save(): Promise<void> {
    if (disposed || !editable) {
      return
    }
    halted = false
    while (!disposed && editable && revision > savedRevision) {
      await ensurePipeline()
      if (disposed || !editable || halted || revision <= savedRevision) {
        return
      }
    }
  }

  async function finalize(): Promise<void> {
    if (disposed || !editable) {
      return
    }
    if (finalizePromise !== null) {
      return finalizePromise
    }
    finalizePromise = (async () => {
      frozen = true
      halted = false
      publish()
      await save()
      if (disposed || !editable) {
        return
      }
      if (revision > savedRevision) {
        // The flush failed; the phase already reflects the failure and no POST
        // may be sent.
        frozen = false
        publish()
        return
      }
      const ctrl = new AbortController()
      controller = ctrl
      const dispatchedValue = currentReplacement()
      dispatched = { revision: savedRevision, marks: dispatchedValue.marks, decisions: dispatchedValue.candidate_decisions }
      phase = 'finalizing'
      publish()
      try {
        const result = await deps.finalize(ctrl.signal)
        if (disposed) {
          return
        }
        if (result.status !== 'finalized') {
          await reconcileWith(dispatchedValue.marks, dispatchedValue.candidate_decisions, savedRevision)
          return
        }
        lastConfirmedMarks = dispatchedValue.marks
        lastConfirmedDecisions = dispatchedValue.candidate_decisions
        dispatched = null
        editable = false
        halted = false
        error = null
        phase = 'completed'
      } catch (raw) {
        if (disposed) {
          return
        }
        const failure = toApiError(raw)
        if (failure.canceled) {
          return
        }
        if (failure.code === 'FINAL_READ_LOCKED' || failure.code === 'INITIAL_READ_REQUIRED' || isUncertain(failure)) {
          await reconcileWith(dispatchedValue.marks, dispatchedValue.candidate_decisions, savedRevision)
          return
        }
        frozen = false
        halted = true
        error = failure.message
        phase = failure.code === 'FINAL_READ_CONFLICT' ? 'conflict' : 'error'
      } finally {
        if (controller === ctrl) {
          controller = null
        }
        publish()
      }
    })().finally(() => {
      finalizePromise = null
    })
    return finalizePromise
  }

  async function reconcile(): Promise<void> {
    if (disposed) {
      return
    }
    frozen = false
    const compareMarks = dispatched?.marks ?? lastConfirmedMarks
    const compareDecisions = dispatched?.decisions ?? lastConfirmedDecisions
    const targetRevision = dispatched?.revision ?? savedRevision
    await reconcileWith(compareMarks, compareDecisions, targetRevision)
  }

  function dispose(): void {
    disposed = true
    controller?.abort()
    controller = null
    dispatched = null
    loopPromise = null
    finalizePromise = null
  }

  publish()

  return {
    getState: snapshot,
    replaceDraft,
    accept,
    dismiss,
    remove,
    save,
    finalize,
    reconcile,
    dispose,
  }
}
