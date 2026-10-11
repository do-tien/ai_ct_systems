import { useCallback, useEffect, useRef, useState } from 'react'
import { Enums, EVENTS, RenderingEngine, cache, eventTarget } from '@cornerstonejs/core'
import type { StackViewport } from '@cornerstonejs/core'
import { Enums as ToolEnums, LengthTool, StackScrollTool, ToolGroupManager, WindowLevelTool, ZoomTool, Types as ToolkitTypes } from '@cornerstonejs/tools'
import { wadors } from '@cornerstonejs/dicom-image-loader'
import type { Types } from '@cornerstonejs/dicom-image-loader'

import type { CaseMetadata } from '../case.ts'
import type { DraftMark, InitialMark } from '../initialread/types.ts'
import type { FinalDraftMark, FinalMark, Candidate } from '../finalread/types.ts'
import type { MarkOrigin, MeasuredGeometry } from '../reads/geometry.ts'
import { locateCandidate } from '../finalread/geometry.ts'
import { loadSeries } from './loadSeries.ts'
import type { SeriesPlan, Vec3 } from './series.ts'
import { edgeLabels } from './orientation.ts'
import type { EdgeLabels } from './orientation.ts'
import { attachReadAnnotations } from './readAnnotations.ts'
import type { ReadAnnotationSession } from './readAnnotations.ts'
import { initializeCornerstone } from './cornerstone.ts'

// Initial-read data and callbacks supplied by CasePage. Absent for a plain
// LC1-14 viewing session, which keeps the annotation bridge disabled.
export interface ViewerInitialRead {
  draft: DraftMark[]
  editable: boolean
  // Bumped when the draft is replaced by a freshly loaded server snapshot, so
  // the annotation session is rebuilt without touching the viewport.
  sessionEpoch: number
  onDraftChange: (draft: DraftMark[]) => void
  onDrawingChange: (drawing: boolean) => void
  onError: (message: string) => void
  onSession: (session: ReadAnnotationSession<InitialMark> | null) => void
}

// A viewer session handle: the owned annotation session plus candidate slice
// focus, which the AiPanel drives. Candidate slice focus is pure navigation and
// is never the gated candidate glyph overlay.
export interface ViewerSession<M extends ReadMarkShape> extends ReadAnnotationSession<M> {
  focusCandidate(candidateId: string): Promise<void>
}

type ReadMarkShape = InitialMark | FinalMark

interface ViewerSessionHandle {
  setEditable(editable: boolean): void
  focus(localId: string): Promise<void>
  focusSlice(sliceIndex: number): Promise<void>
  focusCandidate(candidateId: string): Promise<void>
  remove(localId: string): void
  beginLinkedDraw(candidateId: string): void
  cancelLinkedDraw(): void
  dispose(): void
}

// Final-read data supplied by CasePage when the initial read is locked. The
// candidate list drives slice focus only; no overlay is attached here.
export interface ViewerFinalRead {
  draft: FinalDraftMark[]
  editable: boolean
  sessionEpoch: number
  defaultOrigin: MarkOrigin
  candidates: readonly Candidate[]
  onDraftChange: (draft: FinalDraftMark[]) => void
  onDrawingChange: (drawing: boolean) => void
  onError: (message: string) => void
  onSession: (session: ViewerSession<FinalMark> | null) => void
}

interface ViewerProps {
  caseMetadata: CaseMetadata
  openedAt: number
  onRetry: () => void
  initialRead?: ViewerInitialRead
  finalRead?: ViewerFinalRead
  // A bump asks the viewer to switch to the measurement tool, used when the
  // physician starts an explicit linked measurement from a candidate.
  armMeasure?: number
}

type ToolMode = 'window' | 'zoom' | 'measure'
type Phase = 'loading' | 'ready' | 'error'

const SERIES_MESSAGE = 'Không tải được dữ liệu chuỗi ảnh CT. Kiểm tra kết nối rồi thử lại.'
const RENDER_MESSAGE = 'Không tải hoặc giải mã được ảnh CT. Kiểm tra kết nối rồi thử lại.'
const VIEWPORT_MESSAGE = 'Không khởi tạo được vùng hiển thị ảnh (WebGL). Thử lại hoặc dùng trình duyệt khác.'
const FRAME_MISMATCH_MESSAGE = 'Ảnh hiển thị không thuộc đúng ca (Frame of Reference khác). Đã dừng hiển thị.'

