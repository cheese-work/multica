// Whether this browser asked for push, kept across reloads. The browser's own
// subscription cannot say: after an opt-out that failed halfway it may still
// exist, and after a dropped renewal it is gone although the user wants push.

export type PushIntent = "on" | "off";

const KEY = "multica_push_device";

export function getPushIntent(): PushIntent | null {
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
  } catch {
    // Storage blocked: the app falls back to the browser's subscription.
  }
}
