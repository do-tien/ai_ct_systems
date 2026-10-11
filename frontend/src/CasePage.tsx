import { useCallback, useEffect, useRef, useState } from 'react'

import { loadCase } from './case.ts'
import type { CaseMetadata } from './case.ts'
import { loadInitialRead, replaceInitialMarks, submitInitialRead } from './initialread/api.ts'
import { createInitialWorkflow } from './initialread/workflow.ts'
import type { InitialWorkflow } from './initialread/workflow.ts'
import type { DraftMark, InitialMark, InitialRead, WorkflowState } from './initialread/types.ts'
import { finalizeCase, loadAI, loadFinalRead, replaceFinalRead } from './finalread/api.ts'
import { createFinalWorkflow } from './finalread/workflow.ts'
import type { FinalWorkflow } from './finalread/workflow.ts'
import { createAISession, selectReadMode, timeoutScheduler } from './finalread/aiSession.ts'
import type { AISession, AISessionState } from './finalread/aiSession.ts'
import type { FinalDraftMark, FinalMark, FinalRead, FinalWorkflowState } from './finalread/types.ts'
import type { ReadAnnotationSession } from './viewer/readAnnotations.ts'
import type { ViewerSession } from './viewer/Viewer.tsx'
import Viewer from './viewer/Viewer.tsx'
import AiPanel from './AiPanel.tsx'
import FinalizeBar from './FinalizeBar.tsx'
import MarkList from './MarkList.tsx'
import SubmitBar from './SubmitBar.tsx'

const LOAD_ERROR_MESSAGE = 'Không tải được ca hoặc bản đọc đầu. Kiểm tra kết nối rồi thử lại.'
const FINAL_LOAD_ERROR_MESSAGE = 'Không tải được bản đọc cuối. Kiểm tra kết nối rồi thử lại.'
const CONFLICT_MESSAGE =
  'Trạng thái ca và bản đọc không nhất quán; không tiết lộ hay sửa dữ liệu. Tải lại để thử lại.'

function useNow(): number {
  return typeof performance === 'undefined' ? Date.now() : performance.now()
}

interface BaseLoaded {
  caseMetadata: CaseMetadata
  initial: InitialRead
}

