import ActionConfirm from './ui/ActionConfirm.tsx'
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
  const status = statusText(phase, dirty, error)
  const locked = phase === 'locked'
  const controlsDisabled = !canEdit || busy || drawing

  return (
    <div className="submit-bar">
      <p className="save-status" aria-live="polite" role={status.alert ? 'alert' : undefined}>
        <span className={`chip chip-${status.tone}`}>{drawing ? 'Đang vẽ dấu…' : status.text}</span>
      </p>

      {!locked && (
        <div className="submit-actions">
          <button type="button" className="button-secondary" onClick={onSave} disabled={controlsDisabled}>
            Lưu nháp
          </button>
          <ActionConfirm
            triggerLabel="Nộp và khóa"
            heading="Nộp và khóa bản đọc đầu"
            description="Bản đọc đầu sẽ bị khóa và không sửa được nữa; các chỉnh sửa sau thuộc bản cuối. Tiếp tục?"
            confirmLabel="Xác nhận nộp và khóa"
            disabled={controlsDisabled}
            onConfirm={onSubmit}
          />
        </div>
      )}
    </div>
  )
}
