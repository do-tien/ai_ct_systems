// Authenticated identity projection returned by the login and session routes.
// The browser only ever sees this allowlisted shape; the session token stays in
// the backend-issued HttpOnly cookie.

export interface AuthUser {
  id: number
  displayName: string
}

export interface AuthSession {
  user: AuthUser
  expiresAt: string
}
