// Primary upload surface: pick DICOM files, a folder or an archive, watch real
// byte progress, then choose one suitable series and open the case. AI data is
// never requested or shown here.

import { useCallback, useEffect, useRef, useState } from 'react'

import { confirmUploadSeries, discardUploadSession, loadUploadSession, uploadFiles } from './upload/api.ts'
import type { UploadState } from './upload/types.ts'
import { createUploadWorkflow } from './upload/workflow.ts'
import type { UploadController } from './upload/workflow.ts'

interface UploadPanelProps {
  onOpenCase: (caseId: number) => void
}

// Fixed, non-identifying labels for the suitability reason codes.
const REASON_LABELS: Record<string, string> = {
  SINGLE_SLICE: 'Series chỉ có một lát cắt nên không mở được ca.',
  MULTIFRAME: 'CT đa khung (multiframe/enhanced) không được hỗ trợ.',
  UNSUPPORTED_SERIES: 'Hình học series không phù hợp với quy trình đọc.',
}

const PHASE_LABELS: Record<UploadState['phase'], string> = {
  idle: 'Chọn dữ liệu DICOM của một ca.',
  uploading: 'Đang tải dữ liệu lên máy chủ…',
  inspecting: 'Đang kiểm tra DICOM và liệt kê series…',
  choosing: 'Chọn một series rồi mở ca.',
  confirming: 'Đang tạo ca…',
  opened: 'Đã tạo ca. Đang mở trình xem…',
  uncertain: 'Chưa xác định được kết quả tạo ca. Hãy kiểm tra lại kết quả.',
  canceled: 'Đã hủy thao tác tải lên.',
  failed: 'Không hoàn tất được thao tác tải lên.',
}

const EMPTY_STATE: UploadState = {
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

function formatBytes(value: number): string {
  if (value < 1024) {
    return `${value} B`
  }
  const units = ['KiB', 'MiB', 'GiB']
  let size = value / 1024
  let unit = 0
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024
    unit += 1
  }
  return `${size.toFixed(1)} ${units[unit]}`
}