// Image metadata is registered globally by imageId, so a rapid unmount/remount
// that describes the same imageIds must not let the retired generation remove
// metadata the active generation still needs.
const metadataOwners = new Map<string, number>()
let idCounter = 0

function acquireImageMetadata(imageId: string, metadata: Record<string, unknown>): void {
  const owners = metadataOwners.get(imageId) ?? 0
  if (owners === 0) {
    wadors.metaDataManager.add(imageId, metadata as Types.WADORSMetaData)
  }
  metadataOwners.set(imageId, owners + 1)
}

function releaseImageMetadata(imageId: string): void {
  const owners = metadataOwners.get(imageId)
  if (owners === undefined) {
    return
  }
  if (owners > 1) {
    metadataOwners.set(imageId, owners - 1)
    return
  }
  metadataOwners.delete(imageId)
  wadors.metaDataManager.remove(imageId)
}

function add(a: Vec3, b: Vec3): Vec3 {
  return [a[0] + b[0], a[1] + b[1], a[2] + b[2]]
}

function scale(vector: Vec3, factor: number): Vec3 {
  return [vector[0] * factor, vector[1] * factor, vector[2] * factor]
}

// Screen-right/screen-down in LPS, measured through the viewport's own
// world-to-canvas mapping: the in-plane combination of row and column direction
// whose canvas delta is exactly +x (right) or +y (down). Deriving labels from
// the real mapping avoids a fixed letter guess on a rotated or mirrored camera.
function measureEdgeLabels(viewport: StackViewport, plan: SeriesPlan): EdgeLabels | null {
  const index = Math.min(Math.max(viewport.getCurrentImageIdIndex(), 0), plan.slices.length - 1)
  const origin = plan.slices[index].imagePositionPatient
  const base = viewport.worldToCanvas(origin)
  const alongRow = viewport.worldToCanvas(add(origin, scale(plan.rowDirection, 1)))
  const alongColumn = viewport.worldToCanvas(add(origin, scale(plan.columnDirection, 1)))
  const row = [alongRow[0] - base[0], alongRow[1] - base[1]]
  const column = [alongColumn[0] - base[0], alongColumn[1] - base[1]]
  const determinant = row[0] * column[1] - row[1] * column[0]
  if (!Number.isFinite(determinant) || Math.abs(determinant) < 1e-9) {
    return null
  }
  const toPlane = (canvasX: number, canvasY: number): Vec3 => {
    const a = (canvasX * column[1] - canvasY * column[0]) / determinant
    const b = (row[0] * canvasY - row[1] * canvasX) / determinant
    return add(scale(plan.rowDirection, a), scale(plan.columnDirection, b))
  }
  const screenRight = toPlane(1, 0)
  const screenDown = toPlane(0, 1)
  const length = (v: Vec3): number => Math.hypot(v[0], v[1], v[2])
  if (length(screenRight) === 0 || length(screenDown) === 0) {
    return null
  }
  return edgeLabels(
    [screenRight[0] / length(screenRight), screenRight[1] / length(screenRight), screenRight[2] / length(screenRight)],
    [screenDown[0] / length(screenDown), screenDown[1] / length(screenDown), screenDown[2] / length(screenDown)],
  )
}

