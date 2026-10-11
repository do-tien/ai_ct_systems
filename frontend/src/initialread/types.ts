// Initial-read contract for the LC1-15 workflow. Wire and local shapes only;
// no Cornerstone import, so the module stays executable under Node type
// stripping and can be shared by the API, geometry, workflow and view layers.

import type { Vec3 } from '../viewer/series.ts'

// The eight wire fields the backend accepts for an initial mark (LC1-10 §3.4).
// Coordinates are DICOM patient-space LPS mm; the normal is stored as sent,
// never normalized client-side.
export interface InitialMark {
  p1_mm: Vec3
  p2_mm: Vec3
  center_mm: Vec3
  diameter_mm: number
  origin: 'reader'
  ai_candidate_id: null
  view_plane_normal: Vec3
  note: string | null
}

// A draft row: the persisted mark plus a local identity that survives the
// whole-list replacement (the API regenerates database ids and has no mark id),
// plus the Cornerstone annotation UID that currently renders it (or null while
// the annotation has not yet been created for a row).
export interface DraftMark {
  localId: string
  mark: InitialMark
}

// GET /reads/initial response. `locked_at` non-null means the initial read is
// immutable and later edits belong to the final read.
export interface InitialRead {
  id: number
  case_id: number
  kind: 'initial'
  reader: 'reader'
  locked_at: string | null
  marks: InitialMark[]
}

// POST /reads/initial/submit response (no payload sent).
export interface SubmitResult {
  status: 'initial_locked'
  locked_at: string
  final_read_id: number
}

export type WorkflowPhase =
  | 'ready'
  | 'saving'
  | 'submitting'
  | 'reconciling'
  | 'locked'
  | 'conflict'
  | 'error'

// Published workflow state. `editable` reflects the server-side lock only;
// transient UI gates (a drawing in progress, a pending submit) are derived by
// the caller from `phase`.
export interface WorkflowState {
  draft: DraftMark[]
  revision: number
  savedRevision: number
  phase: WorkflowPhase
  error: string | null
  editable: boolean
  // Incremented whenever the draft is replaced by a freshly loaded server
  // snapshot (reconciliation to a different locked read), so the viewer can
  // remount its annotation session before unblocking.
  snapshotEpoch: number
}
