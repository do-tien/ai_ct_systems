// Application-scoped Cornerstone initialization. One decoder worker and one
// global tool registration for the whole browser session: React StrictMode
// mounts share this promise and cleanup never tears shared state down.

import { init as coreInit } from '@cornerstonejs/core'
import { LengthTool, StackScrollTool, WindowLevelTool, ZoomTool, addTool, init as toolsInit } from '@cornerstonejs/tools'
import { init as loaderInit } from '@cornerstonejs/dicom-image-loader'

// Frame transfers are bounded on the client as well as by the proxy budget.
export const FRAME_REQUEST_TIMEOUT_MS = 120000

let initialization: Promise<void> | null = null

export function initializeCornerstone(): Promise<void> {
  if (initialization === null) {
    initialization = Promise.resolve().then(() => {
      coreInit()
      toolsInit()
      loaderInit({
        maxWebWorkers: 1,
        beforeSend: (xhr) => {
          xhr.timeout = FRAME_REQUEST_TIMEOUT_MS
        },
      })
      addTool(WindowLevelTool)
      addTool(ZoomTool)
      addTool(StackScrollTool)
      // Registered for LC1-15 measurement; only enabled as a rated mode when an
      // editable initial read is present.
      addTool(LengthTool)
    })
  }
  return initialization
}