function applyToolMode(
  toolGroup: ToolkitTypes.IToolGroup,
  mode: ToolMode,
  measureAvailable: boolean,
  editable: boolean,
): void {
  const { MouseBindings } = ToolEnums
  toolGroup.setToolActive(StackScrollTool.toolName, {
    bindings: [{ mouseButton: MouseBindings.Wheel }],
  })
  if (measureAvailable) {
    if (mode === 'measure' && editable) {
      toolGroup.setToolActive(LengthTool.toolName, {
        bindings: [{ mouseButton: MouseBindings.Primary }],
      })
    } else {
      // Passive keeps restored annotations rendered and selectable but disables
      // new-annotation creation; combined with locked annotations it is the
      // read-only state.
      toolGroup.setToolPassive(LengthTool.toolName)
    }
  }
  if (mode === 'zoom') {
    toolGroup.setToolActive(ZoomTool.toolName, {
      bindings: [{ mouseButton: MouseBindings.Primary }, { mouseButton: MouseBindings.Secondary }],
    })
    toolGroup.setToolPassive(WindowLevelTool.toolName)
  } else if (mode === 'measure') {
    toolGroup.setToolActive(ZoomTool.toolName, {
      bindings: [{ mouseButton: MouseBindings.Secondary }],
    })
    toolGroup.setToolPassive(WindowLevelTool.toolName)
  } else {
    toolGroup.setToolActive(WindowLevelTool.toolName, {
      bindings: [{ mouseButton: MouseBindings.Primary }],
    })
    toolGroup.setToolActive(ZoomTool.toolName, {
      bindings: [{ mouseButton: MouseBindings.Secondary }],
    })
  }
}

