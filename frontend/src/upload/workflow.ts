// Serialized upload → select → open workflow. Pure TypeScript with injected
// dependencies and a plain state object, so it runs under Node type stripping
// without jsdom or a browser. Every asynchronous continuation is guarded by the
// generation it started in: a superseded upload, a reload restore or a
// disposed panel can never publish stale state or open the wrong case.

import { UploadApiError, messageForCode } from './api.ts'
import type { UploadCaseResult, UploadSession, UploadState } from './types.ts'

// sessionStorage key holding the current upload session token. Only the opaque
// token is stored; File objects are never claimed to be restored.
export const UPLOAD_SESSION_KEY = 'nodule-reader.upload.session-id'

// The server's own multipart budget (2 GiB including framing) and file ceiling,
// mirrored so an impossible selection is refused before a long transfer starts.
// The server stays authoritative: these only avoid a doomed upload.
export const MAX_UPLOAD_BYTES = 2 * 1024 ** 3
export const MAX_UPLOAD_FILES = 10_000

export interface UploadStorage {
  get(key: string): string | null
  set(key: string, value: string): void
  remove(key: string): void
}

export interface UploadTransferOptions {
  signal: AbortSignal
  onProgress: (transferred: number, total: number) => void
}

export interface UploadDependencies {
  upload(files: File[], options: UploadTransferOptions): Promise<UploadSession>
  load(sessionId: string, signal: AbortSignal): Promise<UploadSession>
  confirm(sessionId: string, seriesId: string, signal: AbortSignal): Promise<UploadCaseResult>
  discard(sessionId: string, signal: AbortSignal): Promise<void>
  storage: UploadStorage
}

export interface UploadController {
  getState(): UploadState
  subscribe(listener: (state: UploadState) => void): () => void
  // start uploads the chosen files. It supersedes any in-flight upload: the
  // older request is aborted and its late result is ignored.
  start(files: File[]): Promise<void>
  choose(seriesId: string): void
  // confirmSelection is single-flight: a second call while confirming is a
  // no-op so the case can never be created twice.
  confirmSelection(): Promise<void>
  // reconcile re-reads the session after an uncertain confirmation.
  reconcile(): Promise<void>
  discard(): Promise<void>
  reset(): void
  // restore reloads the metadata of a session stored in this tab.
  restore(): Promise<void>
  dispose(): void
}

function emptyState(): UploadState {
  return {
    phase: 'idle',
    fileCount: 0,
    totalBytes: 0,
    transferredBytes: 0,
    progress: null,
    session: null,
    selectedSeriesId: null,
    caseId: null,
    error: null,
    restored: false,
  }
}

// preselect returns the only suitable series key, or null. The largest series
// is never chosen automatically.
function preselect(session: UploadSession): string | null {
  const suitable = session.series.filter((option) => option.suitable)
  return suitable.length === 1 ? suitable[0].id : null
}

function describeError(error: unknown): string {
  const code = error instanceof UploadApiError ? error.code : null
  return messageForCode(code)
}

