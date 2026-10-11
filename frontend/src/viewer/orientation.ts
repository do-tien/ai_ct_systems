// Anatomical edge labels for the displayed axial plane, derived from the real
// viewport directions rather than from a fixed assumption about image layout.
// Screen-right and screen-down are unit vectors in DICOM patient-space LPS mm.

import type { Vec3 } from './series.ts'

export interface EdgeLabels {
  left: string
  right: string
  top: string
  bottom: string
}

// A direction's label: every LPS axis that contributes at least half of the
// unit vector contributes its anatomical letter, in x, y, z order, so a
// rotated plane yields a compound direction such as "LP" instead of a
// misleading single letter.
function directionLabel(vector: Vec3): string {
  const labels: string[] = []
  if (Math.abs(vector[0]) >= 0.5) {
    labels.push(vector[0] > 0 ? 'L' : 'R')
  }
  if (Math.abs(vector[1]) >= 0.5) {
    labels.push(vector[1] > 0 ? 'P' : 'A')
  }
  if (Math.abs(vector[2]) >= 0.5) {
    labels.push(vector[2] > 0 ? 'S' : 'I')
  }
  return labels.join('')
}

function unitDirection(vector: Vec3): Vec3 {
  const length = Math.hypot(vector[0], vector[1], vector[2])
  if (!Number.isFinite(length) || length === 0) {
    throw new Error('orientation direction is not a finite non-zero vector')
  }
  return [vector[0] / length, vector[1] / length, vector[2] / length]
}

export function edgeLabels(screenRightLPS: Vec3, screenDownLPS: Vec3): EdgeLabels {
  const right = unitDirection(screenRightLPS)
  const down = unitDirection(screenDownLPS)
  const left: Vec3 = [-right[0], -right[1], -right[2]]
  const up: Vec3 = [-down[0], -down[1], -down[2]]
  return {
    left: directionLabel(left),
    right: directionLabel(right),
    top: directionLabel(up),
    bottom: directionLabel(down),
  }
}
