export { LiveSession, LiveSessionView, type LiveBar } from './LiveSession';
export { liveUrl, useLiveSession, LIVE_SUBPROTOCOL } from './useLiveSession';

/** Local override: `?live=1` shows the live view whatever the session's `live` flag says. */
export function liveForced(): boolean {
  try {
    return new URLSearchParams(window.location.search).get('live') === '1';
  } catch {
    return false;
  }
}
