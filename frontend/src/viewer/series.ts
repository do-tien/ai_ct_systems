// Pure DICOM JSON parsing and axial geometry for the series behind one case.
// No fetch, no DOM and no Cornerstone: the plan is derived only from the QIDO
// instance inventory plus the series WADO metadata and the case identity.

import type { CaseMetadata } from '../case.ts'
import { isValidUID } from '../case.ts'

export type Vec3 = [number, number, number]

export interface SeriesSlice {
  imageId: string
  sopInstanceUID: string
  positionMM: number
  imagePositionPatient: Vec3
  metadata: Record<string, unknown>
}

export interface SeriesPlan {
  slices: SeriesSlice[]
  frameOfReferenceUID: string
  rowDirection: Vec3
  columnDirection: Vec3
  normal: Vec3
}

export class SeriesError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SeriesError'
  }
}

const CT_IMAGE_STORAGE = '1.2.840.10008.5.1.4.1.1.2'
// Tolerances mirror the importer/validator: direction components 1e-5, spacing
// 0.01 mm, and a plane is ambiguous when two projections land within 0.01 mm.
const DIRECTION_TOLERANCE = 1e-5
const SPACING_TOLERANCE_MM = 0.01
const PLANE_TOLERANCE_MM = 0.01
const MAX_INVENTORY = 10000

function requireItem(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new SeriesError(`${label} is not a DICOM JSON object`)
  }
  return value as Record<string, unknown>
}

function readUID(item: Record<string, unknown>, tag: string, label: string): string {
  const entry = item[tag]
  const value =
    typeof entry === 'object' && entry !== null && !Array.isArray(entry)
      ? (entry as Record<string, unknown>).Value
      : undefined
  const first = Array.isArray(value) ? value[0] : undefined
  if (!isValidUID(first)) {
    throw new SeriesError(`${label} UID is missing or not valid DICOM`)
  }
  return first
}

function readNumberVector(
  item: Record<string, unknown>,
  tag: string,
  count: number,
  label: string,
): number[] {
  const entry = item[tag]
  if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) {
    throw new SeriesError(`${label} is missing`)
  }
  const values = (entry as Record<string, unknown>).Value
  if (!Array.isArray(values) || values.length !== count) {
    throw new SeriesError(`${label} has the wrong number of components`)
  }
  return values.map((value) => {
    if (typeof value === 'number') {
      if (!Number.isFinite(value)) {
        throw new SeriesError(`${label} contains a non-finite value`)
      }
      return value
    }
    if (typeof value === 'string') {
      const text = value.trim()
      if (text.length === 0) {
        throw new SeriesError(`${label} contains an empty value`)
      }
      const parsed = Number(text)
      if (!Number.isFinite(parsed)) {
        throw new SeriesError(`${label} contains a non-numeric value`)
      }
      return parsed
    }
    throw new SeriesError(`${label} contains a value that is not numeric`)
  })
}

function readPositiveInteger(item: Record<string, unknown>, tag: string, label: string): number {
  const [value] = readNumberVector(item, tag, 1, label)
  if (!Number.isInteger(value) || value <= 0) {
    throw new SeriesError(`${label} is not a positive integer`)
  }
  return value
}

function readOptionalWord(item: Record<string, unknown>, tag: string): string | undefined {
  const entry = item[tag]
  const value =
    typeof entry === 'object' && entry !== null && !Array.isArray(entry)
      ? (entry as Record<string, unknown>).Value
      : undefined
  const first = Array.isArray(value) ? value[0] : undefined
  return typeof first === 'string' ? first : undefined
}

function cross(a: Vec3, b: Vec3): Vec3 {
  return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]]
}

function dot(a: Vec3, b: Vec3): number {
  return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]
}

function normalize(vector: Vec3, label: string): Vec3 {
  const length = Math.hypot(vector[0], vector[1], vector[2])
  if (!Number.isFinite(length) || length === 0) {
    throw new SeriesError(`${label} is degenerate`)
  }
  return [vector[0] / length, vector[1] / length, vector[2] / length]
}

