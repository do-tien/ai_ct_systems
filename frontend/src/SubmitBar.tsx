import { useState } from 'react'

import type { WorkflowPhase } from './initialread/types.ts'

// Save/submit controls and the honest persistence status. Presentation only:
// the caller owns the workflow; this component never reports success on its own.

interface SubmitBarProps {
  phase: WorkflowPhase
  error: string | null
  dirty: boolean
  busy: boolean
  drawing: boolean
  canEdit: boolean
  onSave: () => void
  onSubmit: () => void
}

function statusText(phase: WorkflowPhase, dirty: boolean, error: string | null): { text: string; alert: boolean } {
  switch (phase) {
    case 'locked':
      return { text: 'Bản đọc đầu đã khóa; chỉnh sửa tiếp theo thuộc bản cuối.', alert: false }
    case 'saving':
      return { text: 'Đang lưu…', alert: false }
    case 'submitting':
      return { text: 'Đang nộp và khóa…', alert: false }
    case 'reconciling':
      return { text: 'Đang đối chiếu với máy chủ…', alert: false }
    case 'conflict':
      return { text: error ?? 'Bản nháp xung đột với máy chủ.', alert: true }
    case 'error':
      return { text: error ?? 'Không lưu được bản nháp.', alert: true }
    default:
      return dirty ? { text: 'Chưa lưu', alert: false } : { text: 'Đã lưu', alert: false }
  }
}

export default function SubmitBar({ phase, error, dirty, busy, drawing, canEdit, onSave, onSubmit }: SubmitBarProps) {
  const [confirming, setConfirming] = useState(false)
  const status = statusText(phase, dirty, error)
  const locked = phase === 'locked'
  const controlsDisabled = !canEdit || busy || drawing

  return (
    <div className="submit-bar">
      <p className={status.alert ? 'submit-status error' : 'submit-status'} aria-live="polite" role={status.alert ? 'alert' : undefined}>
        {drawing ? 'Đang vẽ dấu…' : status.text}
      </p>

      {!locked && !confirming && (
        <div className="submit-actions">
          <button type="button" onClick={onSave} disabled={controlsDisabled}>
            Lưu nháp
          </button>
          <button type="button" onClick={() => setConfirming(true)} disabled={controlsDisabled}>
            Nộp và khóa
          </button>
        </div>
      )}

      {!locked && confirming && (
        <div className="submit-confirm" role="alertdialog" aria-label="Xác nhận nộp bản đọc đầu">
          <p>Nộp sẽ khóa bản đọc đầu và không thể sửa lại; các chỉnh sửa sau thuộc bản cuối. Tiếp tục?</p>
          <div className="submit-actions">
            <button
              type="button"
              onClick={() => {
                setConfirming(false)
                onSubmit()
              }}
              disabled={controlsDisabled}
            >
              Xác nhận nộp và khóa
            </button>
            <button type="button" onClick={() => setConfirming(false)}>
              Hủy
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
