import { useNavigate } from '@tanstack/react-router';
import { Search } from 'lucide-react';
import { useLayoutEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Markdown } from '@/components/markdown/markdown';
import { DeliveredContext } from '@/components/shared/DeliveredContext';
import { MEASURE } from '@/components/shared/measure';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { splitDelivered } from '@/lib/delivered';
import {
  itemHas,
  toolArg,
  toolInput,
  transcriptItems,
  type TranscriptEntry,
  type TranscriptItem
} from './model';
import { useSessionTranscript } from './queries';

/**
 * A session's transcript: what its agent did, turn by turn, kept after kagent drops the session.
 * It opens at the end, or at `entry` when a search hit opened it, and pages both ways. Tool calls
 * show one line each and open to their arguments and result. The turn still running, which is not
 * stored yet, follows the stored entries.
 */
export function TranscriptView({ sessionId, entry }: { sessionId: string; entry?: number }) {
  const transcript = useSessionTranscript(sessionId, entry);
  const pages = transcript.data?.pages;
  const last = pages?.[pages.length - 1];
  const stored = useMemo(() => transcriptItems(pages?.flatMap((p) => p.entries) ?? []), [pages]);
  const live = useMemo(() => transcriptItems(last?.live ?? []), [last]);
  const sources = pages?.[0]?.sources ?? [];
  const unavailable = sources.filter((s) => s.state === 'unavailable');

  const scroller = useRef<HTMLDivElement>(null);
  const placed = useRef(false);
  // Once, when the first page arrives: to the hit's entry, or to the end.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (placed.current || !el || !pages) return;
    placed.current = true;
    const hit = entry ? el.querySelector('[data-hit="true"]') : null;
    if (hit) hit.scrollIntoView?.({ block: 'center' });
    else if (!entry) el.scrollTop = el.scrollHeight;
  }, [pages, entry]);

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <SessionSearch sessionId={sessionId} />
      <div
        ref={scroller}
        data-testid="transcript"
        className="px-md py-sm min-h-0 flex-1 overflow-y-auto overscroll-contain font-mono text-sm"
      >
        <div className={cn('gap-sm flex flex-col', MEASURE)}>
          {transcript.isPending ? (
            <p role="status" className="text-muted-foreground font-sans">
              Loading transcript…
            </p>
          ) : transcript.isError && !pages ? (
            <div className="gap-sm flex flex-col items-start font-sans">
              <p role="alert" className="text-destructive">
                {transcript.error.message}
              </p>
              <Button variant="outline" size="sm" onClick={() => void transcript.refetch()}>
                Retry
              </Button>
            </div>
          ) : null}

          {unavailable.map((s) => (
            <p key={s.created_at} role="note" className="text-term-warning font-sans text-xs">
              Part of this transcript could not be copied{s.reason ? `: ${s.reason}` : '.'}
            </p>
          ))}

          {transcript.hasPreviousPage ? (
            <PageButton
              label="Show earlier"
              pending={transcript.isFetchingPreviousPage}
              onClick={() => void transcript.fetchPreviousPage()}
            />
          ) : null}

          {pages && stored.length === 0 && live.length === 0 ? (
            <p className="text-muted-foreground font-sans" data-testid="transcript-empty">
              {sources.length === 0
                ? 'No transcript yet. Turns are copied here as they finish.'
                : 'Nothing stored for this session.'}
            </p>
          ) : null}

          {stored.map((item) => (
            <ItemRow key={item.key} item={item} hit={entry !== undefined && itemHas(item, entry)} />
          ))}

          {transcript.hasNextPage ? (
            <PageButton
              label="Show later"
              pending={transcript.isFetchingNextPage}
              onClick={() => void transcript.fetchNextPage()}
            />
          ) : null}

          {live.length > 0 ? (
            <section aria-label="Not stored yet" className="gap-sm flex flex-col">
              <p className="text-term-fg-muted border-border-subtle pt-sm border-t font-sans text-xs">
                Not stored yet: the turn still running, or just finished
              </p>
              {live.map((item) => (
                <ItemRow key={item.key} item={item} hit={false} />
              ))}
            </section>
          ) : null}

          {last?.live_error ? (
            <p role="note" className="text-term-warning font-sans text-xs">
              {last.live_error}
            </p>
          ) : null}
        </div>
      </div>
    </div>
  );
}

function PageButton({
  label,
  pending,
  onClick
}: {
  label: string;
  pending: boolean;
  onClick: () => void;
}) {
  return (
    <div className="font-sans">
      <Button variant="outline" size="sm" onClick={onClick} disabled={pending}>
        {pending ? 'Loading…' : label}
      </Button>
    </div>
  );
}

