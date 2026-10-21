import ActionConfirm from './ui/ActionConfirm.tsx'
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

type Tone = 'success' | 'warning' | 'info' | 'danger' | 'neutral'

function statusText(
  phase: FinalWorkflowState['phase'],
  dirty: boolean,
  error: string | null,
): { text: string; alert: boolean; tone: Tone } {
  switch (phase) {
    case 'completed':
      return { text: 'Ca đã hoàn tất; bản cuối chỉ đọc', alert: false, tone: 'success' }
    case 'saving':
      return { text: 'Đang lưu bản cuối…', alert: false, tone: 'info' }
    case 'finalizing':
      return { text: 'Đang hoàn tất ca…', alert: false, tone: 'info' }
    case 'reconciling':
      return { text: 'Đang đối chiếu với máy chủ…', alert: false, tone: 'info' }
    case 'conflict':
      return { text: error ?? 'Bản cuối xung đột với máy chủ.', alert: true, tone: 'danger' }
    case 'error':
      return { text: error ?? 'Không lưu được bản cuối.', alert: true, tone: 'danger' }
    default:
      return dirty
        ? { text: 'Có thay đổi chưa lưu', alert: false, tone: 'warning' }
        : { text: 'Đã lưu trên máy chủ', alert: false, tone: 'success' }
  }
}

export default function FinalizeBar({ phase, error, dirty, busy, drawing, canEdit, onSave, onFinalize }: FinalizeBarProps) {
  const status = statusText(phase, dirty, error)
  const completed = phase === 'completed'
  const controlsDisabled = !canEdit || busy || drawing

  return (
    <div className="submit-bar">
      <p className="save-status" aria-live="polite" role={status.alert ? 'alert' : undefined}>
        <span className={`chip chip-${status.tone}`}>{drawing ? 'Đang vẽ dấu…' : status.text}</span>
      </p>

      {!completed && (
        <div className="submit-actions">
          <button type="button" className="button-secondary" onClick={onSave} disabled={controlsDisabled}>
            Lưu bản cuối
          </button>
          <ActionConfirm
            triggerLabel="Hoàn tất ca"
            heading="Hoàn tất và khóa bản cuối"
            description="Bản cuối sẽ bị khóa và không sửa được nữa; thao tác này không chờ AI kết thúc. Tiếp tục?"
            confirmLabel="Xác nhận hoàn tất"
            disabled={controlsDisabled}
            onConfirm={onFinalize}
          />
        </div>
      )}
    </div>
  )
}
