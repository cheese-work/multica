/** Time a response of `bytes` takes on a link with `net.rttMs` and `net.downBytesPerSec`. */
export const responseDelayMs = (net, bytes) => net.rttMs + (bytes / net.downBytesPerSec) * 1000;

/** True when a document took less time than its bytes need at the profile bandwidth. */
export const documentFasterThanLink = (net, bytes, transferMs) =>
  transferMs < (bytes / net.downBytesPerSec) * 1000;