/** Searches this session's transcript on the search page, which links back here at each hit. */
function SessionSearch({ sessionId }: { sessionId: string }) {
  const navigate = useNavigate();
  const [q, setQ] = useState('');
  function submit(event: FormEvent) {
    event.preventDefault();
    const query = q.trim();
    if (!query) return;
    void navigate({ to: '/transcripts', search: { q: query, session_id: sessionId } });
  }
  return (
    <form
      role="search"
      onSubmit={submit}
      className="gap-sm px-md py-xs border-border-subtle flex items-center border-b"
    >
      <Search aria-hidden="true" className="text-muted-foreground size-4 shrink-0" />
      <input
        type="search"
        aria-label="Search this transcript"
        placeholder="Search this transcript"
        value={q}
        maxLength={200}
        onChange={(event) => setQ(event.target.value)}
        className="placeholder:text-muted-foreground min-h-control min-w-0 flex-1 bg-transparent text-sm outline-none"
      />
    </form>
  );
}

function Cut({ e }: { e: TranscriptEntry }) {
  if (!e.truncated) return null;
  return (
    <span className="text-term-fg-muted block font-sans text-xs">
      Cut to {e.text.length.toLocaleString()} of {e.orig_bytes.toLocaleString()} bytes when stored
    </span>
  );
}

function ItemRow({ item, hit }: { item: TranscriptItem; hit: boolean }) {
  return (
    <div
      data-hit={hit ? 'true' : undefined}
      data-testid="transcript-item"
      className={cn('min-w-0', hit && 'ring-term-accent ring-2 ring-offset-2')}
    >
      {item.t === 'tool' ? (
        <ToolItem call={item.call} result={item.result} />
      ) : (
        <EntryItem e={item.entry} />
      )}
    </div>
  );
}

function EntryItem({ e }: { e: TranscriptEntry }) {
  switch (e.kind) {
    case 'user': {
      // What Mainloop put in front of the message folds away; the row leads with what was written.
      const { context, message } = splitDelivered(e.text);
      return (
        <div data-testid="transcript-user" className="bg-term-bg-secondary px-sm py-xs">
          {context ? <DeliveredContext text={context} /> : null}
          <div className="[overflow-wrap:anywhere] whitespace-pre-wrap">
            <span aria-hidden="true">› </span>
            {message}
            <Cut e={e} />
          </div>
        </div>
      );
    }
    case 'assistant':
      return (
        <div className="flex min-w-0">
          <span className="w-[2ch] flex-none" aria-hidden="true">
            ●
          </span>
          <div className="min-w-0 flex-1">
            <Markdown>{e.text}</Markdown>
            <Cut e={e} />
          </div>
        </div>
      );
    case 'thinking':
      return (
        <details className="text-term-fg-muted">
          <summary className="cursor-pointer">✻ Thinking</summary>
          <p className="pl-[2ch] [overflow-wrap:anywhere] whitespace-pre-wrap">{e.text}</p>
          <Cut e={e} />
        </details>
      );
    default:
      return (
        <p className="text-term-fg-muted pl-[2ch] text-xs [overflow-wrap:anywhere] whitespace-pre-wrap">
          {e.kind === 'ask' ? 'Asked: ' : ''}
          {e.text}
        </p>
      );
  }
}

/** One tool call: a line with its name and argument, which opens to the call and its result. */
function ToolItem({
  call,
  result
}: {
  call: TranscriptEntry | null;
  result: TranscriptEntry | null;
}) {
  const name = call?.tool_name || result?.tool_name || 'tool';
  const arg = call ? toolArg(name, call.text) : '';
  const input = call ? toolInput(call.text) : null;
  const firstLine = result?.text.split('\n').find((l) => l.trim()) ?? '';
  return (
    <details data-testid="transcript-tool" className="min-w-0">
      <summary className="block min-w-0 cursor-pointer list-none [&::-webkit-details-marker]:hidden">
        <span className="flex min-w-0 gap-[1ch]">
          <span
            aria-hidden="true"
            className={cn(
              'flex-none',
              !result && 'text-term-fg-muted',
              result && !result.is_error && 'text-term-green',
              result?.is_error && 'text-term-error'
            )}
          >
            ●
          </span>
          <span className="flex-none font-bold">{name}</span>
          <span className="text-term-fg-muted min-w-0 truncate">{arg}</span>
          {result?.is_error ? (
            <span className="text-term-error flex-none text-xs">error</span>
          ) : null}
        </span>
        {firstLine ? (
          <span className="text-term-fg-muted block truncate pl-[3ch] text-xs">⎿ {firstLine}</span>
        ) : null}
      </summary>
      <div className="gap-xs mt-xs flex flex-col pl-[3ch] text-xs">
        {call ? (
          <pre className="bg-term-bg-secondary p-xs m-0 [overflow-wrap:anywhere] whitespace-pre-wrap">
            {typeof input === 'string' ? input : JSON.stringify(input, null, 2)}
          </pre>
        ) : null}
        {call ? <Cut e={call} /> : null}
        {result ? (
          <pre
            className={cn(
              'm-0 [overflow-wrap:anywhere] whitespace-pre-wrap',
              result.is_error && 'text-term-error'
            )}
          >
            {result.text || '(no output)'}
          </pre>
        ) : (
          <span className="text-term-fg-muted font-sans">No result stored.</span>
        )}
        {result ? <Cut e={result} /> : null}
      </div>
    </details>
  );
}
