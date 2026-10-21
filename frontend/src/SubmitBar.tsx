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

type Tone = 'success' | 'warning' | 'info' | 'danger' | 'neutral'

function statusText(
  phase: WorkflowPhase,
  dirty: boolean,
  error: string | null,
): { text: string; alert: boolean; tone: Tone } {
  switch (phase) {
    case 'locked':
      return { text: 'Đã khóa bản đọc đầu', alert: false, tone: 'neutral' }
    case 'saving':
      return { text: 'Đang lưu…', alert: false, tone: 'info' }
    case 'submitting':
      return { text: 'Đang nộp và khóa…', alert: false, tone: 'info' }
    case 'reconciling':
      return { text: 'Đang đối chiếu với máy chủ…', alert: false, tone: 'info' }
    case 'conflict':
      return { text: error ?? 'Bản nháp xung đột với máy chủ.', alert: true, tone: 'danger' }
    case 'error':
      return { text: error ?? 'Không lưu được bản nháp.', alert: true, tone: 'danger' }
    default:
      return dirty
        ? { text: 'Có thay đổi chưa lưu', alert: false, tone: 'warning' }
        : { text: 'Đã lưu trên máy chủ', alert: false, tone: 'success' }
  }
}

export default function SubmitBar({ phase, error, dirty, busy, drawing, canEdit, onSave, onSubmit }: SubmitBarProps) {
  const [confirming, setConfirming] = useState(false)
  const status = statusText(phase, dirty, error)
  const locked = phase === 'locked'
  const controlsDisabled = !canEdit || busy || drawing

  return (
    <div className="submit-bar">
      <p className="save-status" aria-live="polite" role={status.alert ? 'alert' : undefined}>
        <span className={`chip chip-${status.tone}`}>{drawing ? 'Đang vẽ dấu…' : status.text}</span>
      </p>

      {!locked && !confirming && (
        <div className="submit-actions">
          <button type="button" className="button-secondary" onClick={onSave} disabled={controlsDisabled}>
            Lưu nháp
          </button>
          <button type="button" className="button-primary" onClick={() => setConfirming(true)} disabled={controlsDisabled}>
            Nộp và khóa
          </button>
        </div>
      )}

      {!locked && confirming && (
        <div className="confirm-card" role="alertdialog" aria-label="Xác nhận nộp bản đọc đầu">
          <p>Nộp sẽ khóa bản đọc đầu và không thể sửa lại; các chỉnh sửa sau thuộc bản cuối. Tiếp tục?</p>
          <div className="submit-actions">
            <button
              type="button"
              className="button-primary"
              onClick={() => {
                setConfirming(false)
                onSubmit()
              }}
              disabled={controlsDisabled}
            >
              Xác nhận nộp và khóa
            </button>
            <button type="button" className="button-quiet" onClick={() => setConfirming(false)}>
              Hủy
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
