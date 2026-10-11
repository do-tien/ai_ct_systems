import type { ReadMark } from './reads/geometry.ts'

// Draft rows for either read. Pure presentation: no fetching and no Cornerstone
// globals; a row click asks the viewer session to focus the mark and the
// explicit delete asks it to remove the owned annotation.

interface MarkListProps {
  draft: Array<{ localId: string; mark: ReadMark }>
  editable: boolean
  onFocus: (localId: string) => void
  onRemove: (localId: string) => void
}

function formatDiameter(diameterMM: number): string {
  return `${diameterMM.toFixed(1)} mm`
}

// A neutral origin label; it never implies a clinical meaning the backend did
// not record.
function originLabel(mark: ReadMark): string | null {
  if (mark.origin === 'reader_after_ai') {
    return 'sau AI'
  }
  if (mark.origin === 'from_ai_candidate') {
    return mark.ai_candidate_id === null ? 'gắn ứng viên' : `gắn ứng viên ${mark.ai_candidate_id}`
  }
  return null
}

export default function MarkList({ draft, editable, onFocus, onRemove }: MarkListProps) {
  if (draft.length === 0) {
    return <p className="mark-empty">Chưa có dấu nào. Chọn “Đo nốt” rồi kéo trên ảnh để thêm dấu.</p>
  }
  return (
    <ol className="mark-list">
      {draft.map((entry, index) => {
        const label = originLabel(entry.mark)
        return (
          <li key={entry.localId} className="mark-row">
            <button type="button" className="mark-focus" onClick={() => onFocus(entry.localId)}>
              <span className="mark-index">Dấu {index + 1}</span>
              <span className="mark-diameter">{formatDiameter(entry.mark.diameter_mm)}</span>
              {label !== null && <span className="mark-origin">{label}</span>}
              {entry.mark.note !== null && entry.mark.note !== '' && <span className="mark-note">{entry.mark.note}</span>}
            </button>
            {editable && (
              <button
                type="button"
                className="mark-delete"
                onClick={() => onRemove(entry.localId)}
                aria-label={`Xóa dấu ${index + 1}`}
              >
                Xóa
              </button>
            )}
          </li>
        )
      })}
    </ol>
  )
}