function requireBareOrigin(origin: string): string {
  let parsed: URL
  try {
    parsed = new URL(origin)
  } catch {
    throw new SeriesError('application origin is not a valid URL')
  }
  if (parsed.origin !== origin) {
    throw new SeriesError('application origin must be a bare origin such as https://host:port')
  }
  return parsed.origin
}

function requireMatchingUID(
  item: Record<string, unknown>,
  tag: string,
  expected: string,
  label: string,
): void {
  const entry = item[tag]
  if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) {
    return
  }
  const value = (entry as Record<string, unknown>).Value
  const first = Array.isArray(value) ? value[0] : undefined
  if (first === undefined) {
    return
  }
  if (first !== expected) {
    throw new SeriesError(`inventory ${label} does not match the case`)
  }
}

interface SliceGeometry {
  sop: string
  frame: string
  rows: number
  columns: number
  spacing: [number, number]
  iop: number[]
  ipp: Vec3
  slice: SeriesSlice
}

// Validates one WADO metadata instance and its per-slice geometry. Cross-slice
// consistency is checked by the caller once every slice is known.
function parseSliceGeometry(
  item: Record<string, unknown>,
  studyUID: string,
  seriesUID: string,
  frameOfReferenceUID: string,
): SliceGeometry {
  const sop = readUID(item, '00080018', 'instance SOP')
  if (readUID(item, '0020000D', 'study') !== studyUID) {
    throw new SeriesError('series metadata study does not match the case')
  }
  if (readUID(item, '0020000E', 'series') !== seriesUID) {
    throw new SeriesError('series metadata series does not match the case')
  }
  const frame = readUID(item, '00200052', 'frame of reference')
  if (frame !== frameOfReferenceUID) {
    throw new SeriesError('series metadata frame of reference does not match the case')
  }
  if (readUID(item, '00080016', 'SOP class') !== CT_IMAGE_STORAGE) {
    throw new SeriesError('series metadata SOP class is not classic single-frame CT image storage')
  }
  const modality = readOptionalWord(item, '00080060')
  if (modality !== undefined && modality !== 'CT') {
    throw new SeriesError('series metadata modality is not CT')
  }
  if (item['00280008'] !== undefined) {
    const [frames] = readNumberVector(item, '00280008', 1, 'number of frames')
    if (frames !== 1) {
      throw new SeriesError('series metadata is multiframe; this viewer supports single-frame CT only')
    }
  }
  const rows = readPositiveInteger(item, '00280010', 'image rows')
  const columns = readPositiveInteger(item, '00280011', 'image columns')
  const spacing = readNumberVector(item, '00280030', 2, 'pixel spacing')
  if (!(spacing[0] > 0) || !(spacing[1] > 0)) {
    throw new SeriesError('pixel spacing must be positive')
  }
  const iop = readNumberVector(item, '00200037', 6, 'image orientation')
  const ippValues = readNumberVector(item, '00200032', 3, 'image position')
  const rowDirection = normalize([iop[0], iop[1], iop[2]], 'image orientation row')
  const columnDirection = normalize([iop[3], iop[4], iop[5]], 'image orientation column')
  if (
    Math.abs(Math.hypot(iop[0], iop[1], iop[2]) - 1) > DIRECTION_TOLERANCE ||
    Math.abs(Math.hypot(iop[3], iop[4], iop[5]) - 1) > DIRECTION_TOLERANCE ||
    Math.abs(dot(rowDirection, columnDirection)) > DIRECTION_TOLERANCE
  ) {
    throw new SeriesError('image orientation is not a unit orthogonal basis')
  }
  const ipp: Vec3 = [ippValues[0], ippValues[1], ippValues[2]]
  return {
    sop,
    frame,
    rows,
    columns,
    spacing: [spacing[0], spacing[1]],
    iop,
    ipp,
    slice: {
      imageId: '',
      sopInstanceUID: sop,
      positionMM: 0,
      imagePositionPatient: ipp,
      metadata: item,
    },
  }
}