export default function Viewer({ caseMetadata, openedAt, onRetry, initialRead, finalRead, armMeasure }: ViewerProps) {
  const elementRef = useRef<HTMLDivElement | null>(null)
  const viewportRef = useRef<StackViewport | null>(null)
  const planRef = useRef<SeriesPlan | null>(null)
  const toolGroupRef = useRef<ToolkitTypes.IToolGroup | null>(null)
  const sessionRef = useRef<ViewerSessionHandle | null>(null)
  const refreshRef = useRef<() => void>(() => {})

  const [phase, setPhase] = useState<Phase>('loading')
  const [message, setMessage] = useState<string | null>(null)
  const [sliceIndex, setSliceIndex] = useState(0)
  const [total, setTotal] = useState(0)
  const [labels, setLabels] = useState<EdgeLabels | null>(null)
  const [openMs, setOpenMs] = useState<number | null>(null)
  const [mode, setMode] = useState<ToolMode>('window')
  // Set once the viewport is genuinely rendered; the annotation session is
  // created off it, independent of later transient phases such as 'saving'.
  const [attached, setAttached] = useState(false)

  // The latest read props, read by the mount/session effects without rebuilding
  // the viewport when only callbacks or draft identity change. A viewer instance
  // serves exactly one read: the initial read or the post-lock final read.
  const initialReadRef = useRef(initialRead)
  initialReadRef.current = initialRead
  const finalReadRef = useRef(finalRead)
  finalReadRef.current = finalRead
  const hasRead = initialRead !== undefined || finalRead !== undefined
  const canEdit = (initialRead?.editable ?? false) || (finalRead?.editable ?? false)
  const canEditRef = useRef(canEdit)
  canEditRef.current = canEdit
  const modeRef = useRef(mode)
  modeRef.current = mode
  const hasReadRef = useRef(hasRead)
  hasReadRef.current = hasRead
  const sessionEpoch = initialRead?.sessionEpoch ?? finalRead?.sessionEpoch ?? 0

  useEffect(() => {
    const element = elementRef.current
    if (element === null) {
      return
    }
    let disposed = false
    let ready = false
    let engine: RenderingEngine | null = null
    let viewport: StackViewport | null = null
    let resizeObserver: ResizeObserver | null = null
    const ownedImageIds: string[] = []
    const listeners: Array<[EventTarget, string, EventListener]> = []
    const controller = new AbortController()
    const engineId = `lc1-14-engine-${(idCounter += 1)}`
    const viewportId = `lc1-14-viewport-${idCounter}`
    const toolGroupId = `lc1-14-toolgroup-${idCounter}`

    setPhase('loading')
    setMessage(null)
    setOpenMs(null)
    setLabels(null)
    setSliceIndex(0)
    setTotal(0)
    setAttached(false)

    const useNow = (): number => (typeof performance === 'undefined' ? Date.now() : performance.now())

    const registerListener = (target: EventTarget, type: string, handler: EventListener): void => {
      target.addEventListener(type, handler)
      listeners.push([target, type, handler])
    }

    const refreshOrientation = (): void => {
      if (disposed || viewport === null || planRef.current === null) {
        return
      }
      try {
        const next = measureEdgeLabels(viewport, planRef.current)
        if (next !== null) {
          setLabels(next)
        }
      } catch {
        // A transient mapping failure must not replace a working view; the next
        // camera/image event recomputes the labels.
      }
    }
    refreshRef.current = refreshOrientation

    const fail = (text: string): void => {
      if (disposed) {
        return
      }
      disposed = true
      viewportRef.current = null
      toolGroupRef.current = null
      setAttached(false)
      setPhase('error')
      setMessage(text)
      setLabels(null)
      setSliceIndex(0)
    }

    // Distinguishes an initialization failure from a series-data failure from a
    // viewport failure, so the user sees a neutral message that matches what
    // actually failed.
    let stage: 'init' | 'series' | 'viewport' = 'init'

    const onImageRendered: EventListener = () => {
      const current = viewportRef.current
      const plan = planRef.current
      if (disposed || current === null || plan === null) {
        return
      }
      setSliceIndex(current.getCurrentImageIdIndex())
      if (!ready) {
        let renderedFrame = ''
        try {
          renderedFrame = current.getFrameOfReferenceUID()
        } catch {
          renderedFrame = ''
        }
        if (renderedFrame !== caseMetadata.frameOfReferenceUID) {
          fail(FRAME_MISMATCH_MESSAGE)
          return
        }
        ready = true
        setPhase('ready')
        setMessage(null)
        setOpenMs(useNow() - openedAt)
        if (hasReadRef.current) {
          setAttached(true)
        }
      }
      refreshOrientation()
    }

    const onCameraModified: EventListener = () => {
      refreshOrientation()
    }

    // Frame failures are dispatched by core on its module-level event target
    // (never on the element), and a failed frame does not reject setStack, so
    // this is the only place a load/decode failure becomes visible.
    const onImageLoadError: EventListener = (raw) => {
      const detail = (raw as CustomEvent<{ imageId?: string }>).detail
      const imageId = detail?.imageId
      if (typeof imageId === 'string' && !ownedImageIds.includes(imageId)) {
        return
      }
      fail(RENDER_MESSAGE)
    }

    registerListener(element, EVENTS.IMAGE_RENDERED, onImageRendered)
    registerListener(element, EVENTS.CAMERA_MODIFIED, onCameraModified)
    registerListener(eventTarget, EVENTS.IMAGE_LOAD_FAILED, onImageLoadError)
    registerListener(eventTarget, EVENTS.IMAGE_LOAD_ERROR, onImageLoadError)
    registerListener(element, 'contextmenu', (event) => event.preventDefault())

    void (async () => {
      try {
        await initializeCornerstone()
        if (disposed) {
          return
        }
        stage = 'series'
        const plan = await loadSeries(caseMetadata, controller.signal)
        if (disposed) {
          return
        }
        planRef.current = plan
        for (const slice of plan.slices) {
          acquireImageMetadata(slice.imageId, slice.metadata)
          ownedImageIds.push(slice.imageId)
        }
        setTotal(plan.slices.length)
        stage = 'viewport'

        engine = new RenderingEngine(engineId)
        engine.enableElement({
          viewportId,
          type: Enums.ViewportType.STACK,
          element,
        })
        viewport = engine.getViewport(viewportId) as StackViewport
        viewportRef.current = viewport

        const toolGroup = ToolGroupManager.createToolGroup(toolGroupId)
        if (toolGroup === undefined) {
          throw new Error('tool group could not be created')
        }
        toolGroup.addTool(WindowLevelTool.toolName)
        toolGroup.addTool(ZoomTool.toolName)
        toolGroup.addTool(StackScrollTool.toolName)
        if (hasReadRef.current) {
          toolGroup.addTool(LengthTool.toolName)
        }
        applyToolMode(toolGroup, modeRef.current, hasReadRef.current, canEditRef.current)
        toolGroup.addViewport(viewportId, engineId)
        toolGroupRef.current = toolGroup

        resizeObserver = new ResizeObserver(() => {
          if (disposed || engine === null) {
            return
          }
          engine.resize()
          refreshOrientation()
        })
        resizeObserver.observe(element)

        const middleIndex = Math.floor(plan.slices.length / 2)
        await viewport.setStack(ownedImageIds, middleIndex)
        if (disposed) {
          return
        }
        refreshOrientation()
      } catch {
        if (!disposed) {
          setMessage(stage === 'series' ? SERIES_MESSAGE : VIEWPORT_MESSAGE)
          setPhase('error')
          setLabels(null)
          setSliceIndex(0)
        }
      }
    })()

    return () => {
      disposed = true
      refreshRef.current = () => {}
      controller.abort()
      if (sessionRef.current !== null) {
        sessionRef.current.dispose()
        sessionRef.current = null
      }
      if (resizeObserver !== null) {
        resizeObserver.disconnect()
      }
      for (const [target, type, handler] of listeners) {
        target.removeEventListener(type, handler)
      }
      if (viewportRef.current === viewport) {
        viewportRef.current = null
      }
      toolGroupRef.current = null
      if (ToolGroupManager.getToolGroup(toolGroupId) !== undefined) {
        ToolGroupManager.destroyToolGroup(toolGroupId)
      }
      if (engine !== null) {
        try {
          engine.destroy()
        } catch {
          // A partially constructed engine may already be gone.
        }
      }
      // Owned cache entries are dropped so a retired generation cannot keep
      // decoded frames alive. Loader-level cancellation is deliberately not
      // used: core's cancelLoadImages throws for cached load objects that carry
      // no cancelFn handle, and an xhr.abort() rejection surfaces as an
      // unhandled page error; both were observed to break the retry commit.
      for (const imageId of ownedImageIds) {
        try {
          cache.removeImageLoadObject(imageId, { force: true })
        } catch {
          // Not every imageId has a cached load object.
        }
        releaseImageMetadata(imageId)
      }
      planRef.current = null
    }
    // `mode` is intentionally read once at mount; the dedicated effect below
    // re-applies a mode change without rebuilding the viewport.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [caseMetadata, openedAt])

  // Annotation session: created once the viewport is genuinely rendered and a
  // validated read is present; rebuilt (without touching the viewport) when the
  // server snapshot epoch changes after reconciliation. Exactly one read drives
  // a viewer instance.
  useEffect(() => {
    const viewport = viewportRef.current
    const plan = planRef.current
    const element = elementRef.current
    const ir = initialReadRef.current
    const fr = finalReadRef.current
    if (!attached || viewport === null || plan === null || element === null || (ir === undefined && fr === undefined)) {
      return
    }
    sessionRef.current?.dispose()
    sessionRef.current = null

    const focusCandidateFor = (candidates: readonly Candidate[]) => async (candidateId: string): Promise<void> => {
      const candidate = candidates.find((value) => value.id === candidateId)
      if (candidate === undefined) {
        return
      }
      try {
        const located = locateCandidate(candidate, plan)
        await viewport.setImageIdIndex(located.sliceIndex)
      } catch {
        // A candidate that cannot be located in this series focuses nothing.
      }
    }

    if (ir !== undefined) {
      const session = attachReadAnnotations<InitialMark>({
        viewport,
        element,
        series: plan,
        frameUID: caseMetadata.frameOfReferenceUID,
        marks: ir.draft,
        editable: ir.editable,
        defaultOrigin: 'reader',
        buildMark: (geometry: MeasuredGeometry) => ({ ...geometry, origin: 'reader', ai_candidate_id: null }),
        onDraftChange: (draft) => ir.onDraftChange(draft),
        onDrawingChange: (drawing) => ir.onDrawingChange(drawing),
        onError: (message) => ir.onError(message),
      })
      const initialSession: ViewerSessionHandle = { ...session, focusCandidate: async () => {} }
      sessionRef.current = initialSession
      ir.onSession(session)
      return () => {
        if (sessionRef.current === initialSession) {
          sessionRef.current = null
        }
        session.dispose()
        ir.onSession(null)
      }
    }

    const candidates = fr!.candidates
    const session = attachReadAnnotations<FinalMark>({
      viewport,
      element,
      series: plan,
      frameUID: caseMetadata.frameOfReferenceUID,
      marks: fr!.draft,
      editable: fr!.editable,
      defaultOrigin: fr!.defaultOrigin,
      buildMark: (geometry: MeasuredGeometry, origin: MarkOrigin, candidateId: string | null) => ({
        ...geometry,
        origin,
        ai_candidate_id: candidateId,
      }),
      onDraftChange: (draft) => fr!.onDraftChange(draft),
      onDrawingChange: (drawing) => fr!.onDrawingChange(drawing),
      onError: (message) => fr!.onError(message),
    })
    const enriched = { ...session, focusCandidate: focusCandidateFor(candidates) }
    sessionRef.current = enriched
    fr!.onSession(enriched)
    return () => {
      if (sessionRef.current === enriched) {
        sessionRef.current = null
      }
      session.dispose()
      fr!.onSession(null)
    }
  }, [attached, sessionEpoch, caseMetadata, openedAt])

  useEffect(() => {
    sessionRef.current?.setEditable(canEdit)
  }, [canEdit, attached, sessionEpoch])

  useEffect(() => {
    const toolGroup = toolGroupRef.current
    if (toolGroup !== null && phase === 'ready') {
      applyToolMode(toolGroup, mode, hasReadRef.current, canEditRef.current)
    }
  }, [mode, phase, canEdit])

  // A pending submit or a lock removes the measure mode for the current
  // selection; the annotation session still owns visibility.
  useEffect(() => {
    if (!canEdit && mode === 'measure') {
      setMode('window')
    }
  }, [canEdit, mode])

  // An explicit linked measurement asks the viewer to select the measure tool.
  useEffect(() => {
    if (armMeasure !== undefined && canEdit) {
      setMode('measure')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [armMeasure])

  const step = useCallback((delta: number) => {
    const viewport = viewportRef.current
    const plan = planRef.current
    if (viewport === null || plan === null) {
      return
    }
    const next = viewport.getCurrentImageIdIndex() + delta
    if (next < 0 || next >= plan.slices.length) {
      return
    }
    void viewport.setImageIdIndex(next).catch(() => {})
  }, [])

  const usable = phase === 'ready'

  return (
    <section className="panel viewer" aria-labelledby="viewer-heading">
      <h2 id="viewer-heading">Ảnh CT trục</h2>
      <p className="notice">
        Ca #{caseMetadata.id} — công cụ nghiên cứu, không có lớp phủ AI và không dùng để chẩn đoán.
      </p>

      <div className="viewer-toolbar" role="group" aria-label="Chế độ chuột">
        <button type="button" className={mode === 'window' ? 'active' : ''} disabled={!usable} onClick={() => setMode('window')}>
          Cửa sổ / mức
        </button>
        <button type="button" className={mode === 'zoom' ? 'active' : ''} disabled={!usable} onClick={() => setMode('zoom')}>
          Thu phóng
        </button>
        {hasRead && (
          <button
            type="button"
            className={mode === 'measure' ? 'active' : ''}
            disabled={!usable || !canEdit}
            onClick={() => setMode('measure')}
          >
            Đo nốt
          </button>
        )}
        <span className="viewer-spacer" />
        <button type="button" disabled={!usable || sliceIndex <= 0} onClick={() => step(-1)}>
          Lát trước
        </button>
        <span className="viewer-counter" aria-live="polite">
          {usable ? `${sliceIndex + 1} / ${total}` : '—'}
        </span>
        <button type="button" disabled={!usable || sliceIndex >= total - 1} onClick={() => step(1)}>
          Lát sau
        </button>
      </div>

      <div className="viewer-stage">
        <div className="viewer-canvas" ref={elementRef} />
        {labels !== null && (
          <div className="viewer-labels" aria-hidden="true">
            <span className="viewer-label viewer-label-top">{labels.top}</span>
            <span className="viewer-label viewer-label-bottom">{labels.bottom}</span>
            <span className="viewer-label viewer-label-left">{labels.left}</span>
            <span className="viewer-label viewer-label-right">{labels.right}</span>
          </div>
        )}
        {phase === 'loading' && <p className="viewer-overlay">Đang tải ảnh CT…</p>}
        {phase === 'error' && message !== null && (
          <div className="viewer-overlay viewer-overlay-error" role="alert">
            <p>{message}</p>
            <button type="button" onClick={onRetry}>
              Thử lại
            </button>
          </div>
        )}
      </div>

      {phase === 'ready' && openMs !== null && (
        <p className="viewer-timing">Thời gian hiển thị khung đầu: {Math.round(openMs)} ms</p>
      )}
    </section>
  )
}
