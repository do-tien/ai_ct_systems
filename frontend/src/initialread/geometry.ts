// Initial-read geometry. The measurement and slice-matching implementation is
// shared with the final read in ../reads/geometry.ts; this module narrows it to
// the initial contract (a required stored normal, `origin: 'reader'`, no
// candidate link) so an AI or final mark can never be produced here.

import type { Vec3 } from '../viewer/series.ts'
import { measureEndpoints } from '../reads/geometry.ts'
import type { InitialMark } from './types.ts'

export { MarkGeometryError, markListsEqual, marksEqual } from '../reads/geometry.ts'

// center = midpoint, diameter = distance between endpoints, both in mm and
// straight from the world coordinates the tool reports. Rejects anything that
// must never reach the API: nonfinite endpoints, identical endpoints, a
// missing/zero normal.
export function measureInitialMark(p1: Vec3, p2: Vec3, normal: Vec3, note: string | null): InitialMark {
  const geometry = measureEndpoints(p1, p2, normal, note)
  return {
    p1_mm: geometry.p1_mm,
    p2_mm: geometry.p2_mm,
    center_mm: geometry.center_mm,
    diameter_mm: geometry.diameter_mm,
    origin: 'reader',
    ai_candidate_id: null,
    view_plane_normal: geometry.view_plane_normal,
    note: geometry.note,
  }
}

export { findReadMarkSlice as findMarkSlice } from '../reads/geometry.ts'
