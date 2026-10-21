// Home dashboard: the upload surface plus the authenticated reader's own cases.
// The list is the only way back into a case without typing a URL, so it fails
// closed (explicit error, no stale rows) and never requests AI data.

import { useCallback, useEffect, useRef, useState } from 'react'

import UploadPanel from './UploadPanel.tsx'
import { loadCases } from './cases.ts'
import type { CaseMetadata, CaseState } from './case.ts'
import type { CaseError } from './case.ts'

const LIST_ERROR_MESSAGE = 'Không tải được danh sách ca. Kiểm tra kết nối rồi thử lại.'

const STATUS_LABELS: Record<CaseState, string> = {
  awaiting_read: 'Chờ đọc độc lập',
  initial_locked: 'Đã khóa bản đầu',
  finalized: 'Đã hoàn tất',
}

const STATUS_TONES: Record<CaseState, string> = {
  awaiting_read: 'info',
  initial_locked: 'warning',
  finalized: 'success',
}

const dateFormat = new Intl.DateTimeFormat('vi-VN', { dateStyle: 'short', timeStyle: 'short' })

function formatCreatedAt(value: string): string {
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : dateFormat.format(parsed)
}

export default function Dashboard({ onOpenCase }: { onOpenCase: (caseId: number) => void }) {
  const [cases, setCases] = useState<CaseMetadata[]>([])
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(true)

  const mountedRef = useRef(true)
  const requestRef = useRef<AbortController | null>(null)

  const run = useCallback(async () => {
    requestRef.current?.abort()
    const controller = new AbortController()
    requestRef.current = controller
    setPending(true)
    setError(null)
    try {
      const next = await loadCases(controller.signal)
      if (!mountedRef.current || controller.signal.aborted) {
        return
      }
      setCases(next)
    } catch (failure) {
      if (!mountedRef.current || controller.signal.aborted) {
        return
      }
      setCases([])
      const status = (failure as CaseError).status
      setError(status === 401 ? 'Phiên làm việc đã hết hạn. Đăng nhập lại để xem ca.' : LIST_ERROR_MESSAGE)
    } finally {
      if (mountedRef.current && requestRef.current === controller) {
        setPending(false)
      }
    }
  }, [])

  useEffect(() => {
    mountedRef.current = true
    void run()
    return () => {
      mountedRef.current = false
      requestRef.current?.abort()
    }
  }, [run])

  return (
    <div className="dashboard">
      <div className="page-intro">
        <div>
          <h1>Ca đọc của tôi</h1>
          <p className="page-intro-text">
            Mở ca đã có hoặc nhập ca mới. Bác sĩ đọc độc lập trước, xem gợi ý sau và giữ quyết định
            cuối cùng.
          </p>
        </div>
        <p className="panel-sub">{pending ? 'Đang tải danh sách…' : `${cases.length} ca trong danh sách`}</p>
      </div>

      <div className="dashboard-grid">
        <section className="panel case-list-panel" aria-labelledby="case-list-heading" aria-busy={pending}>
          <div className="case-toolbar">
            <div>
              <h2 id="case-list-heading">Ca của tôi</h2>
              <p className="panel-sub">
                {pending ? 'Đang tải danh sách…' : `${cases.length} ca do bạn nhập hoặc đọc.`}
              </p>
            </div>
            <button type="button" className="button-quiet" onClick={() => void run()} disabled={pending}>
              Làm mới
            </button>
          </div>

          {error !== null && (
            <p className="error" role="alert">
              {error}
            </p>
          )}

          {error === null && pending && cases.length === 0 && (
            <div className="skeleton-list" aria-hidden="true">
              <span className="skeleton-row" />
              <span className="skeleton-row" />
            </div>
          )}

          {error === null && !pending && cases.length === 0 && (
            <div className="empty-state">
              <p>Chưa có ca nào.</p>
              <p className="panel-sub">Nhập dữ liệu DICOM của một ca ở khung bên cạnh để bắt đầu.</p>
            </div>
          )}

          {cases.length > 0 && (
            <ul className="case-list">
              {cases.map((entry) => (
                <li key={entry.id}>
                  <a className="case-card" href={`/?case=${entry.id}`}>
                    <span className="case-card-main">
                      <span className="case-card-title">Ca #{entry.id}</span>
                      <span className="case-card-meta">
                        {formatCreatedAt(entry.createdAt)} · {entry.source}
                      </span>
                    </span>
                    <span className="case-card-status">
                      <span className={`chip chip-${STATUS_TONES[entry.status]}`}>
                        {STATUS_LABELS[entry.status]}
                      </span>
                    </span>
                    <span className="case-card-open">
                      Mở ca
                      <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false">
                        <path
                          d="M6 3.5 10.5 8 6 12.5"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="1.8"
                          strokeLinecap="round"
                          strokeLinejoin="round"
                        />
                      </svg>
                    </span>
                  </a>
                </li>
              ))}
            </ul>
          )}
        </section>

        <UploadPanel onOpenCase={onOpenCase} />
      </div>

      <section className="panel onboarding" aria-labelledby="onboarding-heading">
        <h2 id="onboarding-heading">Quy trình một ca</h2>
        <ol className="onboarding-steps">
          <li>
            <span className="step-index">1</span>
            <span>
              <strong>Nhập ca</strong>
              <small>Tải DICOM của một series CT, chọn series phù hợp và mở ca.</small>
            </span>
          </li>
          <li>
            <span className="step-index">2</span>
            <span>
              <strong>Đọc độc lập</strong>
              <small>Đo và lưu các dấu nốt khi chưa thấy thông tin AI, rồi nộp để khóa.</small>
            </span>
          </li>
          <li>
            <span className="step-index">3</span>
            <span>
              <strong>Xem gợi ý &amp; chốt ca</strong>
              <small>Chấp nhận hoặc bỏ qua từng ứng viên, rồi hoàn tất bản cuối.</small>
            </span>
          </li>
        </ol>
      </section>
    </div>
  )
}
