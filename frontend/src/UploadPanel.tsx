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
  const [dragging, setDragging] = useState(false)
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
  const hasSession = state.session !== null
  const selecting = selected.length > 0 || busy

  const pickers = (
    <div className="upload-picker">
      <label className={busy ? 'button-secondary file-button disabled' : 'button-secondary file-button'}>
        Chọn file
        <input
          type="file"
          multiple
          className="visually-hidden"
          disabled={busy}
          onChange={(event) => onFilesChosen(event.target.files)}
        />
      </label>
      <label className={busy ? 'button-quiet file-button disabled' : 'button-quiet file-button'}>
        Chọn thư mục
        <input
          type="file"
          multiple
          className="visually-hidden"
          disabled={busy}
          onChange={(event) => onFilesChosen(event.target.files)}
          {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
        />
      </label>
    </div>
  )

  return (
    <section className="panel upload-panel" aria-labelledby="upload-heading">
      <div className="panel-heading">
        <div>
          <h2 id="upload-heading">Nhập ca CT</h2>
          <p className="panel-sub">Dữ liệu DICOM của một series CT; ca chỉ được tạo sau khi bạn xác nhận series.</p>
        </div>
      </div>

      {!hasSession && !selecting && (
        <div
          className={dragging ? 'dropzone dropzone-active' : 'dropzone'}
          onDragOver={(event) => {
            if (busy) {
              return
            }
            event.preventDefault()
            setDragging(true)
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={(event) => {
            event.preventDefault()
            setDragging(false)
            if (!busy) {
              onFilesChosen(event.dataTransfer.files)
            }
          }}
        >
          <p className="dropzone-title">Kéo thả file DICOM hoặc archive vào đây</p>
          <p className="dropzone-hint">hoặc chọn từ máy</p>
          {pickers}
        </div>
      )}

      {!hasSession && selecting && (
        <div className={dragging ? 'upload-selection dropzone-active' : 'upload-selection'}>
          <p className="upload-selection-title">
            {selected.length === 0
              ? 'Đang chuẩn bị dữ liệu đã chọn…'
              : `Đã chọn ${selected.length} file (${formatBytes(selectedBytes)}).`}
          </p>
          {pickers}
        </div>
      )}

      {!hasSession && (
        <div className="upload-actions">
          {busy ? (
            <button type="button" className="button-secondary" onClick={() => controllerRef.current?.reset()}>
              Hủy tải lên
            </button>
          ) : (
            <button
              type="button"
              className="button-primary"
              onClick={startUpload}
              disabled={selected.length === 0}
            >
              Tải lên và kiểm tra
            </button>
          )}
        </div>
      )}

      <p className="upload-phase" role="status">
        {state.phase === 'idle' && selected.length > 0
          ? 'Sẵn sàng tải lên khi bạn bấm “Tải lên và kiểm tra”.'
          : PHASE_LABELS[state.phase]}
      </p>

      {state.progress !== null && (
        <div
          className="upload-progress"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={progressPercent}
          aria-label="Tiến độ tải lên"
        >
          <div className="upload-progress-bar" style={{ width: `${progressPercent}%` }} />
        </div>
      )}

      {state.session !== null && (
        <>
          <p className="upload-summary">
            {state.session.series.length} series trong lượt tải lên
            {state.session.skipped_files > 0 ? `, bỏ qua ${state.session.skipped_files} mục chỉ mục/phụ` : ''}
            {state.restored ? ' (khôi phục sau khi tải lại trang)' : ''}.
          </p>
          <fieldset className="upload-series-set">
            <legend className="visually-hidden">Chọn series để mở ca</legend>
            <ul className="upload-series">
              {state.session.series.map((option) => {
                const disabled = !option.suitable || busy
                const selectedOption = state.selectedSeriesId === option.id
                return (
                  <li
                    key={option.id}
                    className={[
                      'series-option',
                      selectedOption ? 'series-option-selected' : '',
                      option.suitable ? '' : 'series-option-disabled',
                    ]
                      .filter(Boolean)
                      .join(' ')}
                  >
                    <label>
                      <input
                        type="radio"
                        name="upload-series"
                        value={option.id}
                        checked={selectedOption}
                        disabled={disabled}
                        onChange={() => controllerRef.current?.choose(option.id)}
                      />
                      <span>
                        <span className="series-option-title">
                          Series {option.index} · {option.modality || 'không rõ modality'} ·{' '}
                          {option.instance_count} lát cắt
                        </span>
                        {!option.suitable && (
                          <span className="upload-reason">
                            {option.reason_code === null
                              ? 'Series không phù hợp.'
                              : (REASON_LABELS[option.reason_code] ?? 'Series không phù hợp.')}
                          </span>
                        )}
                      </span>
                    </label>
                  </li>
                )
              })}
            </ul>
          </fieldset>
        </>
      )}

      {state.phase === 'choosing' && (
        <div className="upload-actions">
          <button
            type="button"
            className="button-primary"
            onClick={() => void controllerRef.current?.confirmSelection()}
            disabled={state.selectedSeriesId === null}
          >
            Tạo ca từ series đã chọn
          </button>
        </div>
      )}

      {state.phase === 'confirming' && <p className="upload-summary">Đang tạo ca từ series đã chọn…</p>}

      {state.phase === 'uncertain' && (
        <div className="upload-actions">
          <button type="button" className="button-secondary" onClick={() => void controllerRef.current?.reconcile()}>
            Kiểm tra kết quả
          </button>
        </div>
      )}

      {state.session !== null && !busy && (
        <div className="upload-actions">
          <button type="button" className="button-quiet" onClick={() => void controllerRef.current?.discard()}>
            Bỏ lượt tải lên này
          </button>
        </div>
      )}

      {state.error !== null && (
        <p className="error" role="alert">
          {state.error}
        </p>
      )}

      <details className="format-note">
        <summary>Định dạng hỗ trợ</summary>
        <p>
          File DICOM <code>.dcm</code>, file không đuôi, thư mục có thư mục con và archive ZIP/TAR/TAR.GZ.
          DICOMDIR, <code>.DS_Store</code>, <code>Thumbs.db</code> và <code>__MACOSX</code> được bỏ qua. Không hỗ
          trợ NIfTI, NRRD, MetaImage, ảnh thường, RAR/7z, archive lồng nhau hoặc tải từ URL.
        </p>
      </details>
    </section>
  )
}
