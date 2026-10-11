// Owned Cornerstone Length annotation bridge shared by the initial and final
// reads. One session owns exactly the annotations it created (restored or
// drawn): it never calls global removeAllAnnotations/unlockAllAnnotations and
// never touches another generation's or another tool's annotations. It reports
// completed geometry changes to the caller; persistence belongs to the workflow
// controller.
//
// The bridge is generic over the read's mark type so the initial read keeps its
// stricter `reader`/required-normal contract while the final read can carry the
// three origins, a null stored normal and an optional candidate link. Only the
// reusable measurement/restoration logic is shared; each read owns its API,
// validation and workflow.
//
// Interaction timing: the pinned tools defer the custom MOUSE_UP (and therefore
// LengthTool's completion) behind a ~400 ms double-click window, so an
// end-of-interaction signal cannot be assumed to be synchronous with the DOM
// pointer event. The bridge therefore reports completion only once the tools'
// own `isInteractingWithTool` flag clears, and it never drops a pending
// annotation unless that annotation is provably zero-length (a click without a
// drag), which the tool never completes.

import { eventTarget } from '@cornerstonejs/core'
import type { StackViewport } from '@cornerstonejs/core'
import {
  Enums as ToolEnums,
  LengthTool,
  annotation,
  cancelActiveManipulations,
  state as toolsState,
  utilities as toolUtils,
} from '@cornerstonejs/tools'
import type { Types as ToolkitTypes } from '@cornerstonejs/tools'

import { MarkGeometryError, findReadMarkSlice, measureEndpoints } from '../reads/geometry.ts'
import type { MarkOrigin, MeasuredGeometry, ReadMark } from '../reads/geometry.ts'
import type { SeriesPlan, Vec3 } from './series.ts'

const { Events: ToolEvents, ChangeTypes } = ToolEnums

const UNSUPPORTED_MESSAGE =
  'Một số dấu đã lưu không hiển thị được trên chuỗi ảnh trục này. Việc chỉnh sửa và lưu đã bị tạm dừng; tải lại để thử lại.'

// Poll cadence while waiting for the tools to release a deferred interaction,
// and the point at which the UI is unblocked regardless (an abandoned click
// must not leave Save disabled forever).
const END_POLL_MS = 25
const END_POLL_LIMIT = 60

export interface ReadDraftRecord<M extends ReadMark> {
  localId: string
  mark: M
}

interface Record<M extends ReadMark> {
  localId: string
  uid: string
  mark: M
  sliceIndex: number
  normal: Vec3
}

export interface ReadAnnotationSessionOptions<M extends ReadMark> {
  viewport: StackViewport
  element: HTMLDivElement
  series: SeriesPlan
  frameUID: string
  marks: ReadDraftRecord<M>[]
  editable: boolean
  // Origin assigned to a newly drawn, unlinked measurement.
  defaultOrigin: MarkOrigin
  // Builds the read's mark type from measured geometry plus the resolved
  // origin/candidate link. The initial read ignores origin/candidate.
  buildMark(geometry: MeasuredGeometry, origin: MarkOrigin, candidateId: string | null): M
  onDraftChange(marks: ReadDraftRecord<M>[]): void
  onDrawingChange(drawing: boolean): void
  onError(message: string): void
}

export interface ReadAnnotationSession<M extends ReadMark> {
  setEditable(editable: boolean): void
  // Arms the next completed measurement as a linked mark for the candidate.
  beginLinkedDraw(candidateId: string): void
  cancelLinkedDraw(): void
  focus(localId: string): Promise<void>
  focusSlice(sliceIndex: number): Promise<void>
  remove(localId: string): void
  dispose(): void
}

let sessionCounter = 0
let localCounter = 0

function toPoint3(vector: Vec3): [number, number, number] {
  return [vector[0], vector[1], vector[2]]
}

