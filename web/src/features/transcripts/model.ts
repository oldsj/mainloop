import type { components } from '@/api/schema.gen';
import { argOf, type HistoryMessage } from '@/features/live/store';
import { z } from 'zod';

export type TranscriptEntry = components['schemas']['TranscriptEntry'];
export type SessionTranscript = components['schemas']['SessionTranscript'];
export type TranscriptSearchHit = components['schemas']['TranscriptSearchHit'];

/**
 * One row of the transcript view: an entry, or a tool call with its result. A call and its result
 * pair by the turn and the call id; either may be missing (a result whose call was capped away, or
 * a call still running).
 */
export type TranscriptItem =
  | { t: 'entry'; key: string; entry: TranscriptEntry }
  | { t: 'tool'; key: string; call: TranscriptEntry | null; result: TranscriptEntry | null };

/** A stable key: the stored id, or the turn and position of a live entry, which has no id. */
export function entryKey(e: TranscriptEntry): string {
  return e.id != null ? `e${e.id}` : `live-${e.task_id}-${e.pos}`;
}

/** Pairs each tool call with its result, in order. Entries come oldest first, across pages. */
export function transcriptItems(entries: readonly TranscriptEntry[]): TranscriptItem[] {
  const out: TranscriptItem[] = [];
  const open = new Map<string, number>(); // turn/call id -> index of the call's item
  for (const e of entries) {
    const call = e.tool_call_id ? `${e.task_id}/${e.tool_call_id}` : '';
    if (e.kind === 'tool_call') {
      out.push({ t: 'tool', key: entryKey(e), call: e, result: null });
      if (call) open.set(call, out.length - 1);
    } else if (e.kind === 'tool_result') {
      const at = call ? open.get(call) : undefined;
      const item = at === undefined ? undefined : out[at];
      if (item?.t === 'tool' && item.result === null) {
        item.result = e;
        open.delete(call);
      } else {
        out.push({ t: 'tool', key: entryKey(e), call: null, result: e });
      }
    } else {
      out.push({ t: 'entry', key: entryKey(e), entry: e });
    }
  }
  return out;
}

/** Whether an item shows the stored entry `id`. */
export function itemHas(item: TranscriptItem, id: number): boolean {
  if (item.t === 'entry') return item.entry.id === id;
  return item.call?.id === id || item.result?.id === id;
}

/** A tool call's arguments: stored as compact JSON, or as plain text when it was cut. */
export function toolInput(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** The one-line argument shown after a tool's name, as the live view shows it. */
export function toolArg(name: string, text: string): string {
  const input = toolInput(text);
  const arg = argOf(name, input);
  if (arg) return arg;
  return text.length > 160 ? `${text.slice(0, 160)}…` : text;
}

/**
 * The live view's history from a transcript page: the messages and the tool calls, leaving out
 * thinking, status and the rest, which the live view does not show either.
 */
export function toHistory(entries: readonly TranscriptEntry[]): HistoryMessage[] {
  const out: HistoryMessage[] = [];
  for (const item of transcriptItems(entries)) {
    if (item.t === 'tool') {
      const name = item.call?.tool_name || item.result?.tool_name || 'tool';
      out.push({
        role: 'tool',
        name,
        input: item.call ? toolInput(item.call.text) : {},
        result: item.result ? item.result.text : null,
        error: item.result?.is_error ?? false
      });
    } else if (item.entry.kind === 'user' || item.entry.kind === 'assistant') {
      out.push({ role: item.entry.kind, content: item.entry.text });
    }
  }
  return out;
}

/** The session page's URL: which view, and the entry a search hit opens at. */
export const sessionSearch = z.object({
  transcript: z.coerce.number().pipe(z.literal(1)).optional().catch(undefined),
  entry: z.coerce.number().int().positive().optional().catch(undefined)
});
export type SessionSearch = z.infer<typeof sessionSearch>;

/** The search page's URL. */
export const transcriptSearch = z.object({
  // A query of digits arrives as a number from the URL parser.
  q: z.coerce.string().max(200).optional().catch(undefined),
  session_id: z.string().optional().catch(undefined),
  project_id: z.string().optional().catch(undefined)
});
export type TranscriptSearch = z.infer<typeof transcriptSearch>;
