// Shared confirmation surface for irreversible read actions. It is an inline
// group, not a modal: the rest of the page stays reachable, so it must not claim
// dialog semantics or trap focus. Opening moves focus to the confirm action and
// cancel returns focus to the trigger.

import { useEffect, useId, useRef, useState } from 'react'
import type { JSX } from 'react'

export interface ActionConfirmProps {
  triggerLabel: string
  heading: string
  description: string
  confirmLabel: string
  disabled: boolean
  onConfirm: () => void
}

export default function ActionConfirm({
  triggerLabel,
  heading,
  description,
  confirmLabel,
  disabled,
  onConfirm,
}: ActionConfirmProps): JSX.Element {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const confirmRef = useRef<HTMLButtonElement>(null)
  const restoreFocusRef = useRef(false)
  const headingId = useId()
  const descriptionId = useId()

  useEffect(() => {
    if (open) {
      confirmRef.current?.focus()
      return
    }
    // The trigger only exists after the closed state renders, so cancel cannot
    // focus it synchronously.
    if (restoreFocusRef.current) {
      restoreFocusRef.current = false
      triggerRef.current?.focus()
    }
  }, [open])

  const cancel = (): void => {
    restoreFocusRef.current = true
    setOpen(false)
  }

  if (!open) {
    return (
      <button
        ref={triggerRef}
        type="button"
        className="button-primary"
        disabled={disabled}
        onClick={() => setOpen(true)}
      >
        {triggerLabel}
      </button>
    )
  }

  return (
    <div
      className="confirm-card"
      role="group"
      aria-labelledby={headingId}
      aria-describedby={descriptionId}
    >
      <p className="confirm-heading" id={headingId}>
        {heading}
      </p>
      <p id={descriptionId}>{description}</p>
      <div className="submit-actions">
        <button
          ref={confirmRef}
          type="button"
          className="button-primary confirm-destructive"
          disabled={disabled}
          onClick={() => {
            // Close first so the confirmation cannot be submitted twice.
            setOpen(false)
            onConfirm()
          }}
        >
          {confirmLabel}
        </button>
        <button type="button" className="button-quiet" onClick={cancel}>
          Hủy
        </button>
      </div>
    </div>
  )
}