// Entry gate: loads the case and initial read, then selects one of the three
// page modes. A coherent combination mounts a pane; anything else fails closed
// with a manual reload and never loads AI or final data.
export default function CasePage({ caseId }: { caseId: number }) {
  const [base, setBase] = useState<BaseLoaded | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(true)
  const [generation, setGeneration] = useState(0)

  const mountedRef = useRef(true)
  const requestRef = useRef<AbortController | null>(null)

  const run = useCallback(async () => {
    requestRef.current?.abort()
    const controller = new AbortController()
    requestRef.current = controller
    setBase(null)
    setError(null)
    setPending(true)
    setGeneration((value) => value + 1)
    try {
      const [caseMetadata, initial] = await Promise.all([
        loadCase(caseId, controller.signal),
        loadInitialRead(caseId, controller.signal),
      ])
      if (!mountedRef.current || controller.signal.aborted) {
        return
      }
      setBase({ caseMetadata, initial })
    } catch {
      if (mountedRef.current && !controller.signal.aborted) {
        setError(LOAD_ERROR_MESSAGE)
      }
    } finally {
      if (mountedRef.current && requestRef.current === controller) {
        setPending(false)
      }
    }
  }, [caseId])

  useEffect(() => {
    mountedRef.current = true
    void run()
    return () => {
      mountedRef.current = false
      requestRef.current?.abort()
    }
  }, [run])

  if (base === null) {
    return (
      <section className="panel" aria-labelledby="case-heading">
        <h2 id="case-heading">Ca #{caseId}</h2>
        <p>Đang tải thông tin ca và bản đọc đầu…</p>
        {error !== null && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <button type="button" onClick={() => void run()} disabled={pending}>
          Thử lại
        </button>
      </section>
    )
  }

  const mode = selectReadMode(base.caseMetadata, base.initial.locked_at)
  if (mode === 'conflict') {
    return (
      <section className="panel" aria-labelledby="case-heading">
        <h2 id="case-heading">Ca #{caseId}</h2>
        <p className="error" role="alert">
          {CONFLICT_MESSAGE}
        </p>
        <button type="button" onClick={() => void run()}>
          Tải lại
        </button>
      </section>
    )
  }

  if (mode === 'independent') {
    return (
      <InitialReadPane
        key={generation}
        caseId={caseId}
        caseMetadata={base.caseMetadata}
        initial={base.initial}
        onReload={() => void run()}
      />
    )
  }

  return (
    <FinalReadPane
      key={generation}
      caseId={caseId}
      caseMetadata={base.caseMetadata}
      initial={base.initial}
      completed={mode === 'completed'}
      onReload={() => void run()}
    />
  )
}

// --- independent read (LC1-15) --------------------------------------------

function InitialReadPane({
  caseId,
  caseMetadata,
  initial,
  onReload,
}: {
  caseId: number
  caseMetadata: CaseMetadata
  initial: InitialRead
  onReload: () => void
}) {
  const [openedAt, setOpenedAt] = useState(0)
  const [state, setState] = useState<WorkflowState | null>(null)
  const [drawing, setDrawing] = useState(false)
  const [blocked, setBlocked] = useState<string | null>(null)

  const mountedRef = useRef(true)
  const workflowRef = useRef<InitialWorkflow | null>(null)
  const sessionRef = useRef<ReadAnnotationSession<InitialMark> | null>(null)

  const initialDraft = useRef<DraftMark[]>(initial.marks.map((mark, index) => ({ localId: `srv-${index}`, mark })))

  useEffect(() => {
    mountedRef.current = true
    setOpenedAt(useNow())
    return () => {
      mountedRef.current = false
      workflowRef.current?.dispose()
      workflowRef.current = null
      sessionRef.current = null
    }
  }, [])

  useEffect(() => {
    const flow = createInitialWorkflow({
      caseMetadata,
      initialRead: initial,
      initialDraft: initialDraft.current,
      replaceMarks: (marks, signal) => replaceInitialMarks(caseId, marks, signal),
      submitRead: (signal) => submitInitialRead(caseId, signal),
      loadSnapshot: async (signal) => {
        const [nextCase, read] = await Promise.all([loadCase(caseId, signal), loadInitialRead(caseId, signal)])
        return { caseMetadata: nextCase, read }
      },
      onState: (next) => {
        if (mountedRef.current) {
          setState(next)
        }
      },
    })
    workflowRef.current = flow
    setState(flow.getState())
    return () => {
      flow.dispose()
      if (workflowRef.current === flow) {
        workflowRef.current = null
      }
    }
  }, [caseId, caseMetadata, initial])

  const warnOnUnload =
    state !== null &&
    (state.revision > state.savedRevision || state.phase === 'saving' || state.phase === 'submitting' || state.phase === 'reconciling')
  useEffect(() => {
    if (!warnOnUnload) {
      return
    }
    const handler = (event: BeforeUnloadEvent): void => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', handler)
    return () => window.removeEventListener('beforeunload', handler)
  }, [warnOnUnload])

  if (state === null) {
    return <p>Đang chuẩn bị bản đọc đầu…</p>
  }

  const busy = state.phase === 'saving' || state.phase === 'submitting' || state.phase === 'reconciling'
  const viewerEditable = state.editable && blocked === null && state.phase !== 'submitting' && state.phase !== 'reconciling'
  const rowEditable = viewerEditable && !busy && !drawing
  const dirty = state.revision > state.savedRevision

  return (
    <>
      <Viewer
        caseMetadata={caseMetadata}
        openedAt={openedAt}
        onRetry={onReload}
        initialRead={{
          draft: state.draft,
          editable: viewerEditable,
          sessionEpoch: state.snapshotEpoch,
          onDraftChange: (draft) => workflowRef.current?.replaceDraft(draft),
          onDrawingChange: setDrawing,
          onError: (message) => setBlocked(message),
          onSession: (session) => {
            sessionRef.current = session
          },
        }}
      />

      <section className="panel" aria-labelledby="initial-read-heading">
        <h2 id="initial-read-heading">Bản đọc đầu độc lập</h2>
        <p className="notice">Tọa độ LPS mm; tên ca và bản đọc không chứa thông tin AI.</p>

        {blocked !== null && (
          <p className="error" role="alert">
            {blocked}
          </p>
        )}

        <MarkList
          draft={state.draft}
          editable={rowEditable}
          onFocus={(localId) => void sessionRef.current?.focus(localId)}
          onRemove={(localId) => sessionRef.current?.remove(localId)}
        />

        <SubmitBar
          phase={state.phase}
          error={state.error}
          dirty={dirty}
          busy={busy}
          drawing={drawing}
          canEdit={viewerEditable}
          onSave={() => void workflowRef.current?.save()}
          onSubmit={() => void workflowRef.current?.submit()}
        />

        {(state.phase === 'error' || state.phase === 'conflict') && (
          <button type="button" onClick={() => void workflowRef.current?.reconcile()} disabled={busy}>
            Đối chiếu với máy chủ
          </button>
        )}
      </section>
    </>
  )
}

// --- post-lock review / completed read (LC1-16) ---------------------------

function FinalReadPane({
  caseId,
  caseMetadata,
  initial,
  completed,
  onReload,
}: {
  caseId: number
  caseMetadata: CaseMetadata
  initial: InitialRead
  completed: boolean
  onReload: () => void
}) {
  const [openedAt, setOpenedAt] = useState(0)
  const [final, setFinal] = useState<FinalRead | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [state, setState] = useState<FinalWorkflowState | null>(null)
  const [aiState, setAiState] = useState<AISessionState>({ projection: null, pending: true, error: null })
  const [drawing, setDrawing] = useState(false)
  const [blocked, setBlocked] = useState<string | null>(null)
  const [measureArm, setMeasureArm] = useState(0)

  const mountedRef = useRef(true)
  const workflowRef = useRef<FinalWorkflow | null>(null)
  const sessionRef = useRef<ViewerSession<FinalMark> | null>(null)
  const aiSessionRef = useRef<AISession | null>(null)

  useEffect(() => {
    mountedRef.current = true
    setOpenedAt(useNow())
    return () => {
      mountedRef.current = false
      workflowRef.current?.dispose()
      workflowRef.current = null
      aiSessionRef.current?.dispose()
      aiSessionRef.current = null
      sessionRef.current = null
    }
  }, [])

  // Final data loads independently of AI: an AI failure must not disable an
  // otherwise valid reader-only final read or completion.
  useEffect(() => {
    const controller = new AbortController()
    let cancelled = false
    setFinal(null)
    setState(null)
    setLoadError(null)
    void (async () => {
      try {
        const read = await loadFinalRead(caseId, controller.signal)
        if (!cancelled) {
          setFinal(read)
        }
      } catch {
        if (!cancelled) {
          setLoadError(FINAL_LOAD_ERROR_MESSAGE)
        }
      }
    })()
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [caseId])

  useEffect(() => {
    if (final === null) {
      return
    }
    const flow = createFinalWorkflow({
      initial: final,
      replace: (value, signal) => replaceFinalRead(caseId, value, signal),
      finalize: (signal) => finalizeCase(caseId, signal),
      reload: async (signal) => {
        const [nextCase, nextInitial, nextFinal] = await Promise.all([
          loadCase(caseId, signal),
          loadInitialRead(caseId, signal),
          loadFinalRead(caseId, signal),
        ])
        return { caseMetadata: nextCase, initialLockedAt: nextInitial.locked_at, final: nextFinal }
      },
      onState: (next) => {
        if (mountedRef.current) {
          setState(next)
        }
      },
    })
    workflowRef.current = flow
    setState(flow.getState())
    return () => {
      flow.dispose()
      if (workflowRef.current === flow) {
        workflowRef.current = null
      }
    }
  }, [final, caseId])

  // Review polls pending AI; completed mode performs exactly one status load and
  // never starts a poll.
  useEffect(() => {
    if (completed) {
      const controller = new AbortController()
      void loadAI(caseId, controller.signal)
        .then((projection) => {
          if (mountedRef.current) {
            setAiState({ projection, pending: false, error: null })
          }
        })
        .catch(() => {
          if (mountedRef.current) {
            setAiState({ projection: null, pending: false, error: 'Không tải được trạng thái AI.' })
          }
        })
      return () => controller.abort()
    }
    const session = createAISession({
      load: (signal) => loadAI(caseId, signal),
      schedule: timeoutScheduler,
      onState: (next) => {
        if (mountedRef.current) {
          setAiState(next)
        }
      },
    })
    aiSessionRef.current = session
    session.start()
    return () => {
      session.dispose()
      if (aiSessionRef.current === session) {
        aiSessionRef.current = null
      }
    }
  }, [caseId, completed])

  // Declared before the loading early-return so the hook count is stable across
  // the loading → loaded transition (Rules of Hooks). `state` is null while the
  // final read is still loading, so the guard is defensive.
  const warnOnUnload =
    state !== null &&
    (state.revision > state.savedRevision ||
      state.phase === 'saving' ||
      state.phase === 'finalizing' ||
      state.phase === 'reconciling')
  useEffect(() => {
    if (!warnOnUnload) {
      return
    }
    const handler = (event: BeforeUnloadEvent): void => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', handler)
    return () => window.removeEventListener('beforeunload', handler)
  }, [warnOnUnload])

  if (final === null || state === null) {
    return (
      <section className="panel" aria-labelledby="final-heading">
        <h2 id="final-heading">Bản cuối sau khóa</h2>
        <p>Đang tải bản đọc cuối…</p>
        {loadError !== null && (
          <p className="error" role="alert">
            {loadError}
          </p>
        )}
        <button type="button" onClick={onReload}>
          Thử lại
        </button>
      </section>
    )
  }

  const busy = state.phase === 'saving' || state.phase === 'finalizing' || state.phase === 'reconciling'
  const editable = state.editable && blocked === null
  const viewerEditable = editable && state.phase !== 'finalizing' && state.phase !== 'reconciling'
  const rowEditable = viewerEditable && !busy && !drawing
  const dirty = state.revision > state.savedRevision
  const projection = aiState.projection
  const candidates = projection !== null && projection.status === 'succeeded' ? projection.candidates : []
  const linked = new Set(
    state.draft.map((entry) => entry.mark.ai_candidate_id).filter((value): value is string => value !== null),
  )

  const focusCandidate = (candidateId: string): void => {
    void sessionRef.current?.focusCandidate(candidateId)
  }

  const acceptCandidate = (candidateId: string): void => {
    const existing = state.draft.find((entry) => entry.mark.ai_candidate_id === candidateId)
    if (existing !== undefined) {
      void sessionRef.current?.focus(existing.localId)
      return
    }
    sessionRef.current?.beginLinkedDraw(candidateId)
    void sessionRef.current?.focusCandidate(candidateId)
    setMeasureArm((value) => value + 1)
  }


  return (
    <>
      <Viewer
        caseMetadata={caseMetadata}
        openedAt={openedAt}
        onRetry={onReload}
        armMeasure={measureArm}
        finalRead={{
          draft: state.draft,
          editable: viewerEditable,
          sessionEpoch: state.snapshotEpoch,
          defaultOrigin: 'reader_after_ai',
          candidates,
          onDraftChange: (draft: FinalDraftMark[]) => workflowRef.current?.replaceDraft(draft),
          onDrawingChange: setDrawing,
          onError: (message) => setBlocked(message),
          onSession: (session) => {
            sessionRef.current = session
          },
        }}
      />

      <AiPanel
        state={aiState}
        decisions={state.decisions}
        linkedCandidates={linked}
        canDecide={rowEditable}
        onFocus={focusCandidate}
        onAccept={acceptCandidate}
        onDismiss={(candidateId, reason) => workflowRef.current?.dismiss(candidateId, reason)}
        onRetry={() => aiSessionRef.current?.retry()}
      />

      <section className="panel" aria-labelledby="final-heading">
        <h2 id="final-heading">{completed ? 'Bản cuối (đã hoàn tất, chỉ đọc)' : 'Bản cuối sau khóa'}</h2>
        <p className="notice">
          Tọa độ LPS mm; bản cuối chỉ đọc sau khi hoàn tất ca. Bản đọc đầu #{initial.id} đã khóa.
        </p>

        {blocked !== null && (
          <p className="error" role="alert">
            {blocked}
          </p>
        )}

        <MarkList
          draft={state.draft}
          editable={rowEditable}
          onFocus={(localId) => void sessionRef.current?.focus(localId)}
          onRemove={(localId) => sessionRef.current?.remove(localId)}
        />

        <FinalizeBar
          phase={state.phase}
          error={state.error}
          dirty={dirty}
          busy={busy}
          drawing={drawing}
          canEdit={viewerEditable}
          onSave={() => void workflowRef.current?.save()}
          onFinalize={() => void workflowRef.current?.finalize()}
        />

        {(state.phase === 'error' || state.phase === 'conflict') && (
          <button type="button" onClick={() => void workflowRef.current?.reconcile()} disabled={busy}>
            Đối chiếu với máy chủ
          </button>
        )}
      </section>
    </>
  )
}
