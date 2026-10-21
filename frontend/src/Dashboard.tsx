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
      <section className="panel hero" aria-labelledby="hero-heading">
        <p className="eyebrow">Quy trình một ca</p>
        <h2 id="hero-heading">Đọc độc lập trước, xem gợi ý sau, bác sĩ quyết định cuối cùng</h2>
        <ol className="hero-steps">
          <li>
            <span className="hero-step-index">1</span>
            <span>
              <strong>Nhập ca</strong>
              <small>Tải DICOM của một series CT, chọn series phù hợp và mở ca.</small>
            </span>
          </li>
          <li>
            <span className="hero-step-index">2</span>
            <span>
              <strong>Đọc độc lập</strong>
              <small>Đo và lưu các dấu nốt khi chưa thấy bất kỳ thông tin AI nào, rồi nộp để khóa.</small>
            </span>
          </li>
          <li>
            <span className="hero-step-index">3</span>
            <span>
              <strong>Xem gợi ý &amp; chốt ca</strong>
              <small>Chấp nhận hoặc bỏ qua từng ứng viên, rồi hoàn tất bản cuối.</small>
            </span>
          </li>
        </ol>
      </section>

      <div className="dashboard-grid">
        <UploadPanel onOpenCase={onOpenCase} />

        <section className="panel case-list-panel" aria-labelledby="case-list-heading" aria-busy={pending}>
          <div className="panel-heading">
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
              <p className="panel-sub">Tải dữ liệu DICOM của một ca ở khung bên trái để bắt đầu.</p>
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
                    <span className={`chip chip-${STATUS_TONES[entry.status]}`}>{STATUS_LABELS[entry.status]}</span>
                    <span className="case-card-open" aria-hidden="true">
                      Mở
                    </span>
                  </a>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  )
}
