// Authenticated identity bar: the server-projected display name plus logout.
// The pending state disables the button while the session is being retired.

interface UserBarProps {
  displayName: string
  pending: boolean
  onLogout: () => void
}

export default function UserBar({ displayName, pending, onLogout }: UserBarProps) {
  const initial = displayName.trim().slice(0, 1).toUpperCase() || '?'
  return (
    <section className="user-bar" aria-label="Phiên làm việc" aria-busy={pending}>
      <span className="user-avatar" aria-hidden="true">
        {initial}
      </span>
      <span className="user-bar-name">
        <small>Bác sĩ</small>
        <strong>{displayName}</strong>
      </span>
      <button type="button" className="button-quiet user-bar-logout" onClick={onLogout} disabled={pending}>
        {pending ? 'Đang đăng xuất…' : 'Đăng xuất'}
      </button>
    </section>
  )
}
