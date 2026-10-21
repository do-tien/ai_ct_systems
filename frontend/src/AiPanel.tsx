import { useState } from 'react'

import type { AISessionState } from './finalread/aiSession.ts'
import type { CandidateDecision } from './finalread/types.ts'

// AI status and explicit candidate decisions for the post-lock review. Pure
// presentation: it never derives a measurement from a candidate and never
// decides on the physician's behalf. The research warning is always shown.

interface AiPanelProps {
  state: AISessionState
  decisions: readonly CandidateDecision[]
  linkedCandidates: ReadonlySet<string>
  canDecide: boolean
  onFocus: (candidateId: string) => void
  onAccept: (candidateId: string) => void
  onDismiss: (candidateId: string, reason: string) => void
  onRetry: () => void
}

const RESEARCH_WARNING =
  'MONAI là mô hình nghiên cứu, không dùng để chẩn đoán. Gợi ý AI không thay thế quyết định của bác sĩ.'

function decisionText(decision: CandidateDecision | undefined): string | null {
  if (decision === undefined) {
    return null
  }
  if (decision.decision === 'accepted') {
    return 'Đã chấp nhận'
  }
  return `Đã bỏ qua: ${decision.dismiss_reason ?? ''}`
}

function candidateSize(size: readonly number[]): string {
  return `${size.map((value) => value.toFixed(1)).join(' × ')} mm`
}

function statusChip(state: AISessionState): { label: string; tone: string } {
  if (state.error !== null) {
    return { label: 'Không tải được trạng thái', tone: 'danger' }
  }
  const projection = state.projection
  if (projection === null) {
    return { label: 'Đang tải trạng thái', tone: 'info' }
  }
  switch (projection.status) {
    case 'queued':
      return { label: 'Đang xếp hàng', tone: 'info' }
    case 'running':
      return { label: 'Đang chạy', tone: 'info' }
    case 'failed':
      return { label: 'Không khả dụng', tone: 'warning' }
    default:
      return { label: 'Đã có kết quả', tone: 'success' }
  }
}

export default function AiPanel({
  state,
  decisions,
  linkedCandidates,
  canDecide,
  onFocus,
  onAccept,
  onDismiss,
  onRetry,
}: AiPanelProps) {
  const [confirmingId, setConfirmingId] = useState<string | null>(null)
  const [reason, setReason] = useState('')

  const projection = state.projection
  const byId = new Map(decisions.map((decision) => [decision.candidate_id, decision]))
  const chip = statusChip(state)

  const startDismiss = (candidateId: string): void => {
    setConfirmingId(candidateId)
    setReason('')
  }

  const confirmDismiss = (candidateId: string): void => {
    if (reason.trim().length === 0) {
      return
    }
    onDismiss(candidateId, reason)
    setConfirmingId(null)
    setReason('')
  }

  return (
    <section className="panel ai-panel" aria-labelledby="ai-heading">
      <div className="panel-heading">
        <div>
          <h2 id="ai-heading">Gợi ý AI</h2>
          <p className="panel-sub">Chỉ hiển thị sau khi bản đọc đầu đã khóa.</p>
        </div>
        <span className={`chip chip-${chip.tone}`}>{chip.label}</span>
      </div>

      <p className="notice notice-small">{RESEARCH_WARNING}</p>

      {state.error !== null && (
        <>
          <p className="error" role="alert">
            Không tải được trạng thái AI. {state.error}
          </p>
          <button type="button" className="button-secondary" onClick={onRetry}>
            Thử lại
          </button>
        </>
      )}

      {projection === null && state.error === null && <p className="panel-sub">Đang tải trạng thái AI…</p>}

      {projection !== null && projection.status === 'queued' && (
        <p className="panel-sub">AI đang xếp hàng xử lý…</p>
      )}
      {projection !== null && projection.status === 'running' && <p className="panel-sub">AI đang chạy…</p>}

      {projection !== null && projection.status === 'failed' && (
        <p className="status-line status-line-warning" role="alert">
          AI không khả dụng; vẫn có thể hoàn tất ca. Mã: {projection.error.code}
        </p>
      )}

      {projection !== null && projection.status === 'succeeded' && (
        <>
          <p className="ai-provenance">
            Model {projection.model.bundle} v{projection.model.bundle_version} · ngưỡng điểm detector{' '}
            {projection.model.score_threshold} · thiết bị {projection.resources.device}
          </p>
          {projection.candidates.length === 0 ? (
            <p className="panel-sub">
              AI không phát hiện ứng viên nào trong ca này. Kết quả rỗng hợp lệ; không chứng minh là không có nốt.
            </p>
          ) : (
            <ol className="candidate-list" aria-label="Ứng viên do AI đề xuất">
              {projection.candidates.map((candidate, index) => {
                const decision = byId.get(candidate.id)
                const linked = linkedCandidates.has(candidate.id)
                return (
                  <li key={candidate.id} className="candidate-row">
                    <div className="candidate-summary">
                      <span className="candidate-index">Ứng viên {index + 1}</span>
                      <span className="candidate-score">Điểm {candidate.score.toFixed(2)}</span>
                      <span className="candidate-size">{candidateSize(candidate.size_mm)}</span>
                      {decisionText(decision) !== null && (
                        <span className={`chip chip-${decision?.decision === 'accepted' ? 'success' : 'neutral'}`}>
                          {decisionText(decision)}
                        </span>
                      )}
                    </div>

                    <div className="candidate-actions">
                      <button type="button" className="button-secondary" onClick={() => onFocus(candidate.id)}>
                        Xem lát
                      </button>
                      {canDecide && !linked && (
                        <button type="button" className="button-primary" onClick={() => onAccept(candidate.id)}>
                          Thêm thành dấu nốt
                        </button>
                      )}
                      {canDecide && confirmingId !== candidate.id && (
                        <button type="button" className="button-quiet" onClick={() => startDismiss(candidate.id)}>
                          Bỏ qua…
                        </button>
                      )}
                    </div>

                    {canDecide && confirmingId === candidate.id && (
                      <div className="confirm-card" role="alertdialog" aria-label="Xác nhận bỏ qua ứng viên">
                        <label className="field">
                          <span>Lý do bỏ qua</span>
                          <input
                            type="text"
                            value={reason}
                            onChange={(event) => setReason(event.target.value)}
                            aria-label="Lý do bỏ qua"
                          />
                        </label>
                        <div className="candidate-actions">
                          <button
                            type="button"
                            className="button-primary"
                            onClick={() => confirmDismiss(candidate.id)}
                            disabled={reason.trim().length === 0}
                          >
                            Xác nhận bỏ qua
                          </button>
                          <button type="button" className="button-quiet" onClick={() => setConfirmingId(null)}>
                            Hủy
                          </button>
                        </div>
                      </div>
                    )}
                  </li>
                )
              })}
            </ol>
          )}
        </>
      )}
    </section>
  )
}
