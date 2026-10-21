// PIN entry gate. The field is masked, numeric and exactly eight digits; the
// value lives only in this component's state and is cleared the moment the
// attempt is dispatched, so no PIN survives success, failure or unmount.

import { useState } from 'react'
import type { FormEvent } from 'react'

import { INVALID_PIN_MESSAGE } from './auth/api.ts'

interface LoginPanelProps {
  error: string | null
  pending: boolean
  onLogin: (pin: string) => void
}

const PIN_PATTERN = /^[0-9]{8}$/

export default function LoginPanel({ error, pending, onLogin }: LoginPanelProps) {
  const [pin, setPin] = useState('')
  const [localError, setLocalError] = useState<string | null>(null)

  const displayedError = localError ?? error

  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!PIN_PATTERN.test(pin)) {
      setLocalError(INVALID_PIN_MESSAGE)
      return
    }
    setLocalError(null)
    setPin('')
    onLogin(pin)
  }

  return (
    <section className="panel login-panel" aria-labelledby="login-heading">
      <h2 id="login-heading">Đăng nhập</h2>
      <p className="login-hint">
        Nhập mã PIN 8 chữ số để bắt đầu phiên làm việc riêng của bác sĩ.
      </p>

      <form onSubmit={onSubmit} noValidate>
        <label className="login-field" htmlFor="login-pin">
          Mã PIN
        </label>
        <input
          id="login-pin"
          className="login-pin"
          type="password"
          inputMode="numeric"
          pattern="[0-9]*"
          minLength={8}
          maxLength={8}
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          required
          autoFocus
          disabled={pending}
          value={pin}
          aria-describedby="login-pin-hint"
          aria-invalid={displayedError !== null}
          onChange={(event) => {
            setPin(event.target.value.replace(/[^0-9]/g, '').slice(0, 8))
            setLocalError(null)
          }}
        />
        <p id="login-pin-hint" className="login-hint">
          Đúng 8 chữ số; số 0 ở đầu vẫn hợp lệ.
        </p>

        {displayedError !== null && (
          <p className="error" role="alert">
            {displayedError}
          </p>
        )}

        <button type="submit" disabled={pending}>
          {pending ? 'Đang đăng nhập…' : 'Đăng nhập'}
        </button>
      </form>
    </section>
  )
}
