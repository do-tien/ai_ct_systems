// Browser session lifecycle: the authenticated transport, the single
// application-scoped 401 notification, absolute expiry scheduling and the
// generation gate that isolates a retired UI from late results.

export const AUTH_REQUIRED_EVENT = 'nr-auth-required'

// The largest delay setTimeout accepts without overflowing.
export const MAX_TIMER_DELAY_MS = 2_147_483_647

export type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>

export interface AuthTransportDeps {
  fetch?: FetchLike
  dispatch?: (event: Event) => void
}

export interface ExpiryScheduler {
  setTimeout: (handler: () => void, delayMs: number) => unknown
  clearTimeout: (handle: unknown) => void
}

export interface SessionExpiryOptions {
  expiresAt: string
  onExpire: () => void
  now?: () => number
  scheduler?: ExpiryScheduler
}

export interface SessionExpiryHandle {
  cancel: () => void
}

export interface AuthGenerationGate {
  // Starts an operation and returns the token it must present when it lands.
  begin: () => number
  // Invalidates every token issued so far.
  retire: () => void
  isCurrent: (generation: number) => boolean
}

const defaultScheduler: ExpiryScheduler = {
  setTimeout: (handler, delayMs) => setTimeout(handler, delayMs),
  clearTimeout: (handle) => clearTimeout(handle as number),
}

// Same RFC3339 shape the login/session response uses. A looser value must not
// reach expiry scheduling, so both the wire parser and this scheduler call it.
const TIMESTAMP_PATTERN =
  /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2})$/

export function isSessionTimestamp(value: string): boolean {
  return TIMESTAMP_PATTERN.test(value) && Number.isFinite(Date.parse(value))
}

export function createAuthGenerationGate(): AuthGenerationGate {
  let generation = 0
  return {
    begin: () => {
      generation += 1
      return generation
    },
    retire: () => {
      generation += 1
    },
    isCurrent: (candidate) => candidate === generation,
  }
}

// Safe in Node tests and in the browser: without a window there is nothing to
// notify, and a missing dispatch seam never throws.
export function notifyAuthRequired(deps: AuthTransportDeps = {}): void {
  if (deps.dispatch !== undefined) {
    deps.dispatch(new Event(AUTH_REQUIRED_EVENT))
    return
  }
  if (typeof window === 'undefined') {
    return
  }
  window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT))
}

// Delegates every byte of the request and the response unchanged so response
// streaming and call-site options are preserved; only the status is inspected.
export function authenticatedFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
  deps: AuthTransportDeps = {},
): Promise<Response> {
  const fetcher = deps.fetch ?? ((request, options) => fetch(request, options))
  return fetcher(input, init).then((response) => {
    if (response.status === 401) {
      notifyAuthRequired(deps)
    }
    return response
  })
}

// Schedules retirement at the server-provided absolute timestamp. An invalid,
// missing or already elapsed timestamp fails closed by retiring immediately.
// Long lifetimes are chained because a single timeout cannot exceed
// MAX_TIMER_DELAY_MS.
export function scheduleSessionExpiry(
  options: SessionExpiryOptions,
): SessionExpiryHandle | null {
  const now = options.now ?? Date.now
  const scheduler = options.scheduler ?? defaultScheduler
  const target = Date.parse(options.expiresAt)
  if (!isSessionTimestamp(options.expiresAt) || !Number.isFinite(target)) {
    options.onExpire()
    return null
  }
  const remaining = target - now()
  if (remaining <= 0) {
    options.onExpire()
    return null
  }

  let handle: unknown = null
  let canceled = false
  const schedule = (delayMs: number): void => {
    handle = scheduler.setTimeout(() => {
      if (canceled) {
        return
      }
      const left = target - now()
      if (left <= 0) {
        options.onExpire()
        return
      }
      schedule(left)
    }, Math.min(delayMs, MAX_TIMER_DELAY_MS))
  }
  schedule(remaining)

  return {
    cancel: () => {
      if (canceled) {
        return
      }
      canceled = true
      if (handle !== null) {
        scheduler.clearTimeout(handle)
      }
    },
  }
}
