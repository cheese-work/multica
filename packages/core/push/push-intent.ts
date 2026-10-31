// Whether this browser asked for push, kept across reloads. The browser's own
// subscription cannot say: after an opt-out that failed halfway it may still
// exist, and after a dropped renewal it is gone although the user wants push.

export type PushIntent = "on" | "off";

const KEY = "multica_push_device";

// A choice storage refused to keep. It wins over whatever storage still says
// (a stale "on" would revive a failed opt-out) until a write succeeds again.
let unsaved: PushIntent | null = null;

export function getPushIntent(): PushIntent | null {
  if (unsaved) return unsaved;
  try {
    const value = localStorage.getItem(KEY);
    return value === "on" || value === "off" ? value : null;
  } catch {
    return null;
  }
}

export function setPushIntent(intent: PushIntent): void {
  try {
    localStorage.setItem(KEY, intent);
    unsaved = null;
  } catch {
    unsaved = intent;
  }
}
