// Page-mode selection and the lock-gated AI status session. The session makes
// at most one status request at a time and schedules the next one only after
// the previous request settles, so an old response can never revive a retired
// session or overlap a newer request. It is created only once a coherent lock
// permits disclosure; it never fetches before then.

import type { CaseMetadata } from '../case.ts'
import { ReadApiError } from '../reads/http.ts'
import type { AIProjection } from './types.ts'

// A new pending status request is scheduled this long after the preceding one
// settles, never while the preceding one is still in flight.
export const AI_POLL_INTERVAL_MS = 2000

export type ReadMode = 'independent' | 'review' | 'completed' | 'conflict'

// Backend state plus a real locked initial read decide the page mode. Hiding the
// UI is never authorization: an inconsistent combination is a conflict that
// loads no final data and starts no AI poll. The legacy `ai_revealed` value is
// treated as review when the initial read is locked.
export function selectReadMode(caseMetadata: CaseMetadata, initialLockedAt: string | null): ReadMode {
  const status: string = caseMetadata.status
  const locked = initialLockedAt !== null
  if (status === 'awaiting_read' && !locked) {
    return 'independent'
  }
  if ((status === 'initial_locked' || status === 'ai_revealed') && locked) {
    return 'review'
  }
  if (status === 'finalized' && locked) {
    return 'completed'
  }
  return 'conflict'
}

export interface AISessionState {
  projection: AIProjection | null
  pending: boolean
  error: string | null
}

export interface AISessionOptions {
  load(signal: AbortSignal): Promise<AIProjection>
  // Schedules one callback and returns a cancellation closure.
  schedule(callback: () => void, delayMs: number): () => void
  onState(state: AISessionState): void
}

export interface AISession {
  start(): void
  retry(): void
  dispose(): void
}

// The production scheduler: one owned timeout whose cancellation is returned.
export function timeoutScheduler(callback: () => void, delayMs: number): () => void {
  const id = window.setTimeout(callback, delayMs)
  return () => window.clearTimeout(id)
}

function messageFor(error: unknown): string {
  if (error instanceof ReadApiError) {
    return error.message
  }
  return 'Không tải được trạng thái AI.'
}

export function createAISession(options: AISessionOptions): AISession {
  let disposed = false
  let running = false
  let projection: AIProjection | null = null
  let error: string | null = null
  let pending = false
  let controller: AbortController | null = null
  let cancelSchedule: (() => void) | null = null
  let started = false

  const publish = (): void => {
    if (!disposed) {
      options.onState({ projection, pending, error })
    }
  }

  const scheduleNext = (): void => {
    cancelSchedule = options.schedule(() => {
      cancelSchedule = null
      void run()
    }, AI_POLL_INTERVAL_MS)
  }

  async function run(): Promise<void> {
    if (disposed || running) {
      return
    }
    running = true
    const ctrl = new AbortController()
    controller = ctrl
    pending = true
    error = null
    publish()

    let next: AIProjection
    try {
      next = await options.load(ctrl.signal)
    } catch (raw) {
      if (controller === ctrl) {
        controller = null
      }
      running = false
      if (disposed) {
        return
      }
      if (raw instanceof ReadApiError && raw.canceled) {
        return
      }
      error = messageFor(raw)
      pending = false
      publish()
      return
    }
    if (controller === ctrl) {
      controller = null
    }
    running = false
    if (disposed) {
      return
    }
    projection = next
    if (next.status === 'queued' || next.status === 'running') {
      // A pending job keeps the session pending until it becomes terminal.
      pending = true
      publish()
      scheduleNext()
      return
    }
    pending = false
    publish()
  }

  function start(): void {
    if (disposed || started) {
      return
    }
    started = true
    void run()
  }

  function retry(): void {
    if (disposed || running) {
      return
    }
    if (cancelSchedule !== null) {
      cancelSchedule()
      cancelSchedule = null
    }
    error = null
    void run()
  }

  function dispose(): void {
    disposed = true
    controller?.abort()
    controller = null
    if (cancelSchedule !== null) {
      cancelSchedule()
      cancelSchedule = null
    }
  }

  return { start, retry, dispose }
}
