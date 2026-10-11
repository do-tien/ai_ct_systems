import { useState } from 'react'

import type { FinalWorkflowState } from './finalread/types.ts'

// Save/finalize controls and the honest persistence status for the final read.
// Presentation only: the caller owns the workflow; this component never reports
// success on its own.

interface FinalizeBarProps {
  phase: FinalWorkflowState['phase']
  error: string | null
  dirty: boolean
  busy: boolean
  drawing: boolean
  canEdit: boolean
  onSave: () => void
  onFinalize: () => void
}

function statusText(phase: FinalWorkflowState['phase'], dirty: boolean, error: string | null): { text: string; alert: boolean } {
  switch (phase) {
    case 'completed':
      return { text: 'Ca đã hoàn tất; bản cuối chỉ đọc.', alert: false }
    case 'saving':
      return { text: 'Đang lưu bản cuối…', alert: false }
    case 'finalizing':
      return { text: 'Đang hoàn tất ca…', alert: false }
    case 'reconciling':
      return { text: 'Đang đối chiếu với máy chủ…', alert: false }
    case 'conflict':
      return { text: error ?? 'Bản cuối xung đột với máy chủ.', alert: true }
    case 'error':
      return { text: error ?? 'Không lưu được bản cuối.', alert: true }
    default:
      return dirty ? { text: 'Chưa lưu', alert: false } : { text: 'Đã lưu', alert: false }
  }
}

export default function FinalizeBar({ phase, error, dirty, busy, drawing, canEdit, onSave, onFinalize }: FinalizeBarProps) {
  const [confirming, setConfirming] = useState(false)
  const status = statusText(phase, dirty, error)
  const completed = phase === 'completed'
  const controlsDisabled = !canEdit || busy || drawing

  return (
    <div className="submit-bar">
      <p className={status.alert ? 'submit-status error' : 'submit-status'} aria-live="polite" role={status.alert ? 'alert' : undefined}>
        {drawing ? 'Đang vẽ dấu…' : status.text}
      </p>

      {!completed && !confirming && (
        <div className="submit-actions">
          <button type="button" onClick={onSave} disabled={controlsDisabled}>
            Lưu bản cuối
          </button>
          <button type="button" onClick={() => setConfirming(true)} disabled={controlsDisabled}>
            Hoàn tất ca
          </button>
        </div>
      )}

      {!completed && confirming && (
        <div className="submit-confirm" role="alertdialog" aria-label="Xác nhận hoàn tất ca">
          <p>Hoàn tất ca sẽ khóa bản cuối và không sửa được nữa; thao tác không chờ AI kết thúc. Tiếp tục?</p>
          <div className="submit-actions">
            <button
              type="button"
              onClick={() => {
                setConfirming(false)
                onFinalize()
              }}
              disabled={controlsDisabled}
            >
              Xác nhận hoàn tất
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
