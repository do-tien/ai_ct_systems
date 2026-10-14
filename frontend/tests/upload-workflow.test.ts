import test from 'node:test'
import assert from 'node:assert/strict'
import { setImmediate as tick } from 'node:timers/promises'

import { UploadApiError } from '../src/upload/api.ts'
import { MAX_UPLOAD_BYTES, MAX_UPLOAD_FILES, createUploadWorkflow, UPLOAD_SESSION_KEY } from '../src/upload/workflow.ts'
import type { UploadDependencies, UploadStorage, UploadTransferOptions } from '../src/upload/workflow.ts'
import type { UploadCaseResult, UploadSession, UploadState } from '../src/upload/types.ts'

interface MemoryStorage extends UploadStorage {
  values: Map<string, string>
}

function memoryStorage(): MemoryStorage {
  const values = new Map<string, string>()
  return {
    values,
    get: (key) => (values.has(key) ? (values.get(key) as string) : null),
    set: (key, value) => {
      values.set(key, value)
    },
    remove: (key) => {
      values.delete(key)
    },
  }
}

function session(overrides: Partial<UploadSession> = {}): UploadSession {
  return {
    id: 'token-1',
    expires_at: '2026-10-14T12:00:00Z',
    skipped_files: 0,
    series: [{ id: 'k1', index: 1, modality: 'CT', instance_count: 120, suitable: true, reason_code: null }],
    selected_series_id: null,
    case_id: null,
    ...overrides,
  }
}

function result(overrides: Partial<UploadCaseResult> = {}): UploadCaseResult {
  return { case_id: 42, instance_count: 120, existing: false, ...overrides }
}

function files(count: number, size = 10): File[] {
  return Array.from({ length: count }, (_unused, index) => ({ name: `file-${index}`, size }) as File)
}

interface Harness {
  dependencies: UploadDependencies
  storage: MemoryStorage
  calls: { upload: number; load: number; confirm: number; discard: number }
  upload: (fn: (options: UploadTransferOptions) => Promise<UploadSession>) => void
  load: (fn: () => Promise<UploadSession>) => void
  confirm: (fn: () => Promise<UploadCaseResult>) => void
  discard: (fn: () => Promise<void>) => void
}

function harness(): Harness {
  const storage = memoryStorage()
  const calls = { upload: 0, load: 0, confirm: 0, discard: 0 }
  let uploadFn: (options: UploadTransferOptions) => Promise<UploadSession> = async () => session()
  let loadFn: () => Promise<UploadSession> = async () => session()
  let confirmFn: () => Promise<UploadCaseResult> = async () => result()
  let discardFn: () => Promise<void> = async () => {}

  const dependencies: UploadDependencies = {
    storage,
    upload: (selected, options) => {
      calls.upload++
      assert.equal(selected.length > 0, true)
      return uploadFn(options)
    },
    load: () => {
      calls.load++
      return loadFn()
    },
    confirm: () => {
      calls.confirm++
      return confirmFn()
    },
    discard: () => {
      calls.discard++
      return discardFn()
    },
  }
  return {
    dependencies,
    storage,
    calls,
    upload: (fn) => {
      uploadFn = fn
    },
    load: (fn) => {
      loadFn = fn
    },
    confirm: (fn) => {
      confirmFn = fn
    },
    discard: (fn) => {
      discardFn = fn
    },
  }
}

function recorder(controller: { subscribe: (listener: (state: UploadState) => void) => () => void }): UploadState[] {
  const states: UploadState[] = []
  controller.subscribe((state) => states.push(state))
  return states
}

test('a single suitable series is preselected but never opened automatically', async () => {
  const { dependencies, storage } = harness()
  const controller = createUploadWorkflow(dependencies)
  const states = recorder(controller)

  await controller.start(files(2))
  const state = controller.getState()
  assert.equal(state.phase, 'choosing')
  assert.equal(state.selectedSeriesId, 'k1')
  assert.equal(state.caseId, null)
  assert.equal(storage.values.get(UPLOAD_SESSION_KEY), 'token-1')
  assert.equal(states.some((entry) => entry.phase === 'uploading'), true)
})

test('progress 100 percent becomes inspecting, not completion', async () => {
  const { dependencies } = harness()
  let report: UploadTransferOptions | null = null
  dependencies.upload = (_files, options) => {
    report = options
    return Promise.withResolvers<UploadSession>().promise
  }
  const controller = createUploadWorkflow(dependencies)
  const states = recorder(controller)

  void controller.start(files(1, 100))
  await tick()
  ;(report as unknown as UploadTransferOptions).onProgress(100, 100)

  assert.equal(controller.getState().phase, 'inspecting')
  assert.equal(controller.getState().progress, 1)
  assert.equal(states.at(-1)?.phase, 'inspecting')
})

test('a superseded upload never publishes its late result', async () => {
  const { dependencies, storage } = harness()
  const first = Promise.withResolvers<UploadSession>()
  const second = Promise.withResolvers<UploadSession>()
  let call = 0
  dependencies.upload = () => {
    call++
    return call === 1 ? first.promise : second.promise
  }
  const controller = createUploadWorkflow(dependencies)

  void controller.start(files(1))
  await tick()
  void controller.start(files(2))
  await tick()

  first.resolve(session({ id: 'stale-token' }))
  await tick()
  assert.equal(controller.getState().session, null)

  second.resolve(session({ id: 'fresh-token' }))
  await tick()
  assert.equal(controller.getState().session?.id, 'fresh-token')
  assert.equal(storage.values.get(UPLOAD_SESSION_KEY), 'fresh-token')
})

