import type { components } from './schema.gen';

export type ProgressFrame = components['schemas']['EventSessionProgress'];

/** How long a closed turn's frame is kept for a reply the page has not read yet. */
export const REPLY_HOLD_MS = 10_000;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null;

const STATUS_KINDS = ['thinking', 'writing', 'tool', 'waiting'];
const TOOL_STATES = ['running', 'done', 'error'];

function isProgressTool(value: unknown): boolean {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.name === 'string' &&
    typeof value.label === 'string' &&
    typeof value.state === 'string' &&
    TOOL_STATES.includes(value.state)
  );
}

/** Checks every field the live bubble reads; a frame this client does not understand is skipped. */
export function isProgressFrame(value: unknown): value is ProgressFrame {
  return (
    isRecord(value) &&
    typeof value.session_id === 'string' &&
    typeof value.message_id === 'string' &&
    typeof value.seq === 'number' &&
    typeof value.active === 'boolean' &&
    typeof value.started_at === 'string' &&
    typeof value.text === 'string' &&
    typeof value.text_truncated === 'boolean' &&
    (value.reply_id === undefined || typeof value.reply_id === 'string') &&
    Array.isArray(value.tools) &&
    value.tools.every(isProgressTool) &&
    isRecord(value.status) &&
    typeof value.status.label === 'string' &&
    typeof value.status.kind === 'string' &&
    STATUS_KINDS.includes(value.status.kind)
  );
}

/**
 * The frame to keep for a closing frame. The server's closing frame repeats the last text and tool
 * lines; one that carries none (an older server, or a turn that wrote nothing) keeps those of the
 * frame already held for the same message, so the bubble never blanks before the reply is read.
 */
function closing(frame: ProgressFrame, held: ProgressFrame | undefined): ProgressFrame {
  if (held?.message_id !== frame.message_id) return frame;
  const noText = frame.text === '';
  return {
    ...frame,
    text: noText ? held.text : frame.text,
    text_truncated: noText ? held.text_truncated : frame.text_truncated,
    tools: frame.tools.length > 0 ? frame.tools : held.tools
  };
}

type Listener = () => void;

/**
 * The live state of running turns, one entry per session. It is memory only: the server does not
 * store or replay it, and a reconnect clears it because the stream sends the current state again
 * right after `connected`.
 *
 * Each frame is the whole state of a turn, so the frame with the highest `seq` for a message wins
 * and the rest are dropped; a different message id starts a new turn. A closing frame
 * (`active: false`) that names a reply stays for `REPLY_HOLD_MS`, so the bubble can wait until the
 * reply is on the page; one without a reply goes at once.
 */
export function createLiveProgress() {
  const frames = new Map<string, ProgressFrame>();
  const closed = new Map<string, string>(); // session id -> the last message whose turn closed
  const timers = new Map<string, ReturnType<typeof setTimeout>>();
  const listeners = new Set<Listener>();

  const notify = () => listeners.forEach((listener) => listener());

  function forget(sessionId: string) {
    clearTimeout(timers.get(sessionId));
    timers.delete(sessionId);
    if (frames.delete(sessionId)) notify();
  }

  return {
    apply(frame: ProgressFrame): void {
      const held = frames.get(frame.session_id);
      if (held?.message_id === frame.message_id && frame.seq <= held.seq) return;
      if (frame.active) {
        // A late frame of a turn that already closed must not bring its bubble back.
        if (closed.get(frame.session_id) === frame.message_id) return;
      } else {
        closed.set(frame.session_id, frame.message_id);
      }
      clearTimeout(timers.get(frame.session_id));
      timers.delete(frame.session_id);
      if (!frame.active && !frame.reply_id) return forget(frame.session_id);
      frames.set(frame.session_id, frame.active ? frame : closing(frame, held));
      if (!frame.active) {
        timers.set(
          frame.session_id,
          setTimeout(() => forget(frame.session_id), REPLY_HOLD_MS)
        );
      }
      notify();
    },
    /** Drops all live state, for a new connection. */
    reset(): void {
      timers.forEach((timer) => clearTimeout(timer));
      timers.clear();
      closed.clear();
      if (frames.size > 0) {
        frames.clear();
        notify();
      }
    },
    get(sessionId: string | null | undefined): ProgressFrame | undefined {
      return sessionId ? frames.get(sessionId) : undefined;
    },
    subscribe(listener: Listener): () => void {
      listeners.add(listener);
      return () => listeners.delete(listener);
    }
  };
}

export const liveProgress = createLiveProgress();
