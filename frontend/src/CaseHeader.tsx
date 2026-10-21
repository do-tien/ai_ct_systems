// Case identity and the honest three-step progress bar. Steps are derived only
// from the real case state and the page mode; nothing here requests or claims AI
// results, so the pre-lock view reveals no AI information beyond the fact that a
// review step exists after the lock.

import type { CaseMetadata } from './case.ts'

export type CaseStage = 'independent' | 'review' | 'completed'

interface CaseHeaderProps {
  caseMetadata: CaseMetadata
  stage: CaseStage
}

const STAGE_STATUS: Record<CaseStage, { label: string; tone: string }> = {
  independent: { label: 'Chờ đọc độc lập', tone: 'info' },
  review: { label: 'Đã khóa bản đầu', tone: 'warning' },
  completed: { label: 'Đã hoàn tất', tone: 'success' },
}

const dateFormat = new Intl.DateTimeFormat('vi-VN', { dateStyle: 'medium', timeStyle: 'short' })

function formatDate(value: string | null): string | null {
  if (value === null) {
    return null
  }
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : dateFormat.format(parsed)
}

type StepState = 'done' | 'current' | 'pending'

interface Step {
  label: string
  hint: string
  state: StepState
}

function stepsFor(stage: CaseStage): Step[] {
  if (stage === 'completed') {
    return [
      { label: 'Đọc độc lập', hint: 'Đã khóa', state: 'done' },
      { label: 'Xem gợi ý AI', hint: 'Đã qua bước xem xét', state: 'done' },
      { label: 'Chốt ca', hint: 'Bản cuối đã khóa', state: 'done' },
    ]
  }
  if (stage === 'review') {
    return [
      { label: 'Đọc độc lập', hint: 'Đã khóa', state: 'done' },
      { label: 'Xem gợi ý AI', hint: 'Chấp nhận hoặc bỏ qua từng ứng viên', state: 'current' },
      { label: 'Chốt ca', hint: 'Lưu bản cuối rồi hoàn tất', state: 'pending' },
    ]
  }
  return [
    { label: 'Đọc độc lập', hint: 'Đo dấu rồi nộp để khóa', state: 'current' },
    { label: 'Xem gợi ý AI', hint: 'Mở sau khi khóa bản đầu', state: 'pending' },
    { label: 'Chốt ca', hint: 'Hoàn tất bản cuối', state: 'pending' },
  ]
}

function CheckIcon() {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false">
      <path
        d="M3 8.5 6.2 11.7 13 4.9"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

export default function CaseHeader({ caseMetadata, stage }: CaseHeaderProps) {
  const status = STAGE_STATUS[stage]
  const created = formatDate(caseMetadata.createdAt)
  const finalized = formatDate(caseMetadata.finalizedAt)
  const steps = stepsFor(stage)

  return (
    <header className="case-header">
      <div className="case-header-top">
        <a className="back-link" href="/">
          <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false">
            <path
              d="M10.5 3 5.5 8l5 5"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.8"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          Danh sách ca
        </a>
        <span className={`chip chip-${status.tone}`}>{status.label}</span>
      </div>

      <div className="case-title-row">
        <h1 className="case-title">Ca #{caseMetadata.id}</h1>
        <p className="case-meta">
          {created !== null && <span>Tạo lúc {created}</span>}
          {finalized !== null && <span>Hoàn tất lúc {finalized}</span>}
          <span className="case-uid" title={caseMetadata.seriesInstanceUID}>
            Series {caseMetadata.seriesInstanceUID}
          </span>
        </p>
      </div>

      <ol className="stepper" aria-label="Tiến trình ca">
        {steps.map((step, index) => (
          <li
            key={step.label}
            className={`step step-${step.state}`}
            aria-current={step.state === 'current' ? 'step' : undefined}
          >
            <span className="step-index">{step.state === 'done' ? <CheckIcon /> : index + 1}</span>
            <span className="step-text">
              <strong>{step.label}</strong>
              <small>{step.hint}</small>
            </span>
            <span className="visually-hidden">
              {step.state === 'done' ? 'Đã hoàn tất' : step.state === 'current' ? 'Đang thực hiện' : 'Chưa tới'}
            </span>
          </li>
        ))}
      </ol>
    </header>
  )
}
