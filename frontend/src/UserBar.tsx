// Authenticated identity bar: the server-projected display name plus logout.
// The pending state disables the button while the session is being retired.

interface UserBarProps {
  displayName: string
  pending: boolean
  onLogout: () => void
}

export default function UserBar({ displayName, pending, onLogout }: UserBarProps) {
  return (
    <section className="user-bar" aria-label="Phiên làm việc" aria-busy={pending}>
      <p className="user-bar-name">
        Người dùng: <strong>{displayName}</strong>
      </p>
      <button type="button" className="user-bar-logout" onClick={onLogout} disabled={pending}>
        {pending ? 'Đang đăng xuất…' : 'Đăng xuất'}
      </button>
    </section>
  )
}
