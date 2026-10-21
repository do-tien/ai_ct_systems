// Upload wire and local shapes for the browser DICOM intake. Wire fields only;
// no Cornerstone import, so the module stays executable under Node type
// stripping and can be shared by the API, workflow and panel layers. Nothing
// here carries a patient tag, a path or an AI field.

// One selectable series exactly as the server discloses it before the initial
// read is locked: an opaque session key, an index, a modality and a count.
export interface UploadSeriesOption {
  id: string
  index: number
  modality: string
  instance_count: number
  suitable: boolean
  reason_code: string | null
}

// GET/POST /api/uploads response. `selected_series_id` and `case_id` are null
// until the session is confirmed.
export interface UploadSession {
  id: string
  expires_at: string
  skipped_files: number
  series: UploadSeriesOption[]
  selected_series_id: string | null
  case_id: number | null
}

// POST /api/uploads/{id}/cases response. `existing` means the series was
// already imported and no second case was created.
export interface UploadCaseResult {
  case_id: number
  instance_count: number
  existing: boolean
}

// The user-visible phases. `uploading` and `inspecting` are distinct because
// byte progress stopping at 100% is not task completion: the server still has
// to parse the batch and describe every series.
export type UploadPhase =
  | 'idle'
  | 'uploading'
  | 'inspecting'
  | 'choosing'
  | 'confirming'
  | 'opened'
  | 'uncertain'
  | 'canceled'
  | 'failed'

// Published workflow state. `progress` is a 0..1 fraction only while uploading;
// it is never synthesized for the server-side inspection step.
export interface UploadState {
  phase: UploadPhase
  fileCount: number
  totalBytes: number
  transferredBytes: number
  progress: number | null
  session: UploadSession | null
  selectedSeriesId: string | null
  caseId: number | null
  error: string | null
  // Set when a stored session token was restored after a reload, so the panel
  // can explain where the metadata came from.
  restored: boolean
}