export function buildSeriesPlan(
  qido: unknown,
  metadata: unknown,
  caseMetadata: CaseMetadata,
  origin: string,
): SeriesPlan {
  const base = requireBareOrigin(origin)
  if (!Array.isArray(metadata) || metadata.length === 0) {
    throw new SeriesError('series metadata inventory is empty')
  }
  if (!Array.isArray(qido) || qido.length === 0) {
    throw new SeriesError('QIDO instance inventory is empty')
  }
  if (metadata.length > MAX_INVENTORY || qido.length > MAX_INVENTORY) {
    throw new SeriesError('series inventory exceeds the supported bound')
  }
  const studyUID = caseMetadata.studyInstanceUID
  const seriesUID = caseMetadata.seriesInstanceUID
  if (!isValidUID(studyUID) || !isValidUID(seriesUID)) {
    throw new SeriesError('case study or series UID is not valid DICOM')
  }

  const qidoSops = new Set<string>()
  for (const raw of qido) {
    const item = requireItem(raw, 'QIDO instance inventory entry')
    const sop = readUID(item, '00080018', 'instance SOP')
    if (qidoSops.has(sop)) {
      throw new SeriesError('QIDO inventory contains a duplicate SOP instance')
    }
    qidoSops.add(sop)
    requireMatchingUID(item, '0020000D', studyUID, 'study')
    requireMatchingUID(item, '0020000E', seriesUID, 'series')
  }

  const seenSops = new Set<string>()
  const geometries = metadata.map((raw) => {
    const geometry = parseSliceGeometry(
      requireItem(raw, 'series metadata instance'),
      studyUID,
      seriesUID,
      caseMetadata.frameOfReferenceUID,
    )
    if (seenSops.has(geometry.sop)) {
      throw new SeriesError('series metadata contains a duplicate SOP instance')
    }
    seenSops.add(geometry.sop)
    return geometry
  })

  if (qidoSops.size !== seenSops.size || [...qidoSops].some((sop) => !seenSops.has(sop))) {
    throw new SeriesError('series inventory is incomplete: QIDO and WADO metadata differ')
  }

  const reference = geometries[0]
  for (const geometry of geometries) {
    if (geometry.rows !== reference.rows || geometry.columns !== reference.columns) {
      throw new SeriesError('series dimensions are not consistent across slices')
    }
    if (
      Math.abs(geometry.spacing[0] - reference.spacing[0]) > SPACING_TOLERANCE_MM ||
      Math.abs(geometry.spacing[1] - reference.spacing[1]) > SPACING_TOLERANCE_MM
    ) {
      throw new SeriesError('pixel spacing is not consistent across slices')
    }
    if (geometry.iop.some((value, index) => Math.abs(value - reference.iop[index]) > DIRECTION_TOLERANCE)) {
      throw new SeriesError('image orientation is not consistent across slices')
    }
  }

  const rowDirection: Vec3 = normalize([reference.iop[0], reference.iop[1], reference.iop[2]], 'image orientation row')
  const columnDirection: Vec3 = normalize(
    [reference.iop[3], reference.iop[4], reference.iop[5]],
    'image orientation column',
  )
  const normal = normalize(cross(rowDirection, columnDirection), 'slice normal')

  const slices = geometries
    .map((geometry) => ({
      ...geometry.slice,
      imageId: buildImageId(base, studyUID, seriesUID, geometry.sop),
      positionMM: dot(geometry.ipp, normal),
    }))
    .sort((a, b) => a.positionMM - b.positionMM)

  for (let index = 1; index < slices.length; index += 1) {
    if (slices[index].positionMM - slices[index - 1].positionMM < PLANE_TOLERANCE_MM) {
      throw new SeriesError('two slices share the same plane position within 0.01 mm')
    }
  }

  return {
    slices,
    frameOfReferenceUID: caseMetadata.frameOfReferenceUID,
    rowDirection,
    columnDirection,
    normal,
  }
}

// ImageIds are constructed locally from an application origin and validated
// UIDs; any RetrieveURL/BulkDataURI advertised by upstream is deliberately not
// used as the frame target.
export function buildImageId(origin: string, studyUID: string, seriesUID: string, sopInstanceUID: string): string {
  return `wadors:${origin}/dicom-web/studies/${studyUID}/series/${seriesUID}/instances/${sopInstanceUID}/frames/1`
}
