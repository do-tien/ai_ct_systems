import test from 'node:test'
import assert from 'node:assert/strict'

import { buildSeriesPlan } from '../src/viewer/series.ts'

const STUDY = '1.2.3.4.5'
const SERIES = '1.2.3.4.6'
const FRAME = '1.2.3.4.7'
const CT_IMAGE_STORAGE = '1.2.840.10008.5.1.4.1.1.2'
const ORIGIN = 'https://viewer.local:5173'

const caseMetadata = {
  id: 7,
  source: 'LIDC-IDRI/LIDC-IDRI-0001',
  studyInstanceUID: STUDY,
  seriesInstanceUID: SERIES,
  frameOfReferenceUID: FRAME,
  status: 'awaiting_read' as const,
  createdAt: '2026-10-10T00:00:00Z',
  finalizedAt: null,
}

type Json = Record<string, unknown>

function tag(value: unknown, vr = 'DS', ...rest: unknown[]): Json {
  return { Value: [value, ...rest], vr }
}

function metadataItem(over: Json = {}): Json {
  const base: Json = {
    '00080016': tag(CT_IMAGE_STORAGE, 'UI'),
    '00080018': tag('1.2.3.4.100', 'UI'),
    '00080060': tag('CT', 'CS'),
    '0020000D': tag(STUDY, 'UI'),
    '0020000E': tag(SERIES, 'UI'),
    '00200052': tag(FRAME, 'UI'),
    '00200032': { Value: ['0', '0', '0'], vr: 'DS' },
    '00200037': { Value: ['1', '0', '0', '0', '1', '0'], vr: 'DS' },
    '00280010': { Value: [512], vr: 'US' },
    '00280011': { Value: [512], vr: 'US' },
    '00280030': { Value: ['0.5', '0.5'], vr: 'DS' },
    '00281052': { Value: ['-1024'], vr: 'DS' },
    '00281053': { Value: ['1'], vr: 'DS' },
  }
  return { ...base, ...over }
}

function qidoItem(sop: string, over: Json = {}): Json {
  const base: Json = {
    '00080016': tag(CT_IMAGE_STORAGE, 'UI'),
    '00080018': tag(sop, 'UI'),
    '00080060': tag('CT', 'CS'),
    '0020000D': tag(STUDY, 'UI'),
    '0020000E': tag(SERIES, 'UI'),
  }
  return { ...base, ...over }
}

function ipp(x: string, y: string, z: string): Json {
  return { Value: [x, y, z], vr: 'DS' }
}

test('sortsByProjectedPositionNotInstanceNumber', () => {
  const iop = { Value: ['0', '1', '0', '0', '0', '-1'], vr: 'DS' }
  // normal = normalize(r x c) with r=[0,1,0], c=[0,0,-1] -> (-1,0,0); the
  // projected position is therefore -IPP.x, independent of z and InstanceNumber.
  const slices: Array<[string, string, Json, string]> = [
    ['1.2.3.4.100', '20', ipp('20', '0', '0'), '3'],
    ['1.2.3.4.101', '0', ipp('0', '0', '0'), '1'],
    ['1.2.3.4.102', '10', ipp('10', '0', '0'), '2'],
  ]
  const qido = slices.map(([sop]) => qidoItem(sop))
  const metadata = slices.map(([sop, , position, instance]) =>
    metadataItem({
      '00080018': tag(sop, 'UI'),
      '00200037': iop,
      '00200032': position,
      '00200013': tag(instance, 'IS'),
    }),
  )

  const plan = buildSeriesPlan(qido, metadata, caseMetadata, ORIGIN)

  assert.deepEqual(
    plan.slices.map((slice) => slice.sopInstanceUID),
    ['1.2.3.4.100', '1.2.3.4.102', '1.2.3.4.101'],
  )
  assert.deepEqual(
    plan.slices.map((slice) => slice.positionMM),
    [-20, -10, 0],
  )
  assert.deepEqual(
    plan.slices.map((slice) => slice.imageId),
    [
      `wadors:${ORIGIN}/dicom-web/studies/${STUDY}/series/${SERIES}/instances/1.2.3.4.100/frames/1`,
      `wadors:${ORIGIN}/dicom-web/studies/${STUDY}/series/${SERIES}/instances/1.2.3.4.102/frames/1`,
      `wadors:${ORIGIN}/dicom-web/studies/${STUDY}/series/${SERIES}/instances/1.2.3.4.101/frames/1`,
    ],
  )
  assert.deepEqual(plan.normal, [-1, 0, 0])
})