export default function UploadPanel({ onOpenCase }: UploadPanelProps) {
  const [state, setState] = useState<UploadState>(EMPTY_STATE)
  const [selected, setSelected] = useState<File[]>([])
  const controllerRef = useRef<UploadController | null>(null)
  const openedRef = useRef(false)

  useEffect(() => {
    const controller = createUploadWorkflow({
      upload: (files, options) => uploadFiles({ files, ...options }),
      load: (sessionId, signal) => loadUploadSession(sessionId, signal),
      confirm: (sessionId, seriesId, signal) => confirmUploadSeries(sessionId, seriesId, signal),
      discard: (sessionId, signal) => discardUploadSession(sessionId, signal),
      storage: {
        get: (key) => window.sessionStorage.getItem(key),
        set: (key, value) => window.sessionStorage.setItem(key, value),
        remove: (key) => window.sessionStorage.removeItem(key),
      },
    })
    controllerRef.current = controller
    setState(controller.getState())
    const unsubscribe = controller.subscribe(setState)
    void controller.restore()
    return () => {
      unsubscribe()
      controller.dispose()
      controllerRef.current = null
    }
  }, [])

  useEffect(() => {
    if (state.phase === 'opened' && state.caseId !== null && !openedRef.current) {
      openedRef.current = true
      onOpenCase(state.caseId)
    }
  }, [state.phase, state.caseId, onOpenCase])

  const busy = state.phase === 'uploading' || state.phase === 'inspecting' || state.phase === 'confirming'
  const progressPercent = state.progress === null ? 0 : Math.round(state.progress * 100)

  const onFilesChosen = useCallback((list: FileList | null) => {
    setSelected(list === null ? [] : Array.from(list))
  }, [])

  const startUpload = useCallback(() => {
    if (selected.length === 0) {
      return
    }
    void controllerRef.current?.start(selected)
  }, [selected])

  const selectedBytes = selected.reduce((sum, file) => sum + file.size, 0)

  return (
    <section className="panel upload-panel" aria-labelledby="upload-heading">
      <h2 id="upload-heading">Nhập ca CT từ dữ liệu DICOM</h2>
      <p className="notice">
        Hỗ trợ file DICOM <code>.dcm</code>, file không đuôi, thư mục có thư mục con và archive
        ZIP/TAR/TAR.GZ. DICOMDIR, <code>.DS_Store</code>, <code>Thumbs.db</code> và{' '}
        <code>__MACOSX</code> được bỏ qua. Không hỗ trợ NIfTI, NRRD, MetaImage, ảnh thường, RAR/7z,
        archive lồng nhau hoặc tải từ URL.
      </p>

      <div className="upload-picker">
        <label>
          Chọn file
          <input
            type="file"
            multiple
            disabled={busy}
            onChange={(event) => onFilesChosen(event.target.files)}
          />
        </label>
        <label>
          Chọn thư mục
          <input
            type="file"
            multiple
            disabled={busy}
            onChange={(event) => onFilesChosen(event.target.files)}
            {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
          />
        </label>
      </div>

      <p className="upload-summary">
        {selected.length === 0
          ? 'Chưa chọn file nào.'
          : `Đã chọn ${selected.length} file (${formatBytes(selectedBytes)}).`}
      </p>

      <div className="upload-actions">
        <button type="button" onClick={startUpload} disabled={busy || selected.length === 0}>
          Tải lên và kiểm tra
        </button>
        {busy && (
          <button type="button" onClick={() => controllerRef.current?.reset()} disabled={state.phase === 'confirming'}>
            Hủy
          </button>
        )}
      </div>

      <p className="upload-phase" role="status">
        {PHASE_LABELS[state.phase]}
      </p>

      {state.progress !== null && (
        <div className="upload-progress" aria-hidden="true">
          <div className="upload-progress-bar" style={{ width: `${progressPercent}%` }} />
        </div>
      )}

      {state.session !== null && (
        <>
          <p className="upload-summary">
            {state.session.series.length} series trong lượt tải lên
            {state.session.skipped_files > 0 ? `, bỏ qua ${state.session.skipped_files} mục chỉ mục/phụ` : ''}
            {state.restored ? ' (khôi phục từ tab này sau khi tải lại trang)' : ''}.
          </p>
          <ul className="upload-series">
            {state.session.series.map((option) => {
              const disabled = !option.suitable || busy
              return (
                <li key={option.id}>
                  <label>
                    <input
                      type="radio"
                      name="upload-series"
                      value={option.id}
                      checked={state.selectedSeriesId === option.id}
                      disabled={disabled}
                      onChange={() => controllerRef.current?.choose(option.id)}
                    />
                    <span>
                      Series {option.index} — {option.modality || 'không rõ modality'} —{' '}
                      {option.instance_count} lát cắt
                    </span>
                  </label>
                  {!option.suitable && (
                    <span className="upload-reason">
                      {option.reason_code === null
                        ? 'Series không phù hợp.'
                        : (REASON_LABELS[option.reason_code] ?? 'Series không phù hợp.')}
                    </span>
                  )}
                </li>
              )
            })}
          </ul>
        </>
      )}

      {state.phase === 'choosing' && (
        <button
          type="button"
          onClick={() => void controllerRef.current?.confirmSelection()}
          disabled={state.selectedSeriesId === null}
        >
          Mở ca
        </button>
      )}

      {state.phase === 'confirming' && <p className="upload-summary">Đang tạo ca từ series đã chọn…</p>}

      {state.phase === 'uncertain' && (
        <button type="button" onClick={() => void controllerRef.current?.reconcile()}>
          Kiểm tra kết quả
        </button>
      )}

      {state.session !== null && !busy && (
        <button type="button" onClick={() => void controllerRef.current?.discard()}>
          Bỏ lượt tải lên này
        </button>
      )}

      {state.error !== null && (
        <p className="error" role="alert">
          {state.error}
        </p>
      )}
    </section>
  )
}
