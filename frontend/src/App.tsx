import { useCallback, useEffect, useRef, useState } from 'react'

import CasePage from './CasePage.tsx'
import Dashboard from './Dashboard.tsx'
import LoginPanel from './LoginPanel.tsx'
import UserBar from './UserBar.tsx'
import {
  AuthApiError,
  loadSession,
  login as loginRequest,
  logout as logoutRequest,
} from './auth/api.ts'
import {
  AUTH_REQUIRED_EVENT,
  createAuthGenerationGate,
  scheduleSessionExpiry,
} from './auth/session.ts'
import type { AuthGenerationGate, SessionExpiryHandle } from './auth/session.ts'
import type { AuthSession } from './auth/types.ts'

type CheckName = 'backend' | 'postgres' | 'orthanc'
type RowState = 'loading' | 'ok' | 'unavailable' | 'unknown'

type AuthView =
  | { status: 'loading' }
  | { status: 'anonymous' }
  | { status: 'authenticated'; session: AuthSession }
  | { status: 'failed'; message: string }

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
const AUTH_LOAD_ERROR_MESSAGE = 'Không kiểm tra được phiên làm việc. Kiểm tra kết nối rồi thử lại.'
const LOGIN_ERROR_MESSAGE = 'Không đăng nhập được. Kiểm tra kết nối rồi thử lại.'

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

  const [auth, setAuth] = useState<AuthView>({ status: 'loading' })
  const [loginPending, setLoginPending] = useState(false)
  const [loginError, setLoginError] = useState<string | null>(null)
  const [logoutPending, setLogoutPending] = useState(false)

  const aliveRef = useRef(true)
  const authRequestRef = useRef<AbortController | null>(null)
  const expiryRef = useRef<SessionExpiryHandle | null>(null)
  const gateRef = useRef<AuthGenerationGate | null>(null)
  if (gateRef.current === null) {
    gateRef.current = createAuthGenerationGate()
  }
  const gate = gateRef.current
  // Mirrors the auth view for event handlers, which must decide synchronously
  // whether a 401 belongs to the authenticated generation.
  const authRef = useRef<AuthView>({ status: 'loading' })

  const updateAuth = useCallback((next: AuthView) => {
    authRef.current = next
    setAuth(next)
  }, [])

  const cancelExpiry = useCallback(() => {
    expiryRef.current?.cancel()
    expiryRef.current = null
  }, [])

  // Retires the mounted generation before clearing the user. Late session,
  // login or logout results then fail the gate check and cannot repopulate the
  // next user's UI. `force` is used by expiry, where there is no authenticated
  // view yet or the timestamp itself proved unusable.
  const retireSession = useCallback(
    (options: { force?: boolean } = {}) => {
      if (options.force !== true && authRef.current.status !== 'authenticated') {
        return
      }
      gate.retire()
      authRequestRef.current?.abort()
      authRequestRef.current = null
      cancelExpiry()
      updateAuth({ status: 'anonymous' })
      setLoginPending(false)
      setLoginError(null)
      setLogoutPending(false)
    },
    [cancelExpiry, gate, updateAuth],
  )

  const applySession = useCallback(
    (session: AuthSession) => {
      cancelExpiry()
      const handle = scheduleSessionExpiry({
        expiresAt: session.expiresAt,
        onExpire: () => retireSession({ force: true }),
      })
      if (handle === null) {
        return
      }
      expiryRef.current = handle
      setLoginError(null)
      updateAuth({ status: 'authenticated', session })
    },
    [cancelExpiry, retireSession, updateAuth],
  )

  const loadAuthSession = useCallback(async () => {
    const generation = gate.begin()
    authRequestRef.current?.abort()
    const controller = new AbortController()
    authRequestRef.current = controller
    updateAuth({ status: 'loading' })
    try {
      const session = await loadSession(controller.signal)
      if (!aliveRef.current || !gate.isCurrent(generation)) {
        return
      }
      if (session === null) {
        updateAuth({ status: 'anonymous' })
        return
      }
      applySession(session)
    } catch (failure) {
      if (!aliveRef.current || !gate.isCurrent(generation) || controller.signal.aborted) {
        return
      }
      updateAuth({
        status: 'failed',
        message: failure instanceof AuthApiError ? failure.message : AUTH_LOAD_ERROR_MESSAGE,
      })
    }
  }, [applySession, gate, updateAuth])

  const submitLogin = useCallback(
    async (pin: string) => {
      const generation = gate.begin()
      authRequestRef.current?.abort()
      const controller = new AbortController()
      authRequestRef.current = controller
      setLoginPending(true)
      setLoginError(null)
      try {
        const session = await loginRequest(pin, controller.signal)
        if (!aliveRef.current || !gate.isCurrent(generation)) {
          return
        }
        applySession(session)
      } catch (failure) {
        if (!aliveRef.current || !gate.isCurrent(generation) || controller.signal.aborted) {
          return
        }
        setLoginError(failure instanceof AuthApiError ? failure.message : LOGIN_ERROR_MESSAGE)
      } finally {
        if (aliveRef.current && gate.isCurrent(generation)) {
          setLoginPending(false)
        }
      }
    },
    [applySession, gate],
  )

  const submitLogout = useCallback(() => {
    if (logoutPending) {
      return
    }
    const generation = gate.begin()
    authRequestRef.current?.abort()
    const controller = new AbortController()
    authRequestRef.current = controller
    cancelExpiry()
    setLogoutPending(true)
    setLoginError(null)
    // Logout is best effort, so it resolves even when the session is gone or
    // the network fails; either way the local session is retired below.
    void logoutRequest(controller.signal).then(() => {
      if (!aliveRef.current || !gate.isCurrent(generation)) {
        return
      }
      updateAuth({ status: 'anonymous' })
      setLogoutPending(false)
    })
  }, [cancelExpiry, gate, logoutPending, updateAuth])

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

  useEffect(() => {
    aliveRef.current = true
    void loadAuthSession()
    const onAuthRequired = (): void => {
      retireSession()
    }
    window.addEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
    return () => {
      aliveRef.current = false
      window.removeEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
      authRequestRef.current?.abort()
      expiryRef.current?.cancel()
      expiryRef.current = null
    }
  }, [loadAuthSession, retireSession])

  // A confirmed upload is opened with a full navigation to the existing deep
  // link; no router is added.
  const openCase = useCallback((id: number) => {
    window.location.assign(`/?case=${id}`)
  }, [])

  // Only an authenticated case route gets the dark reading context.
  const workspace = auth.status === 'authenticated' && caseId !== null

  return (
    <div className={workspace ? 'app app-workspace' : 'app'}>
      <a className="skip-link" href="#main-content">
        Bỏ qua đến nội dung chính
      </a>
      <header className="app-header">
        <a className="brand" href="/" aria-label="Nodule Reader, về trang chủ">
          <span className="brand-mark" aria-hidden="true">
            NR
          </span>
          <span className="brand-text">
            <strong>Nodule Reader</strong>
            <small>Công cụ nghiên cứu CT lồng ngực</small>
          </span>
        </a>
        <div className="app-header-actions">
          <span className="app-badge">Không dùng để chẩn đoán</span>
          {auth.status === 'authenticated' && (
            <UserBar
              displayName={auth.session.user.displayName}
              pending={logoutPending}
              onLogout={submitLogout}
            />
          )}
        </div>
      </header>

      <main className="page" id="main-content" tabIndex={-1}>
        {auth.status === 'loading' && (
          <section className="panel auth-loading" aria-busy="true">
            <p role="status">Đang kiểm tra phiên làm việc…</p>
          </section>
        )}

        {auth.status === 'failed' && (
          <section className="panel auth-failure" aria-labelledby="auth-failure-heading">
            <h2 id="auth-failure-heading">Không mở được phiên làm việc</h2>
            <p className="error" role="alert">
              {auth.message}
            </p>
            <button type="button" className="button-primary" onClick={() => void loadAuthSession()}>
              Thử lại
            </button>
          </section>
        )}

        {auth.status === 'anonymous' && (
          <LoginPanel
            pending={loginPending}
            error={loginError}
            onLogin={(pin) => void submitLogin(pin)}
          />
        )}

        {auth.status === 'authenticated' && (caseId === null ? <Dashboard onOpenCase={openCase} /> : <CasePage caseId={caseId} />)}
      </main>

      <footer className="app-footer">
        <p className="footer-note">
          Công cụ nghiên cứu hỗ trợ đọc CT lồng ngực: bác sĩ đọc độc lập trước, sau đó xem xét các
          điểm không đồng ý với gợi ý hỗ trợ và giữ quyết định cuối cùng. Không dùng để chẩn đoán.
        </p>

        <details className="diagnostics">
          <summary>Chẩn đoán kết nối</summary>

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

          <button type="button" className="button-quiet" onClick={() => void runCheck()} disabled={pending}>
            Kiểm tra lại
          </button>

          {error !== null && (
            <p className="error" role="alert">
              {error}
            </p>
          )}
        </details>
      </footer>
    </div>
  )
}