test('accepts reversedNormalAndNumericStringGeometry', () => {
  const qido = [qidoItem('1.2.3.4.100'), qidoItem('1.2.3.4.101')]
  const metadata = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI'), '00200032': ipp('0', '0', '5.5') }),
    metadataItem({ '00080018': tag('1.2.3.4.101', 'UI'), '00200032': ipp('0', '0', '0') }),
  ]
  const plan = buildSeriesPlan(qido, metadata, caseMetadata, ORIGIN)
  assert.deepEqual(
    plan.slices.map((slice) => slice.positionMM),
    [0, 5.5],
  )
})

test('preservesRescaleAndPixelMetadataForTheLoader', () => {
  const qido = [qidoItem('1.2.3.4.100')]
  const item = metadataItem({
    '00080018': tag('1.2.3.4.100', 'UI'),
    '00280103': { Value: [1], vr: 'US' },
    '00281050': { Value: ['-500', '2000'], vr: 'DS' },
  })
  const plan = buildSeriesPlan(qido, [item], caseMetadata, ORIGIN)
  assert.equal(plan.slices[0].metadata['00280103']['Value'][0], 1)
  assert.deepEqual(plan.slices[0].metadata['00281050']['Value'], ['-500', '2000'])
})

test('rejectsMixedOrMissingFrame', () => {
  const qido = [qidoItem('1.2.3.4.100'), qidoItem('1.2.3.4.101')]
  const mixed = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') }),
    metadataItem({ '00080018': tag('1.2.3.4.101', 'UI'), '00200052': tag('1.2.3.4.99', 'UI') }),
  ]
  assert.throws(() => buildSeriesPlan(qido, mixed, caseMetadata, ORIGIN), /frame/i)

  const missing = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') }),
    metadataItem({ '00080018': tag('1.2.3.4.101', 'UI'), '00200052': { vr: 'UI' } }),
  ]
  assert.throws(() => buildSeriesPlan(qido, missing, caseMetadata, ORIGIN), /frame/i)

  const mismatchedCase = [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') })]
  assert.throws(
    () => buildSeriesPlan([qidoItem('1.2.3.4.100')], mismatchedCase, { ...caseMetadata, frameOfReferenceUID: '1.2.3.9' }, ORIGIN),
    /frame/i,
  )
})

test('rejectsQidoMetadataSetMismatch', () => {
  const qido = [qidoItem('1.2.3.4.100'), qidoItem('1.2.3.4.101')]
  const short = [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') })]
  assert.throws(() => buildSeriesPlan(qido, short, caseMetadata, ORIGIN), /incomplete|inventory|match/i)

  const extra = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') }),
    metadataItem({ '00080018': tag('1.2.3.4.101', 'UI') }),
    metadataItem({ '00080018': tag('1.2.3.4.102', 'UI') }),
  ]
  assert.throws(() => buildSeriesPlan(qido, extra, caseMetadata, ORIGIN), /incomplete|inventory|match/i)
})

test('rejectsDuplicateSopOrPlane', () => {
  const duplicated = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') }),
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI'), '00200032': ipp('0', '0', '2') }),
  ]
  assert.throws(
    () => buildSeriesPlan([qidoItem('1.2.3.4.100')], duplicated, caseMetadata, ORIGIN),
    /duplicate/i,
  )

  const samePlane = [
    metadataItem({ '00080018': tag('1.2.3.4.100', 'UI'), '00200032': ipp('0', '0', '3') }),
    metadataItem({ '00080018': tag('1.2.3.4.101', 'UI'), '00200032': ipp('0', '0', '3.004') }),
  ]
  assert.throws(
    () => buildSeriesPlan([qidoItem('1.2.3.4.100'), qidoItem('1.2.3.4.101')], samePlane, caseMetadata, ORIGIN),
    /plane|position/i,
  )
})