export function createUploadWorkflow(dependencies: UploadDependencies): UploadController {
  let state = emptyState()
  const listeners = new Set<(state: UploadState) => void>()
  let generation = 0
  let active: AbortController | null = null
  let disposed = false

  const publish = (patch: Partial<UploadState>): void => {
    state = { ...state, ...patch }
    for (const listener of listeners) {
      listener(state)
    }
  }

  const current = (token: number): boolean => !disposed && token === generation

  // begin supersedes any in-flight request and returns the new generation with
  // its abort signal.
  const begin = (): { token: number; signal: AbortSignal } => {
    active?.abort()
    active = new AbortController()
    generation += 1
    return { token: generation, signal: active.signal }
  }

  // adoptSession publishes a loaded or uploaded session without opening a case.
  const adoptSession = (session: UploadSession, restored: boolean): void => {
    if (session.case_id !== null) {
      dependencies.storage.remove(UPLOAD_SESSION_KEY)
      publish({
        phase: 'opened',
        session,
        selectedSeriesId: session.selected_series_id,
        caseId: session.case_id,
        progress: null,
        error: null,
        restored,
      })
      return
    }
    publish({
      phase: 'choosing',
      session,
      selectedSeriesId: preselect(session),
      caseId: null,
      progress: null,
      transferredBytes: state.totalBytes,
      error: null,
      restored,
    })
  }

  // reset aborts any in-flight request, retires the current generation and
  // clears the stored token.
  const reset = (): void => {
    active?.abort()
    active = null
    generation += 1
    dependencies.storage.remove(UPLOAD_SESSION_KEY)
    publish(emptyState())
  }

  return {
    getState: () => state,

    subscribe: (listener) => {
      listeners.add(listener)
      return () => {
        listeners.delete(listener)
      }
    },

    async start(files) {
      if (files.length === 0) {
        return
      }
      const totalBytes = files.reduce((sum, file) => sum + file.size, 0)
      if (files.length > MAX_UPLOAD_FILES || totalBytes > MAX_UPLOAD_BYTES) {
        publish({
          ...emptyState(),
          phase: 'failed',
          fileCount: files.length,
          totalBytes,
          error: messageForCode('UPLOAD_TOO_LARGE'),
        })
        return
      }
      const { token, signal } = begin()
      publish({
        phase: 'uploading',
        fileCount: files.length,
        totalBytes,
        transferredBytes: 0,
        progress: 0,
        session: null,
        selectedSeriesId: null,
        caseId: null,
        error: null,
        restored: false,
      })
      try {
        const session = await dependencies.upload(files, {
          signal,
          onProgress: (transferred, total) => {
            if (!current(token)) {
              return
            }
            // Reaching 100% of the transfer is not completion: the server still
            // has to parse the batch and describe every series.
            const transferredAll = total > 0 && transferred >= total
            publish({
              phase: transferredAll ? 'inspecting' : 'uploading',
              transferredBytes: transferred,
              progress: total > 0 ? Math.min(1, transferred / total) : null,
            })
          },
        })
        if (!current(token)) {
          return
        }
        dependencies.storage.set(UPLOAD_SESSION_KEY, session.id)
        adoptSession(session, false)
      } catch (error) {
        if (!current(token)) {
          return
        }
        if (error instanceof UploadApiError && error.canceled) {
          publish({ phase: 'canceled', progress: null, error: null })
          return
        }
        publish({ phase: 'failed', progress: null, error: describeError(error) })
      }
    },

    choose(seriesId) {
      if (state.phase !== 'choosing' || state.session === null) {
        return
      }
      if (!state.session.series.some((option) => option.id === seriesId)) {
        return
      }
      publish({ selectedSeriesId: seriesId })
    },

    async confirmSelection() {
      const session = state.session
      const seriesId = state.selectedSeriesId
      if (state.phase !== 'choosing' || session === null || seriesId === null) {
        return
      }
      const { token, signal } = begin()
      publish({ phase: 'confirming', error: null })
      try {
        const result = await dependencies.confirm(session.id, seriesId, signal)
        if (!current(token)) {
          return
        }
        dependencies.storage.remove(UPLOAD_SESSION_KEY)
        publish({ phase: 'opened', caseId: result.case_id, error: null })
      } catch (error) {
        if (!current(token)) {
          return
        }
        if (error instanceof UploadApiError && error.canceled) {
          publish({ phase: 'choosing', error: null })
          return
        }
        if (error instanceof UploadApiError && error.uncertain) {
          // The case may already exist: keep the token and the selection and
          // offer an explicit result check instead of replaying the write.
          publish({ phase: 'uncertain', error: describeError(error) })
          return
        }
        if (error instanceof UploadApiError && (error.code === 'UPLOAD_EXPIRED' || error.code === 'UPLOAD_NOT_FOUND')) {
          dependencies.storage.remove(UPLOAD_SESSION_KEY)
          publish({ phase: 'failed', session: null, selectedSeriesId: null, error: describeError(error) })
          return
        }
        publish({ phase: 'choosing', error: describeError(error) })
      }
    },

    async reconcile() {
      const session = state.session
      if (state.phase !== 'uncertain' || session === null) {
        return
      }
      const { token, signal } = begin()
      try {
        const loaded = await dependencies.load(session.id, signal)
        if (!current(token)) {
          return
        }
        if (loaded.case_id !== null) {
          adoptSession(loaded, false)
          return
        }
        // The server has no case for this session: nothing was committed.
        publish({ phase: 'choosing', session: loaded, selectedSeriesId: state.selectedSeriesId, error: null })
      } catch (error) {
        if (!current(token)) {
          return
        }
        if (error instanceof UploadApiError && (error.code === 'UPLOAD_EXPIRED' || error.code === 'UPLOAD_NOT_FOUND')) {
          dependencies.storage.remove(UPLOAD_SESSION_KEY)
          publish({ phase: 'failed', session: null, selectedSeriesId: null, error: describeError(error) })
          return
        }
        // Still indeterminate: the user can retry the check.
        publish({ phase: 'uncertain', error: describeError(error) })
      }
    },

    async discard() {
      const session = state.session
      // The UI returns to idle immediately; the server call gets its own live
      // signal so reset/dispose can still cancel it without blocking the panel.
      active?.abort()
      generation += 1
      dependencies.storage.remove(UPLOAD_SESSION_KEY)
      const controllerForDiscard = new AbortController()
      active = controllerForDiscard
      publish(emptyState())
      if (session === null) {
        return
      }
      try {
        await dependencies.discard(session.id, controllerForDiscard.signal)
      } catch {
        // A failed discard leaves the session to expire; it never blocks the UI.
      }
    },

    reset,

    async restore() {
      const stored = dependencies.storage.get(UPLOAD_SESSION_KEY)
      if (stored === null || stored === '') {
        return
      }
      const { token, signal } = begin()
      try {
        const session = await dependencies.load(stored, signal)
        if (!current(token)) {
          return
        }
        adoptSession(session, true)
      } catch (error) {
        if (!current(token)) {
          return
        }
        if (error instanceof UploadApiError && (error.code === 'UPLOAD_EXPIRED' || error.code === 'UPLOAD_NOT_FOUND')) {
          dependencies.storage.remove(UPLOAD_SESSION_KEY)
          publish({ ...emptyState(), error: describeError(error) })
          return
        }
        dependencies.storage.remove(UPLOAD_SESSION_KEY)
        publish({ ...emptyState(), error: describeError(error) })
      }
    },

    dispose() {
      disposed = true
      active?.abort()
      active = null
      listeners.clear()
    },
  }
}
