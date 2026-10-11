// Post-lock final-read contract for the LC1-16 workflow. Wire and local shapes
// only; no Cornerstone import, so the module stays executable under Node type
// stripping and can be shared by the API, geometry, workflow and view layers.
// The AI projection is a discriminated union so a succeeded document can never
// carry an error member and a failed one can never carry candidates.

import type { Vec3 } from '../viewer/series.ts'
import type { MarkOrigin } from '../reads/geometry.ts'

export type { MarkOrigin }

export type AICandidateStatus = 'queued' | 'running' | 'succeeded' | 'failed'

// The eight clinical wire fields of a final mark. Unlike the initial read the
// stored normal may be null; endpoints/center/diameter are unchanged.
export interface FinalMark {
  p1_mm: Vec3
  p2_mm: Vec3
  center_mm: Vec3
  diameter_mm: number
  origin: MarkOrigin
  ai_candidate_id: string | null
  view_plane_normal: Vec3 | null
  note: string | null
}

// A draft row: the persisted mark plus a local identity that survives the
// whole-list replacement (the API regenerates database ids and has no mark id).
export interface FinalDraftMark {
  localId: string
  mark: FinalMark
}

export type DecisionKind = 'accepted' | 'dismissed'

// One physician decision about a candidate of the same case. `dismiss_reason`
// is null for an accepted decision and nonempty for a dismissed one.
export interface CandidateDecision {
  candidate_id: string
  decision: DecisionKind
  dismiss_reason: string | null
}

// The whole-list PUT body: both lists are always supplied; an empty list is a
// valid clear.
export interface FinalReplacement {
  marks: FinalMark[]
  candidate_decisions: CandidateDecision[]
}

// GET /reads/final and the PUT /reads/final success envelope.
export interface FinalRead extends FinalReplacement {
  id: number
  case_id: number
  kind: 'final'
  reader: 'reader'
  locked_at: string | null
}

export interface Candidate {
  id: string
  center_mm: Vec3
  size_mm: Vec3
  score: number
}

export interface AIModel {
  bundle: string
  bundle_version: string
  score_threshold: number
}

export interface AIInput {
  num_slices: number
  spacing_mm: number[]
  size_voxels: number[]
}

export interface AITiming {
  load: number
  inference: number
  total: number
}

export interface AIResources {
  device: string
}

export interface AIError {
  code: string
  message: string
}

// State-specific AI projection. `queued`/`running` disclose no candidates and
// no error; `failed` carries an empty candidate list and a fixed error;
// `succeeded` carries the candidates and the exact LC1-13 provenance and has no
// error member at all.
export type AIProjection =
  | { status: 'queued'; candidates: null; error: null }
  | { status: 'running'; candidates: null; error: null }
  | { status: 'failed'; candidates: []; error: AIError }
  | {
      status: 'succeeded'
      candidates: Candidate[]
      model: AIModel
      input: AIInput
      timing_s: AITiming
      resources: AIResources
    }

// POST /finalize response (no payload sent).
export interface FinalizeResult {
  status: 'finalized'
  finalized_at: string
  ai_status: AICandidateStatus
}

// Published final-workflow state. `editable` reflects server authorization and
// the lock only; transient UI gates are derived by the caller.
export interface FinalWorkflowState {
  draft: FinalDraftMark[]
  decisions: CandidateDecision[]
  revision: number
  savedRevision: number
  phase: 'ready' | 'saving' | 'finalizing' | 'reconciling' | 'completed' | 'conflict' | 'error'
  editable: boolean
  error: string | null
  // Incremented whenever the draft is replaced by a freshly loaded server
  // snapshot, so the viewer can rebuild its annotation session.
  snapshotEpoch: number
}
