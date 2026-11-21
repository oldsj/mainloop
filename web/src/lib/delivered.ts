/**
 * A message as Mainloop delivered it to a native session. The server may put context in front of
 * what the owner wrote: the main session's standing context and guidance, the earlier conversation
 * carried over to a new session, or a project's changed instructions and memory. That preamble
 * opens with one of the markers below on its first line and ends with a line holding only
 * `[mainloop:new-message]`; the owner's text follows. A carried message cannot start a line of its
 * own, so the first such line is the end of the preamble (server/internal/sessions/carryover.go).
 */
export interface Delivered {
  /** What Mainloop added in front, or null when the message went as written. */
  context: string | null;
  /** What the owner (or the task that sent it) wrote. */
  message: string;
}

const OPENERS = ['[mainloop:context]', '[mainloop:project-context]'];
const END = '\n[mainloop:new-message]\n';

/** Splits a delivered message into the context Mainloop added and the message itself. */
export function splitDelivered(text: string): Delivered {
  if (!OPENERS.some((opener) => text.startsWith(opener))) return { context: null, message: text };
  const at = text.indexOf(END);
  if (at < 0) return { context: null, message: text };
  return { context: text.slice(0, at), message: text.slice(at + END.length) };
}