function normalizeVec3(vector: Vec3): Vec3 {
  const length = Math.hypot(vector[0], vector[1], vector[2])
  if (!Number.isFinite(length) || length === 0) {
    return [0, 0, 1]
  }
  return [vector[0] / length, vector[1] / length, vector[2] / length]
}

function defaultTextBox(): ToolkitTypes.ToolSpecificAnnotationTypes.LengthAnnotation['data']['handles']['textBox'] {
  const zero = (): [number, number, number] => [0, 0, 0]
  return {
    hasMoved: false,
    worldPosition: zero(),
    worldBoundingBox: { topLeft: zero(), topRight: zero(), bottomLeft: zero(), bottomRight: zero() },
  }
}

export function attachReadAnnotations<M extends ReadMark>(
  options: ReadAnnotationSessionOptions<M>,
): ReadAnnotationSession<M> {
  const { viewport, element, series, frameUID, editable } = options
  const sessionId = (sessionCounter += 1)

  const callbacks = {
    onDraftChange: options.onDraftChange,
    onDrawingChange: options.onDrawingChange,
    onError: options.onError,
  }

  const records: Record<M>[] = []
  const byUid = new Map<string, Record<M>>()
  const pendingNewUids = new Set<string>()
  let suppress = true
  let disposed = false
  let blocked = false
  let currentEditable = editable
  let drawing = false
  let dirty = false
  let pendingCandidateId: string | null = null
  let endPollTimer: number | null = null

  const draft = (): ReadDraftRecord<M>[] => records.map((record) => ({ localId: record.localId, mark: record.mark }))

  const sliceIndexOfImageId = (imageId: string | undefined): number => {
    if (typeof imageId !== 'string') {
      return viewport.getCurrentImageIdIndex()
    }
    const index = series.slices.findIndex((slice) => slice.imageId === imageId)
    return index === -1 ? viewport.getCurrentImageIdIndex() : index
  }

  const noteOf = (annot: ToolkitTypes.Annotation): string | null =>
    annot.data?.label === undefined || annot.data.label === '' ? null : String(annot.data.label)

  const geometryFromAnnotation = (annot: ToolkitTypes.Annotation, normal: Vec3): MeasuredGeometry | null => {
    const points = annot.data?.handles?.points
    if (!Array.isArray(points) || points.length !== 2) {
      return null
    }
    try {
      return measureEndpoints(points[0] as Vec3, points[1] as Vec3, normal, noteOf(annot))
    } catch (error) {
      if (error instanceof MarkGeometryError) {
        return null
      }
      throw error
    }
  }

  const isOurs = (annot: ToolkitTypes.Annotation): boolean =>
    annot.metadata?.toolName === LengthTool.toolName && annot.metadata?.FrameOfReferenceUID === frameUID

  const cameraNormal = (): Vec3 => normalizeVec3(viewport.getCamera().viewPlaneNormal as Vec3)

  const buildAnnotation = (mark: M, normal: Vec3, sliceImageId: string, viewUp: Vec3): ToolkitTypes.ToolSpecificAnnotationTypes.LengthAnnotation => {
    return {
      annotationUID: undefined,
      highlighted: false,
      invalidated: true,
      isLocked: false,
      isVisible: true,
      metadata: {
        toolName: LengthTool.toolName,
        FrameOfReferenceUID: frameUID,
        referencedImageId: sliceImageId,
        viewPlaneNormal: toPoint3(normal),
        viewUp: toPoint3(viewUp),
      },
      data: {
        handles: {
          points: [toPoint3(mark.p1_mm), toPoint3(mark.p2_mm)],
          activeHandleIndex: null,
          textBox: defaultTextBox(),
        },
        cachedStats: {},
        label: mark.note ?? '',
      },
    }
  }

  const requestRender = (): void => {
    try {
      toolUtils.triggerAnnotationRenderForViewportIds([viewport.id])
    } catch {
      // A destroyed viewport must not turn a cleanup path into an error.
    }
  }

  const removeAnnotationQuietly = (uid: string): void => {
    suppress = true
    try {
      annotation.state.removeAnnotation(uid)
    } finally {
      suppress = false
    }
  }

  const lockRecord = (record: Record<M>): void => {
    if (!currentEditable) {
      annotation.locking.setAnnotationLocked(record.uid, true)
    }
  }

  // --- restoration ---------------------------------------------------------

  if (viewport.getFrameOfReferenceUID() !== frameUID || series.frameOfReferenceUID !== frameUID) {
    blocked = true
    callbacks.onError(UNSUPPORTED_MESSAGE)
    return makeSession()
  }

  const viewUp = normalizeVec3(viewport.getCamera().viewUp as Vec3)
  const restored: Array<{ localId: string; mark: M; sliceIndex: number; normal: Vec3 }> = []
  for (const entry of options.marks) {
    try {
      restored.push({
        localId: entry.localId,
        mark: entry.mark,
        sliceIndex: findReadMarkSlice(entry.mark, series),
        // A null stored normal is inferred from the endpoints; the annotation
        // carries the camera normal for display, and the persisted null is kept
        // untouched on the record until an endpoint edit.
        normal: entry.mark.view_plane_normal ?? normalizeVec3(series.normal),
      })
    } catch {
      blocked = true
      break
    }
  }

  if (blocked) {
    callbacks.onError(UNSUPPORTED_MESSAGE)
    suppress = false
    return makeSession()
  }

  for (const entry of restored) {
    const built = buildAnnotation(entry.mark, entry.normal, series.slices[entry.sliceIndex].imageId, viewUp)
    const uid = annotation.state.addAnnotation(built, element)
    const record: Record<M> = {
      localId: entry.localId,
      uid,
      mark: entry.mark,
      sliceIndex: entry.sliceIndex,
      normal: entry.normal,
    }
    records.push(record)
    byUid.set(uid, record)
    lockRecord(record)
  }
  requestRender()
  suppress = false

  // --- interaction completion ---------------------------------------------

  // Adopts any pending annotation whose geometry became valid (a drag the tools
  // never explicitly completed) and drops the zero-length leftover of a click.
  // Only ever runs after the pointer is up, so a live drag is never touched.
  const adoptPending = (): void => {
    for (const uid of [...pendingNewUids]) {
      pendingNewUids.delete(uid)
      const annot = annotation.state.getAnnotation(uid)
      if (annot === undefined) {
        continue
      }
      const normal = cameraNormal()
      const geometry = geometryFromAnnotation(annot, normal)
      if (geometry === null) {
        removeAnnotationQuietly(uid)
        continue
      }
      const origin: MarkOrigin = pendingCandidateId !== null ? 'from_ai_candidate' : options.defaultOrigin
      const mark = options.buildMark(geometry, origin, pendingCandidateId)
      pendingCandidateId = null
      const record: Record<M> = {
        localId: `m-${sessionId}-${(localCounter += 1)}`,
        uid,
        mark,
        sliceIndex: sliceIndexOfImageId(annot.metadata?.referencedImageId),
        normal,
      }
      records.push(record)
      byUid.set(uid, record)
      lockRecord(record)
      dirty = true
    }
    requestRender()
  }

  const finishInteraction = (): void => {
    adoptPending()
    if (drawing) {
      drawing = false
      callbacks.onDrawingChange(false)
    }
    if (dirty) {
      dirty = false
      callbacks.onDraftChange(draft())
    }
  }

  const pollEnd = (attempt: number): void => {
    endPollTimer = null
    if (disposed) {
      return
    }
    if (toolsState.isInteractingWithTool && attempt < END_POLL_LIMIT) {
      endPollTimer = window.setTimeout(() => pollEnd(attempt + 1), END_POLL_MS)
      return
    }
    finishInteraction()
  }

  const cancelEndPoll = (): void => {
    if (endPollTimer !== null) {
      window.clearTimeout(endPollTimer)
      endPollTimer = null
    }
  }

  // Restarts the settle timer so it fires once the event burst stops, which is
  // well after the tools dispatch their deferred pointer-up handling.
  const scheduleEnd = (): void => {
    if (disposed) {
      return
    }
    cancelEndPoll()
    endPollTimer = window.setTimeout(() => pollEnd(1), 0)
  }

  const adoptCompleted = (annot: ToolkitTypes.Annotation, uid: string): void => {
    const normal = cameraNormal()
    const geometry = geometryFromAnnotation(annot, normal)
    if (geometry === null) {
      removeAnnotationQuietly(uid)
      finishInteraction()
      return
    }
    // The tools may complete an annotation the bridge already dropped as a
    // zero-length leftover; re-register it so it renders again.
    if (annotation.state.getAnnotation(uid) === undefined) {
      annotation.state.addAnnotation(annot, element)
    }
    const origin: MarkOrigin = pendingCandidateId !== null ? 'from_ai_candidate' : options.defaultOrigin
    const mark = options.buildMark(geometry, origin, pendingCandidateId)
    pendingCandidateId = null
    const record: Record<M> = {
      localId: `m-${sessionId}-${(localCounter += 1)}`,
      uid,
      mark,
      sliceIndex: sliceIndexOfImageId(annot.metadata?.referencedImageId),
      normal,
    }
    records.push(record)
    byUid.set(uid, record)
    lockRecord(record)
    dirty = true
    requestRender()
    finishInteraction()
  }

  const onAdded: EventListener = (raw) => {
    if (disposed || suppress) {
      return
    }
    const detail = (raw as CustomEvent<{ annotation?: ToolkitTypes.Annotation; viewportId?: string }>).detail
    const annot = detail?.annotation
    if (annot === undefined || !isOurs(annot) || detail.viewportId !== viewport.id) {
      return
    }
    const uid = annot.annotationUID
    if (typeof uid !== 'string' || byUid.has(uid)) {
      return
    }
    pendingNewUids.add(uid)
    if (!drawing) {
      drawing = true
      callbacks.onDrawingChange(true)
    }
    scheduleEnd()
  }

  const onCompleted: EventListener = (raw) => {
    if (disposed || suppress) {
      return
    }
    const annot = (raw as CustomEvent<{ annotation?: ToolkitTypes.Annotation }>).detail?.annotation
    if (annot === undefined || !isOurs(annot)) {
      return
    }
    const uid = annot.annotationUID
    if (typeof uid !== 'string') {
      return
    }
    pendingNewUids.delete(uid)
    adoptCompleted(annot, uid)
  }

  const onModified: EventListener = (raw) => {
    if (disposed || suppress) {
      return
    }
    const detail = (raw as CustomEvent<{ annotation?: ToolkitTypes.Annotation; changeType?: string }>).detail
    const annot = detail?.annotation
    if (annot === undefined || detail.changeType === ChangeTypes.StatsUpdated) {
      return
    }
    const record = byUid.get(annot.annotationUID ?? '')
    if (record === undefined) {
      return
    }
    // Editing endpoints preserves the origin, candidate link and note; only the
    // measured geometry is rewritten.
    const geometry = geometryFromAnnotation(annot, record.normal)
    if (geometry === null) {
      return
    }
    record.mark = options.buildMark(geometry, record.mark.origin, record.mark.ai_candidate_id)
    record.sliceIndex = sliceIndexOfImageId(annot.metadata?.referencedImageId)
    record.normal = [...geometry.view_plane_normal]
    dirty = true
    if (!drawing) {
      drawing = true
      callbacks.onDrawingChange(true)
    }
    scheduleEnd()
  }

  const onRemoved: EventListener = (raw) => {
    if (disposed || suppress) {
      return
    }
    const annot = (raw as CustomEvent<{ annotation?: ToolkitTypes.Annotation }>).detail?.annotation
    const uid = annot?.annotationUID
    if (typeof uid !== 'string') {
      return
    }
    const record = byUid.get(uid)
    if (record === undefined) {
      return
    }
    byUid.delete(uid)
    const index = records.indexOf(record)
    if (index !== -1) {
      records.splice(index, 1)
    }
    callbacks.onDraftChange(draft())
  }

  eventTarget.addEventListener(ToolEvents.ANNOTATION_ADDED, onAdded)
  eventTarget.addEventListener(ToolEvents.ANNOTATION_COMPLETED, onCompleted)
  eventTarget.addEventListener(ToolEvents.ANNOTATION_MODIFIED, onModified)
  eventTarget.addEventListener(ToolEvents.ANNOTATION_REMOVED, onRemoved)

  const onWindowPointerUp = (): void => {
    scheduleEnd()
  }
  window.addEventListener('mouseup', onWindowPointerUp, true)
  window.addEventListener('touchend', onWindowPointerUp, true)

  // --- public session ------------------------------------------------------

  function setEditable(next: boolean): void {
    if (disposed) {
      return
    }
    currentEditable = next
    if (!next) {
      cancelEndPoll()
      try {
        cancelActiveManipulations(element)
      } catch {
        // No active manipulation to cancel.
      }
      finishInteraction()
    }
    for (const record of records) {
      annotation.locking.setAnnotationLocked(record.uid, !next)
    }
    requestRender()
  }

  function beginLinkedDraw(candidateId: string): void {
    if (disposed || !currentEditable) {
      return
    }
    pendingCandidateId = candidateId
  }

  function cancelLinkedDraw(): void {
    if (disposed) {
      return
    }
    pendingCandidateId = null
  }

  async function focus(localId: string): Promise<void> {
    const record = records.find((entry) => entry.localId === localId)
    if (record === undefined) {
      return
    }
    try {
      await viewport.setImageIdIndex(record.sliceIndex)
    } catch {
      // A slice change that cannot complete must not throw out of the UI.
    }
    annotation.selection.setAnnotationSelected(record.uid, true)
  }

  async function focusSlice(sliceIndex: number): Promise<void> {
    if (disposed || sliceIndex < 0 || sliceIndex >= series.slices.length) {
      return
    }
    try {
      await viewport.setImageIdIndex(sliceIndex)
    } catch {
      // A slice change that cannot complete must not throw out of the UI.
    }
  }

  function remove(localId: string): void {
    if (disposed || blocked || !currentEditable) {
      return
    }
    const record = records.find((entry) => entry.localId === localId)
    if (record === undefined) {
      return
    }
    byUid.delete(record.uid)
    const index = records.indexOf(record)
    if (index !== -1) {
      records.splice(index, 1)
    }
    removeAnnotationQuietly(record.uid)
    requestRender()
    callbacks.onDraftChange(draft())
  }

  function dispose(): void {
    if (disposed) {
      return
    }
    disposed = true
    cancelEndPoll()
    window.removeEventListener('mouseup', onWindowPointerUp, true)
    window.removeEventListener('touchend', onWindowPointerUp, true)
    eventTarget.removeEventListener(ToolEvents.ANNOTATION_ADDED, onAdded)
    eventTarget.removeEventListener(ToolEvents.ANNOTATION_COMPLETED, onCompleted)
    eventTarget.removeEventListener(ToolEvents.ANNOTATION_MODIFIED, onModified)
    eventTarget.removeEventListener(ToolEvents.ANNOTATION_REMOVED, onRemoved)
    try {
      cancelActiveManipulations(element)
    } catch {
      // Nothing active.
    }
    suppress = true
    for (const record of records.slice()) {
      try {
        annotation.state.removeAnnotation(record.uid)
      } catch {
        // Already gone with the viewport.
      }
    }
    records.length = 0
    byUid.clear()
    pendingNewUids.clear()
  }

  function makeSession(): ReadAnnotationSession<M> {
    return { setEditable, beginLinkedDraw, cancelLinkedDraw, focus, focusSlice, remove, dispose }
  }

  return makeSession()
}
