import { Link, useNavigate } from '@tanstack/react-router';
import { Search } from 'lucide-react';
import { useState, type FormEvent } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { PAGE } from '@/components/shared/measure';
import { BACK_LINK, BackIcon, PageHeader } from '@/components/shared/page-header';
import { Button } from '@/components/ui/button';
import { messageTime } from '@/lib/time';
import type { TranscriptSearch, TranscriptSearchHit } from './model';
import { useTranscriptSearch } from './queries';

const KIND_LABEL: Record<string, string> = {
  user: 'Prompt',
  assistant: 'Reply',
  thinking: 'Thinking',
  tool_call: 'Tool call',
  tool_result: 'Tool result',
  status: 'Status',
  ask: 'Question',
  other: 'Other'
};

/**
 * Full-text search over the owner's stored transcripts: "where did we discuss X". The query lives in
 * the URL, so a search can be shared and Back returns to it. Each hit opens its session's
 * transcript at that entry. A session's own search arrives with `session_id` and can be widened.
 */
export function TranscriptSearchPage({ search }: { search: TranscriptSearch }) {
  const navigate = useNavigate();
  const q = search.q ?? '';
  const [draft, setDraft] = useState(q);
  const results = useTranscriptSearch(q, search.session_id, search.project_id);
  const scoped = Boolean(search.session_id || search.project_id);

  function submit(event: FormEvent) {
    event.preventDefault();
    void navigate({
      to: '/transcripts',
      search: { ...search, q: draft.trim() || undefined }
    });
  }

  return (
    <div className={`${PAGE} gap-md`}>
      <PageHeader
        title="Search transcripts"
        description="Every stored turn of every session: prompts, replies, tool calls and their results, with secrets redacted."
        back={
          <Link to="/" aria-label="Back to the main chat" className={BACK_LINK}>
            <BackIcon />
          </Link>
        }
      />
      <form role="search" onSubmit={submit} className="gap-sm flex items-center">
        <div className="gap-sm px-sm border-border focus-within:border-ring flex min-w-0 flex-1 items-center border">
          <Search aria-hidden="true" className="text-muted-foreground size-4 shrink-0" />
          <input
            type="search"
            aria-label="Search transcripts"
            placeholder='Words, "a phrase", or -excluded'
            value={draft}
            maxLength={200}
            onChange={(event) => setDraft(event.target.value)}
            className="placeholder:text-muted-foreground min-h-control min-w-0 flex-1 bg-transparent text-sm outline-none"
          />
        </div>
        <Button type="submit">Search</Button>
      </form>
      {scoped ? (
        <p className="text-muted-foreground text-sm">
          {search.session_id ? 'In one session only. ' : 'In one project only. '}
          <Link
            to="/transcripts"
            search={{ q: q || undefined }}
            className="text-foreground underline"
            data-testid="transcript-search-widen"
          >
            Search everything
          </Link>
        </p>
      ) : null}

      {!q ? null : results.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Searching…
        </p>
      ) : results.isError ? (
        <ErrorRecovery
          title="Search failed"
          message="Mainloop could not run the search. Try again."
          onRetry={() => void results.refetch()}
          pending={results.isFetching}
        />
      ) : results.data.hits.length === 0 ? (
        <p className="text-muted-foreground text-sm" data-testid="transcript-search-empty">
          Nothing found for “{q}”.
        </p>
      ) : (
        <ol className="gap-sm flex flex-col" aria-label="Results">
          {results.data.hits.map((hit) => (
            <HitRow key={hit.entry_id} hit={hit} />
          ))}
        </ol>
      )}
    </div>
  );
}

function HitRow({ hit }: { hit: TranscriptSearchHit }) {
  const kind = KIND_LABEL[hit.kind] ?? hit.kind;
  return (
    <li className="border-border-subtle pb-sm border-b">
      <Link
        to="/sessions/$id"
        params={{ id: hit.session_id }}
        search={{ transcript: 1, entry: hit.entry_id }}
        data-testid="transcript-hit"
        className="gap-xs hover:bg-muted p-xs flex flex-col"
      >
        <span className="gap-sm flex min-w-0 items-baseline">
          <span className="min-w-0 flex-1 truncate text-sm font-medium">
            {hit.session_title || 'Untitled session'}
          </span>
          {hit.at ? (
            <span className="text-muted-foreground flex-none text-xs">{messageTime(hit.at)}</span>
          ) : null}
        </span>
        <span className="text-muted-foreground text-xs">
          {kind}
          {hit.tool_name ? ` · ${hit.tool_name}` : ''}
        </span>
        <span className="text-xs [overflow-wrap:anywhere] whitespace-pre-wrap">
          {hit.snippet.map((part, i) =>
            part.match ? (
              <mark key={i} className="bg-term-selection text-foreground">
                {part.text}
              </mark>
            ) : (
              <span key={i}>{part.text}</span>
            )
          )}
        </span>
      </Link>
    </li>
  );
}
