// Serialized initial-read draft persistence. A tiny page-scoped controller:
// at most one whole-list PUT is in flight, edits coalesce into the latest
// snapshot, submit flushes the latest revision before its single POST, and an
// uncertain outcome freezes writes and reconciles with the server instead of
// replaying. No DOM, no timers, no debounce, no automatic retry.

import type { CaseMetadata } from '../case.ts'
import { ReadApiError } from '../reads/http.ts'
import { markListsEqual } from './geometry.ts'
import type { DraftMark, InitialMark, InitialRead, SubmitResult, WorkflowPhase, WorkflowState } from './types.ts'

export interface InitialSnapshot {
  caseMetadata: CaseMetadata
  read: InitialRead
}

export interface WorkflowDependencies {
  caseMetadata: CaseMetadata
  initialRead: InitialRead
  initialDraft: DraftMark[]
  replaceMarks(marks: readonly InitialMark[], signal: AbortSignal): Promise<InitialMark[]>
  submitRead(signal: AbortSignal): Promise<SubmitResult>
  loadSnapshot(signal: AbortSignal): Promise<InitialSnapshot>
  onState(state: WorkflowState): void
}

export interface InitialWorkflow {
  getState(): WorkflowState
  replaceDraft(draft: DraftMark[]): void
  save(): Promise<void>
  submit(): Promise<void>
  reconcile(): Promise<void>
  dispose(): void
}

let snapshotCounter = 0

function cloneDraft(draft: readonly DraftMark[]): DraftMark[] {
  return draft.map((entry) => ({ localId: entry.localId, mark: entry.mark }))
}

function marksOf(draft: readonly DraftMark[]): InitialMark[] {
  return draft.map((entry) => entry.mark)
}

