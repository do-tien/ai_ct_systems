import { useCallback, useEffect, useRef, useState } from 'react'

import CasePage from './CasePage.tsx'

type CheckName = 'backend' | 'postgres' | 'orthanc'
type RowState = 'loading' | 'ok' | 'unavailable' | 'unknown'

const CHECK_ORDER: CheckName[] = ['backend', 'postgres', 'orthanc']

const CHECK_LABELS: Record<CheckName, string> = {
  backend: 'Backend',
  postgres: 'PostgreSQL',
  orthanc: 'Orthanc',
}

const ROW_TEXT: Record<RowState, string> = {
  loading: 'Đang kiểm tra…',
  ok: 'Kết nối tốt',
  unavailable: 'Không khả dụng',
  unknown: 'Chưa xác định',
}

const REQUEST_TIMEOUT_MS = 5000
const INVALID_RESPONSE_MESSAGE = 'Không kết nối được backend hoặc phản hồi không hợp lệ'

// Only a positive decimal case id opens the viewer; anything else keeps the
// health page and never sends a malformed case request.
function parseCaseId(search: string): number | null {
  const value = new URLSearchParams(search).get('case')
  if (value === null || !/^[1-9][0-9]*$/.test(value)) {
    return null
  }
  const id = Number(value)
  return Number.isSafeInteger(id) ? id : null
}

function filledRows(state: RowState): Record<CheckName, RowState> {
  return { backend: state, postgres: state, orthanc: state }
}

function isCheckStatus(value: unknown): value is 'ok' | 'unavailable' {
  return value === 'ok' || value === 'unavailable'
}

// Accepts only the documented health contract. Anything else is treated as an
// invalid response so stale green statuses are cleared instead of kept.
function parseHealth(payload: unknown): Record<CheckName, RowState> | null {
  if (typeof payload !== 'object' || payload === null) {
    return null
  }
  const body = payload as Record<string, unknown>
  if (body.status !== 'ok' && body.status !== 'degraded') {
    return null
  }
  const checks = body.checks
  if (typeof checks !== 'object' || checks === null) {
    return null
  }
  const values = checks as Record<string, unknown>
  if (!isCheckStatus(values.backend) || !isCheckStatus(values.postgres) || !isCheckStatus(values.orthanc)) {
    return null
  }
  return { backend: values.backend, postgres: values.postgres, orthanc: values.orthanc }
}

export default function App() {
  const caseId = parseCaseId(window.location.search)
  const [rows, setRows] = useState<Record<CheckName, RowState>>(() => filledRows('loading'))
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(true)

  const mountedRef = useRef(true)
  const activeRequestRef = useRef<AbortController | null>(null)

  const runCheck = useCallback(async () => {
    activeRequestRef.current?.abort()
    const controller = new AbortController()
    activeRequestRef.current = controller

    setRows(filledRows('loading'))
    setError(null)
    setPending(true)

    // Distinguishes our own timeout (show the error) from an unmount abort
    // (state update is skipped entirely).
    let timedOut = false
    const timeoutId = window.setTimeout(() => {
      timedOut = true
      controller.abort()
    }, REQUEST_TIMEOUT_MS)

    const canUpdate = () => mountedRef.current && !(controller.signal.aborted && !timedOut)

    try {
      const response = await fetch('/healthz', {
        signal: controller.signal,
        headers: { Accept: 'application/json' },
      })
      const payload: unknown = await response.json()
      const checks = parseHealth(payload)
      if (checks === null) {
        if (canUpdate()) {
          setRows(filledRows('unknown'))
          setError(INVALID_RESPONSE_MESSAGE)
        }
        return
      }
      if (canUpdate()) {
        setRows(checks)
        setError(null)
      }
    } catch {
      if (canUpdate()) {
        setRows(filledRows('unknown'))
        setError(INVALID_RESPONSE_MESSAGE)
      }
    } finally {
      window.clearTimeout(timeoutId)
      if (canUpdate() && activeRequestRef.current === controller) {
        setPending(false)
      }
    }
  }, [])

  useEffect(() => {
    mountedRef.current = true
    void runCheck()
    return () => {
      mountedRef.current = false
      activeRequestRef.current?.abort()
    }
  }, [runCheck])

  return (
    <main className="page">
      <header className="page-header">
        <h1>Nodule Reader — môi trường phát triển</h1>
        <p className="notice">
          Công cụ nghiên cứu: xem ảnh CT trục của một ca đã nhập, không hiển thị gợi ý AI và không dùng để chẩn đoán.
        </p>
      </header>

      {caseId === null ? (
        <p className="notice">
          Mở một ca đã nhập bằng địa chỉ <code>/?case=&lt;id&gt;</code>, ví dụ <code>/?case=1</code>.
        </p>
      ) : (
        <CasePage caseId={caseId} />
      )}

      <section className="panel" aria-labelledby="status-heading">
        <h2 id="status-heading">Kiểm tra kết nối</h2>

        <table className="status-table">
          <thead>
            <tr>
              <th scope="col">Thành phần</th>
              <th scope="col">Trạng thái</th>
            </tr>
          </thead>
          <tbody>
            {CHECK_ORDER.map((name) => (
              <tr key={name}>
                <th scope="row">{CHECK_LABELS[name]}</th>
                <td className={`status status-${rows[name]}`}>{ROW_TEXT[rows[name]]}</td>
              </tr>
            ))}
          </tbody>
        </table>

        <button type="button" onClick={() => void runCheck()} disabled={pending}>
          Kiểm tra lại
        </button>

        {error !== null && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
      </section>
    </main>
  )
}
