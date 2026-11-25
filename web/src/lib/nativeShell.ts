/**
 * The iPhone app's shell (docs/specs/ios-app.md): the app shows this web app in a web view and keeps
 * native only what has to be. It exposes one message handler, `mainloop`, that answers each message
 * (WebKit's `postMessage` returns a promise). The page asks; it never holds a key, the web session's
 * token (an HttpOnly cookie) or an approval signature.
 *
 * - `approve`: the app reads the action's canonical record itself, shows its own approval sheet,
 *   asks for Touch ID and sends the signed answer. The page only names the card.
 * - `sessionExpired`: the app starts a new web session and reloads the page.
 * - `openDeviceSettings`: the app's own This iPhone screen.
 */

interface MessageHandler {
  postMessage(message: unknown): Promise<unknown> | undefined;
}

interface WebKitWindow {
  webkit?: { messageHandlers?: { mainloop?: MessageHandler } };
}

function handler(): MessageHandler | null {
  if (typeof window === 'undefined') return null;
  const h = (window as WebKitWindow).webkit?.messageHandlers?.mainloop;
  return h && typeof h.postMessage === 'function' ? h : null;
}

/** True when the page runs inside the iPhone app. */
export function inNativeShell(): boolean {
  return handler() !== null;
}

export type NativeApproval =
  | { status: 'approved' }
  | { status: 'cancelled' }
  | { status: 'failed'; message: string };

const FAILED = "The iPhone app didn't answer. Nothing was approved.";

/**
 * Asks the app to approve a privileged-action card: the app shows its own sheet from the record it
 * hashed, and sends the answer itself after Touch ID. The reply is checked here, at the boundary:
 * anything but a known status reads as a failure, so nothing is shown as approved by mistake.
 */
export async function approveInNativeShell(card: {
  itemId: string;
  actionId: string;
  digest: string;
}): Promise<NativeApproval> {
  const h = handler();
  if (!h) return { status: 'failed', message: FAILED };
  let reply: unknown;
  try {
    reply = await h.postMessage({
      type: 'approve',
      item_id: card.itemId,
      action_id: card.actionId,
      digest: card.digest
    });
  } catch (error) {
    return {
      status: 'failed',
      message: error instanceof Error && error.message ? error.message : FAILED
    };
  }
  if (typeof reply !== 'object' || reply === null) return { status: 'failed', message: FAILED };
  const { status, message } = reply as { status?: unknown; message?: unknown };
  if (status === 'approved' || status === 'cancelled') return { status };
  return { status: 'failed', message: typeof message === 'string' && message ? message : FAILED };
}

/** Tells the app the web session lapsed (a 401): it starts a new one and reloads the page. */
export function sessionExpiredInNativeShell(): void {
  void handler()
    ?.postMessage({ type: 'sessionExpired' })
    ?.catch(() => undefined);
}

/** Opens the app's own This iPhone screen (the device, its approval key, notifications, Sign out). */
export function openNativeDeviceSettings(): void {
  void handler()
    ?.postMessage({ type: 'openDeviceSettings' })
    ?.catch(() => undefined);
}
