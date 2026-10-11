import { useState } from 'react'

import type { AISessionState } from './finalread/aiSession.ts'
import type { Candidate, CandidateDecision } from './finalread/types.ts'

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
      <h2 id="ai-heading">Gợi ý AI (sau khóa bản đọc đầu)</h2>
      <p className="notice">{RESEARCH_WARNING}</p>

      {state.error !== null && (
        <p className="error" role="alert">
          Không tải được trạng thái AI. {state.error}
        </p>
      )}
      {state.error !== null && (
        <button type="button" onClick={onRetry}>
          Thử lại
        </button>
      )}

      {projection === null && state.error === null && <p>Đang tải trạng thái AI…</p>}

      {projection !== null && projection.status === 'queued' && <p>AI đang xếp hàng xử lý…</p>}
      {projection !== null && projection.status === 'running' && <p>AI đang chạy…</p>}

      {projection !== null && projection.status === 'failed' && (
        <p role="alert">AI không khả dụng; vẫn có thể hoàn tất ca. ({projection.error.code})</p>
      )}

      {projection !== null && projection.status === 'succeeded' && (
        <>
          <p className="ai-provenance">
            Model {projection.model.bundle} v{projection.model.bundle_version} — ngưỡng điểm detector{' '}
            {projection.model.score_threshold} — thiết bị {projection.resources.device}
          </p>
          {projection.candidates.length === 0 ? (
            <p>AI không phát hiện ứng viên nào trong ca này (kết quả rỗng hợp lệ; không chứng minh là không có nốt).</p>
          ) : (
            <ol className="candidate-list">
              {projection.candidates.map((candidate, index) => {
                const decision = byId.get(candidate.id)
                const linked = linkedCandidates.has(candidate.id)
                return (
                  <li key={candidate.id} className="candidate-row">
                    <div className="candidate-summary">
                      <span className="candidate-index">Ứng viên {index + 1}</span>
                      <span className="candidate-score">Điểm detector {candidate.score.toFixed(2)}</span>
                      <span className="candidate-size">Kích thước hộp detector (mm) {candidateSize(candidate.size_mm)}</span>
                      {decisionText(decision) !== null && <span className="candidate-decision">{decisionText(decision)}</span>}
                    </div>

                    <div className="candidate-actions">
                      <button type="button" onClick={() => onFocus(candidate.id)}>
                        Xem lát
                      </button>
                      {canDecide && !linked && (
                        <button type="button" onClick={() => onAccept(candidate.id)}>
                          Thêm thành dấu nốt
                        </button>
                      )}
                      {canDecide && confirmingId !== candidate.id && (
                        <button type="button" onClick={() => startDismiss(candidate.id)}>
                          Bỏ qua…
                        </button>
                      )}
                    </div>

                    {canDecide && confirmingId === candidate.id && (
                      <div className="candidate-dismiss" role="alertdialog" aria-label="Xác nhận bỏ qua ứng viên">
                        <label>
                          Lý do bỏ qua
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
                            onClick={() => confirmDismiss(candidate.id)}
                            disabled={reason.trim().length === 0}
                          >
                            Xác nhận bỏ qua
                          </button>
                          <button type="button" onClick={() => setConfirmingId(null)}>
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