test('cancellation is not a failure and other errors use a safe message', async () => {
  const { dependencies } = harness()
  const controller = createUploadWorkflow(dependencies)

  dependencies.upload = () => Promise.reject(new UploadApiError('aborted', { canceled: true }))
  await controller.start(files(1))
  assert.equal(controller.getState().phase, 'canceled')
  assert.equal(controller.getState().error, null)

  dependencies.upload = () => Promise.reject(new Error('boom: upstream detail'))
  await controller.start(files(1))
  assert.equal(controller.getState().phase, 'failed')
  assert.doesNotMatch(controller.getState().error ?? '', /upstream detail/)
})

test('confirm is single flight', async () => {
  const harnessed = harness()
  const confirm = Promise.withResolvers<UploadCaseResult>()
  harnessed.confirm(() => confirm.promise)
  const controller = createUploadWorkflow(harnessed.dependencies)
  await controller.start(files(1))
  controller.choose('k1')

  void controller.confirmSelection()
  await tick()
  void controller.confirmSelection()
  await tick()

  assert.equal(harnessed.calls.confirm, 1)
  confirm.resolve(result())
  await tick()
  assert.equal(controller.getState().phase, 'opened')
  assert.equal(controller.getState().caseId, 42)
})

test('an uncertain confirm is reconciled by checking the server', async () => {
  const { dependencies, storage } = harness()
  dependencies.confirm = () => Promise.reject(new UploadApiError('unknown', { code: 'COMMIT_OUTCOME_UNKNOWN', uncertain: true }))
  const controller = createUploadWorkflow(dependencies)
  await controller.start(files(1))
  controller.choose('k1')
  await controller.confirmSelection()

  assert.equal(controller.getState().phase, 'uncertain')
  assert.equal(storage.values.get(UPLOAD_SESSION_KEY), 'token-1')

  dependencies.load = () => Promise.resolve(session({ case_id: 77, selected_series_id: 'k1' }))
  await controller.reconcile()
  assert.equal(controller.getState().phase, 'opened')
  assert.equal(controller.getState().caseId, 77)
  assert.equal(storage.values.has(UPLOAD_SESSION_KEY), false)
})

test('reconciliation without a case returns to selection', async () => {
  const { dependencies } = harness()
  dependencies.confirm = () => Promise.reject(new UploadApiError('unknown', { code: 'COMMIT_OUTCOME_UNKNOWN', uncertain: true }))
  const controller = createUploadWorkflow(dependencies)
  await controller.start(files(1))
  controller.choose('k1')
  await controller.confirmSelection()

  dependencies.load = () => Promise.resolve(session())
  await controller.reconcile()
  assert.equal(controller.getState().phase, 'choosing')
  assert.equal(controller.getState().selectedSeriesId, 'k1')
  assert.equal(controller.getState().caseId, null)
})

test('an expired session fails and clears the stored token', async () => {
  const { dependencies, storage } = harness()
  dependencies.confirm = () => Promise.reject(new UploadApiError('expired', { code: 'UPLOAD_EXPIRED', status: 409 }))
  const controller = createUploadWorkflow(dependencies)
  await controller.start(files(1))
  controller.choose('k1')
  await controller.confirmSelection()

  assert.equal(controller.getState().phase, 'failed')
  assert.equal(controller.getState().session, null)
  assert.equal(storage.values.has(UPLOAD_SESSION_KEY), false)
})

test('restore reloads a stored session and drops an expired one', async () => {
  const { dependencies, storage } = harness()
  const controller = createUploadWorkflow(dependencies)

  storage.set(UPLOAD_SESSION_KEY, 'stored-token')
  await controller.restore()
  assert.equal(controller.getState().phase, 'choosing')
  assert.equal(controller.getState().restored, true)

  storage.set(UPLOAD_SESSION_KEY, 'stale-token')
  dependencies.load = () => Promise.reject(new UploadApiError('expired', { code: 'UPLOAD_EXPIRED', status: 409 }))
  await controller.restore()
  assert.equal(controller.getState().phase, 'idle')
  assert.equal(storage.values.has(UPLOAD_SESSION_KEY), false)
})

test('an oversized selection is refused before any transfer', async () => {
  const harnessed = harness()
  const controller = createUploadWorkflow(harnessed.dependencies)

  await controller.start(files(2, MAX_UPLOAD_BYTES))
  assert.equal(controller.getState().phase, 'failed')
  assert.equal(harnessed.calls.upload, 0)
  assert.match(controller.getState().error ?? '', /giới hạn/)

  await controller.start(files(MAX_UPLOAD_FILES + 1, 1))
  assert.equal(controller.getState().phase, 'failed')
  assert.equal(harnessed.calls.upload, 0)
})

test('a disposed workflow ignores its late upload', async () => {
  const { dependencies } = harness()
  const pending = Promise.withResolvers<UploadSession>()
  dependencies.upload = () => pending.promise
  const controller = createUploadWorkflow(dependencies)
  const states = recorder(controller)

  void controller.start(files(1))
  await tick()
  const seen = states.length
  controller.dispose()
  pending.resolve(session())
  await tick()

  assert.equal(states.length, seen)
})

test('discard clears the local state and tells the server once', async () => {
  const { dependencies, calls, storage } = harness()
  const controller = createUploadWorkflow(dependencies)
  await controller.start(files(1))

  await controller.discard()
  assert.equal(calls.discard, 1)
  assert.equal(controller.getState().phase, 'idle')
  assert.equal(controller.getState().session, null)
  assert.equal(storage.values.has(UPLOAD_SESSION_KEY), false)
})