function sameDraft(a: readonly DraftMark[], b: readonly DraftMark[]): boolean {
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

// A write whose definitive status/code never arrived may already have been
// applied server-side.
function isUncertain(error: ReadApiError): boolean {
  return error.uncertain || (error.status === null && error.code === null)
}

function toApiError(error: unknown): ReadApiError {
  if (error instanceof ReadApiError) {
    return error
  }
  return new ReadApiError('unexpected workflow failure')
}

export function createInitialWorkflow(deps: WorkflowDependencies): InitialWorkflow {
  let draft = cloneDraft(deps.initialDraft)
  let revision = 0
  let savedRevision = 0
  let editable = deps.initialRead.locked_at === null
  let phase: WorkflowPhase = editable ? 'ready' : 'locked'
  let error: string | null = null
  let snapshotEpoch = 0
  let disposed = false
  let halted = false
  let loopPromise: Promise<void> | null = null
  let submitPromise: Promise<void> | null = null
  let controller: AbortController | null = null
  let dispatched: { revision: number; marks: InitialMark[] } | null = null
  let lastConfirmedMarks: InitialMark[] = marksOf(draft)

  const snapshot = (): WorkflowState => ({
    draft: cloneDraft(draft),
    revision,
    savedRevision,
    phase,
    error,
    editable,
    snapshotEpoch,
  })

  const publish = (): void => {
    if (!disposed) {
      deps.onState(snapshot())
    }
  }

  const setPhase = (next: WorkflowPhase): void => {
    phase = next
    publish()
  }

  const applyLockedSnapshot = (read: InitialRead): void => {
    const token = (snapshotCounter += 1)
    draft = read.marks.map((mark, index) => ({ localId: `server-${token}-${index}`, mark }))
    lastConfirmedMarks = marksOf(draft)
    revision = 0
    savedRevision = 0
    editable = false
    halted = false
    error = null
    dispatched = null
    snapshotEpoch += 1
    phase = 'locked'
    publish()
  }

  async function reconcileWith(compareTo: InitialMark[], targetRevision: number): Promise<void> {
    const ctrl = new AbortController()
    controller = ctrl
    phase = 'reconciling'
    publish()
    let snap: InitialSnapshot
    try {
      snap = await deps.loadSnapshot(ctrl.signal)
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
    const locked = snap.read.locked_at !== null
    const status = snap.caseMetadata.status
    const coherent = locked ? status === 'initial_locked' || status === 'finalized' : status === 'awaiting_read'
    if (!coherent) {
      halted = true
      error = 'Trạng thái ca và bản đọc không nhất quán; cần tải lại trang.'
      phase = 'error'
      publish()
      return
    }
    if (locked) {
      applyLockedSnapshot(snap.read)
      return
    }
    if (markListsEqual(snap.read.marks, compareTo)) {
      savedRevision = Math.max(savedRevision, targetRevision)
      lastConfirmedMarks = compareTo
      halted = false
      error = null
      phase = 'ready'
      publish()
      return
    }
    halted = true
    error = 'Bản nháp trên máy chủ khác với bản đang sửa; chọn “Lưu nháp” để ghi đè sau khi xem lại.'
    phase = 'conflict'
    publish()
  }

  async function handleWriteFailure(failure: ReadApiError, marks: InitialMark[], targetRevision: number): Promise<void> {
    if (failure.code === 'INITIAL_READ_LOCKED') {
      await reconcileWith(marks, targetRevision)
      return
    }
    if (failure.code === 'INITIAL_READ_CONFLICT') {
      halted = true
      error = failure.message
      phase = 'conflict'
      publish()
      return
    }
    if (isUncertain(failure)) {
      await reconcileWith(marks, targetRevision)
      return
    }
    halted = true
    error = failure.message
    phase = 'error'
    publish()
  }

  async function runPipeline(): Promise<void> {
    while (!disposed && editable && !halted && revision > savedRevision) {
      const target = revision
      const marks = marksOf(draft)
      const ctrl = new AbortController()
      controller = ctrl
      dispatched = { revision: target, marks }
      phase = 'saving'
      publish()
      try {
        const saved = await deps.replaceMarks(marks, ctrl.signal)
        if (disposed) {
          return
        }
        if (!Array.isArray(saved) || saved.length !== marks.length) {
          await handleWriteFailure(new ReadApiError('save response did not match the request', { uncertain: true }), marks, target)
          return
        }
        savedRevision = Math.max(savedRevision, target)
        lastConfirmedMarks = marks
        dispatched = null
        phase = 'ready'
        publish()
      } catch (raw) {
        if (disposed) {
          return
        }
        const failure = toApiError(raw)
        if (failure.canceled) {
          return
        }
        await handleWriteFailure(failure, marks, target)
        return
      } finally {
        if (controller === ctrl) {
          controller = null
        }
      }
    }
    if (!disposed && phase === 'saving') {
      phase = editable ? 'ready' : 'locked'
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

  function replaceDraft(next: DraftMark[]): void {
    if (disposed || !editable) {
      return
    }
    if (phase === 'submitting' || phase === 'reconciling') {
      return
    }
    if (sameDraft(next, draft)) {
      return
    }
    draft = cloneDraft(next)
    revision += 1
    error = null
    if (phase === 'error' || phase === 'conflict') {
      // A new edit while halted stays dirty until an explicit save/reconcile.
      publish()
      return
    }
    publish()
    if (!halted) {
      void ensurePipeline()
    }
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

  async function submit(): Promise<void> {
    if (disposed || !editable) {
      return
    }
    if (submitPromise !== null) {
      return submitPromise
    }
    submitPromise = (async () => {
      halted = false
      await save()
      if (disposed || !editable) {
        return
      }
      if (revision > savedRevision) {
        // The flush failed; the phase already reflects the failure.
        return
      }
      const ctrl = new AbortController()
      controller = ctrl
      const snapshotMarks = marksOf(draft)
      dispatched = { revision: savedRevision, marks: snapshotMarks }
      phase = 'submitting'
      publish()
      try {
        const result = await deps.submitRead(ctrl.signal)
        if (disposed) {
          return
        }
        if (result.status !== 'initial_locked') {
          await reconcileWith(snapshotMarks, savedRevision)
          return
        }
        lastConfirmedMarks = snapshotMarks
        dispatched = null
        editable = false
        halted = false
        error = null
        phase = 'locked'
      } catch (raw) {
        if (disposed) {
          return
        }
        const failure = toApiError(raw)
        if (failure.canceled) {
          return
        }
        if (failure.code === 'INITIAL_READ_LOCKED') {
          await reconcileWith(snapshotMarks, savedRevision)
          return
        }
        if (failure.code === 'INITIAL_READ_CONFLICT') {
          halted = true
          error = failure.message
          phase = 'conflict'
          return
        }
        if (isUncertain(failure)) {
          await reconcileWith(snapshotMarks, savedRevision)
          return
        }
        halted = true
        error = failure.message
        phase = 'error'
      } finally {
        if (controller === ctrl) {
          controller = null
        }
        publish()
      }
    })().finally(() => {
      submitPromise = null
    })
    return submitPromise
  }

  async function reconcile(): Promise<void> {
    if (disposed) {
      return
    }
    const compareTo = dispatched?.marks ?? lastConfirmedMarks
    const targetRevision = dispatched?.revision ?? savedRevision
    await reconcileWith(compareTo, targetRevision)
  }

  function dispose(): void {
    disposed = true
    controller?.abort()
    controller = null
    dispatched = null
    loopPromise = null
    submitPromise = null
  }

  return {
    getState: snapshot,
    replaceDraft,
    save,
    submit,
    reconcile,
    dispose,
  }
}