test('rejectsMalformedGeometry', () => {
  const geometry = (over: Json): unknown =>
    buildSeriesPlan([qidoItem('1.2.3.4.100')], [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI'), ...over })], caseMetadata, ORIGIN)

  assert.throws(() => geometry({ '00200037': { Value: ['1', '0', '0', '0', '1'], vr: 'DS' } }), /geometry|orientation/i)
  assert.throws(() => geometry({ '00200037': { Value: ['1', '0', '0', '0', '1', ''], vr: 'DS' } }), /geometry|orientation/i)
  assert.throws(() => geometry({ '00200037': { Value: ['1', '0', '0', '0', '1', '0'], vr: 'DS' }, '00280030': { Value: [], vr: 'DS' } }), /spacing/i)
  assert.throws(() => geometry({ '00280030': { Value: ['0', '0.5'], vr: 'DS' } }), /spacing/i)
  assert.throws(() => geometry({ '00280010': { Value: [0], vr: 'US' } }), /rows|columns|dimension/i)
  assert.throws(() => geometry({ '00280011': { Value: [null], vr: 'US' } }), /rows|columns|dimension/i)
  assert.throws(() => geometry({ '00200032': { Value: ['0', '0', 'NaN'], vr: 'DS' } }), /position/i)
  assert.throws(() => geometry({ '00200032': { Value: ['0', '0'], vr: 'DS' } }), /position/i)
  assert.throws(() => geometry({ '00200037': { Value: ['2', '0', '0', '0', '2', '0'], vr: 'DS' } }), /orientation|unit|orthogonal/i)
  assert.throws(() => geometry({ '00200037': { Value: ['1', '0', '0', '1', '1', '0'], vr: 'DS' } }), /orientation|unit|orthogonal/i)
})

test('rejectsIdentityMismatchOrMultiframe', () => {
  const one = (over: Json): unknown =>
    buildSeriesPlan([qidoItem('1.2.3.4.100')], [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI'), ...over })], caseMetadata, ORIGIN)

  assert.throws(() => one({ '0020000D': tag('1.2.9.9', 'UI') }), /study|identity|series/i)
  assert.throws(() => one({ '0020000E': tag('1.2.9.8', 'UI') }), /series|identity/i)
  assert.throws(() => one({ '00080016': tag('1.2.840.10008.5.1.4.1.1.4', 'UI') }), /sop class|modality|storage/i)
  assert.throws(() => one({ '00080060': tag('MR', 'CS') }), /modality|storage|ct/i)
  assert.throws(() => one({ '00280008': { Value: [2], vr: 'IS' } }), /frame|multiframe|single/i)
  assert.throws(() => one({ '00080018': { vr: 'UI' } }), /sop|identity/i)
  assert.throws(() => one({ '00080018': tag('not-a-uid', 'UI') }), /sop|identity|uid/i)
})

test('rejectsEmptyOrCrossStudyInventory', () => {
  assert.throws(() => buildSeriesPlan([], [], caseMetadata, ORIGIN), /empty|inventory/i)
  assert.throws(
    () => buildSeriesPlan([qidoItem('1.2.3.4.100', { '0020000D': tag('1.2.9.9', 'UI') })], [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') })], caseMetadata, ORIGIN),
    /study|series|identity/i,
  )
})

test('constructsOnlySameOriginFrameTargets', () => {
  const hostile = metadataItem({
    '00080018': tag('1.2.3.4.100', 'UI'),
    '00081190': { Value: ['https://evil.example.org/dicom-web/studies/x/instances/y'], vr: 'UR' },
    '00081192': { Value: ['http://169.254.169.254/latest/meta-data'], vr: 'UR' },
  })
  const plan = buildSeriesPlan([qidoItem('1.2.3.4.100', { '00081190': { Value: ['https://evil.example.org/x'], vr: 'UR' } })], [hostile], caseMetadata, ORIGIN)
  assert.equal(
    plan.slices[0].imageId,
    `wadors:${ORIGIN}/dicom-web/studies/${STUDY}/series/${SERIES}/instances/1.2.3.4.100/frames/1`,
  )
  assert.ok(!plan.slices[0].imageId.includes('evil'), 'hostile retrieve URL must not become an imageId')
  assert.ok(!plan.slices[0].imageId.includes('169.254.169.254'))

  const badUid = metadataItem({ '00080018': tag('1.2.3.4.100/../../etc', 'UI') })
  assert.throws(() => buildSeriesPlan([qidoItem('1.2.3.4.100')], [badUid], caseMetadata, ORIGIN), /sop|identity|uid/i)

  const badOrigin = (): unknown =>
    buildSeriesPlan([qidoItem('1.2.3.4.100')], [metadataItem({ '00080018': tag('1.2.3.4.100', 'UI') })], caseMetadata, 'not a url')
  assert.throws(badOrigin, /origin/i)
})
