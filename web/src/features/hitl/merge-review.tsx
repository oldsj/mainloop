import { useRef, useState, type ReactNode } from 'react';
import { Button } from '@/components/ui/button';
import type {
  HITLMergeDetailSection,
  HITLMergeDetails,
  MergeDetailItem,
  VerifiedMergeContext
} from '@/lib/contract';
import { messageTime } from '@/lib/time';
import {
  compareLinkFor,
  evidenceKey,
  isReviewable,
  prLinkFor,
  safeExternalHref,
  type SummarizedFacts
} from './evidence';

export type LoadMergeDetails = (
  proposalId: string,
  section: HITLMergeDetailSection,
  cursor: number
) => Promise<HITLMergeDetails>;

const text = (v: unknown, fallback = ''): string =>
  typeof v === 'string' ? v : typeof v === 'number' ? String(v) : fallback;

const when = (iso: string | null | undefined): string =>
  (iso && messageTime(iso)) || iso || 'time unavailable';

const link = 'text-primary inline-flex min-h-11 items-center underline underline-offset-4';

interface PagedState {
  loaded: boolean;
  loading: boolean;
  error: string | null;
  /** The cursor to retry after an error; 0 is the first page. */
  retryCursor: number;
  description: string;
  items: MergeDetailItem[];
  nextCursor: number | null;
  truncated: boolean;
}

const EMPTY: PagedState = {
  loaded: false,
  loading: false,
  error: null,
  retryCursor: 0,
  description: '',
  items: [],
  nextCursor: null,
  truncated: false
};

/** A cursor the server may hand back: a whole number past the one just read, or no more pages. */
function nextCursorOf(page: HITLMergeDetails, cursor: number): number | null {
  if (page.next_cursor === null) return null;
  const next = Number(page.next_cursor);
  return Number.isSafeInteger(next) && next > cursor ? next : null;
}

/**
 * One lazy section. It loads when first opened, then pages on request. A page that does not
 * name the proposal, section, cursor and summary digest the reviewer is looking at is refused,
 * so details of another summary can never sit under this one.
 */
function DetailSection({
  title,
  section,
  facts,
  load,
  moreLabel,
  prompt,
  children
}: {
  title: string;
  section: HITLMergeDetailSection;
  facts: VerifiedMergeContext;
  load: LoadMergeDetails;
  moreLabel: string;
  prompt: string;
  children: (state: PagedState) => ReactNode;
}) {
  const [state, setState] = useState<PagedState>(EMPTY);
  // A ref, not state: two quick toggles must not both start a read.
  const inFlight = useRef(false);
  const proposalId = facts.proposal_id;
  const digest = facts.summary_digest;

  async function fetchPage(cursor: number) {
    if (inFlight.current || !proposalId) return;
    inFlight.current = true;
    setState((s) => ({ ...s, loading: true, error: null }));
    try {
      const page = await load(proposalId, section, cursor);
      if (
        page.proposal_id !== proposalId ||
        page.section !== section ||
        page.cursor !== cursor ||
        page.summary_digest !== digest
      )
        throw new Error(
          'The server returned details for a different proposal or summary. Refresh this request.'
        );
      setState((s) => ({
        loaded: true,
        loading: false,
        error: null,
        retryCursor: 0,
        description: page.description ?? s.description,
        items: cursor > 0 ? [...s.items, ...(page.items ?? [])] : (page.items ?? []),
        nextCursor: nextCursorOf(page, cursor),
        truncated: page.truncated
      }));
    } catch (error) {
      setState((s) => ({
        ...s,
        loading: false,
        error: error instanceof Error ? error.message : 'Could not load these details.',
        retryCursor: cursor
      }));
    } finally {
      inFlight.current = false;
    }
  }

  return (
    <details
      className="border-border border-t"
      onToggle={(event) => {
        if (event.currentTarget.open && !state.loaded) void fetchPage(state.retryCursor);
      }}
    >
      <summary className="py-sm min-h-11 cursor-pointer text-sm">{title}</summary>
      <div className="gap-sm pb-md grid text-sm">
        {state.loading && !state.loaded ? <p role="status">Loading…</p> : null}
        {state.error ? (
          <div className="gap-sm grid">
            <p role="alert" className="text-destructive">
              {state.error}
            </p>
            <div>
              <Button variant="outline" onClick={() => void fetchPage(state.retryCursor)}>
                Try again
              </Button>
            </div>
          </div>
        ) : null}
        {state.loaded ? (
          <>
            {children(state)}
            {state.nextCursor !== null ? (
              <div>
                <Button
                  variant="outline"
                  disabled={state.loading}
                  onClick={() => void fetchPage(state.nextCursor ?? 0)}
                >
                  {moreLabel}
                </Button>
              </div>
            ) : null}
          </>
        ) : !state.loading && !state.error ? (
          <p className="text-muted-foreground">{prompt}</p>
        ) : null}
      </div>
    </details>
  );
}

function FileItem({ file }: { file: MergeDetailItem }) {
  return (
    <li className="gap-xs grid">
      <code>{text(file.filename, 'Unknown path')}</code>
      <span>
        {text(file.status, 'changed')} · +{text(file.additions, '0')} / −{text(file.deletions, '0')}
      </span>
      {typeof file.previous_filename === 'string' ? (
        <span>
          Renamed from <code>{file.previous_filename}</code>
        </span>
      ) : null}
    </li>
  );
}

function CheckItem({ item }: { item: MergeDetailItem }) {
  const href = safeExternalHref(item.html_url ?? item.target_url);
  return (
    <li className="gap-xs grid">
      <span>
        <strong>{text(item.kind, 'CI result')}:</strong>{' '}
        {text(item.name) || text(item.context) || `#${text(item.id)}`} ·{' '}
        {text(item.conclusion) || text(item.state) || text(item.status) || 'unknown'}
        {item.app_id !== undefined && item.app_id !== null ? ` · app ${text(item.app_id)}` : ''}
      </span>
      {href ? (
        <a className={link} href={href} target="_blank" rel="noopener noreferrer">
          Open result
        </a>
      ) : null}
    </li>
  );
}

/** The evidence for one merge call the server verified. Approve is decided by the caller. */
export function VerifiedMerge({
  facts,
  conversation,
  load
}: {
  facts: SummarizedFacts;
  conversation: ReactNode;
  load: LoadMergeDetails;
}) {
  const { summary } = facts;
  const prUrl = prLinkFor(facts.pr_url, summary.repository, summary.pr_number);
  const compareUrl = compareLinkFor(facts.compare_url, summary.repository);
  const withheld = !isReviewable(facts);
  // When withheld, the notice leads with one reason and the list below must not repeat it. Otherwise
  // nothing leads, so every reason is listed.
  const lead = withheld ? (facts.freshness_reason ?? summary.unavailable_reasons[0]) : undefined;
  const otherReasons = summary.unavailable_reasons.filter((reason) => reason !== lead);
  const key = evidenceKey(facts);
  return (
    <section
      aria-label="Verified merge context"
      className="gap-sm border-border p-md grid rounded-none border text-sm"
    >
      <h4 className="font-medium">{summary.title || 'PR title unavailable'}</h4>
      {summary.description_excerpt ? (
        <p className="text-muted-foreground">
          PR description excerpt: {summary.description_excerpt}
          {summary.description_truncated ? ' … (description truncated)' : ''}
        </p>
      ) : null}
      <p>
        <strong>
          {summary.repository ?? 'Unknown repository'} #{summary.pr_number ?? '?'}
        </strong>{' '}
        · {summary.file_count ?? 'unknown'} files · +{summary.additions ?? '?'} / −
        {summary.deletions ?? '?'}
        {summary.paths_preview.length
          ? ` · including ${summary.paths_preview.join(', ')}${summary.paths_preview_truncated ? ', …' : ''}`
          : ''}
      </p>
      <p className="font-medium">Approval required because:</p>
      {(summary.approval_reasons.length ? summary.approval_reasons : [null]).map((reason, i) => (
        <p key={i} className="font-medium">
          {reason === null
            ? 'An owner decision is required for this proposal.'
            : reason.type === 'project_policy'
              ? 'Project policy requires approval.'
              : `Protected path matched ${reason.glob}: ${reason.path}.`}
        </p>
      ))}
      {withheld ? (
        <p role="status" className="text-term-yellow">
          {lead ?? 'Approval context is unavailable. Refresh this request.'} Approve is disabled;
          Reject stays available.
        </p>
      ) : null}
      {otherReasons.map((reason, i) => (
        <p key={i} className="text-muted-foreground">
          {reason}
        </p>
      ))}
      {facts.deadline ? (
        <p className="text-muted-foreground">Evaluation deadline: {when(facts.deadline)}</p>
      ) : null}
      <p>
        Head: {summary.head} · <code>{summary.head_sha}</code>
      </p>
      <p>
        Base: {summary.base} · <code>{summary.base_sha}</code>
      </p>
      <p className="text-muted-foreground">
        Proposal <code>{facts.proposal_id}</code> · project policy v{summary.policy_version} ·
        protected globs v{summary.globs_version}
      </p>
      {facts.state ? <p className="text-muted-foreground">Merge state: {facts.state}</p> : null}
      <p className="text-muted-foreground">
        Reviewed summary SHA-256: <code>{facts.summary_digest}</code>
      </p>
      {summary.ci?.complete ? (
        <p>
          Historical CI evidence, not current status: {summary.ci.result_count} results recorded at{' '}
          {when(summary.ci.captured_at)} · {summary.ci.passed_count} passed,{' '}
          {summary.ci.failed_count} failed, {summary.ci.pending_count} pending at preparation.
        </p>
      ) : (
        <p className="text-muted-foreground">Recorded CI evidence is unavailable or incomplete.</p>
      )}
      <nav aria-label="Pull request links" className="gap-x-md flex flex-wrap">
        {prUrl ? (
          <a className={link} href={prUrl} target="_blank" rel="noopener noreferrer">
            Open PR
          </a>
        ) : null}
        {compareUrl ? (
          <a className={link} href={compareUrl} target="_blank" rel="noopener noreferrer">
            Compare diff
          </a>
        ) : null}
        {conversation}
      </nav>
      <DetailSection
        key={`${key}:description`}
        title="PR description from GitHub"
        section="description"
        facts={facts}
        load={load}
        moreLabel="Load more"
        prompt="Expand to load the captured PR description."
      >
        {(state) => (
          <>
            <pre className="bg-muted p-sm max-h-80 overflow-auto break-words whitespace-pre-wrap">
              {state.description}
            </pre>
            {state.truncated ? (
              <p className="text-muted-foreground">The saved description is truncated.</p>
            ) : null}
          </>
        )}
      </DetailSection>
      <DetailSection
        key={`${key}:files`}
        title="Changed paths"
        section="files"
        facts={facts}
        load={load}
        moreLabel="Load more paths"
        prompt="Expand to load captured paths."
      >
        {(state) => (
          <>
            <ul className="gap-sm pl-md grid list-disc">
              {state.items.map((file, i) => (
                <FileItem key={i} file={file} />
              ))}
            </ul>
            {state.truncated ? (
              <p className="text-muted-foreground">The captured path inventory is incomplete.</p>
            ) : null}
          </>
        )}
      </DetailSection>
      <DetailSection
        key={`${key}:checks`}
        title="Full CI list"
        section="checks"
        facts={facts}
        load={load}
        moreLabel="Load more CI results"
        prompt="Expand to load the complete captured CI list."
      >
        {(state) => (
          <>
            <ul className="gap-sm pl-md grid list-disc">
              {state.items.map((item, i) => (
                <CheckItem key={i} item={item} />
              ))}
            </ul>
            {state.truncated ? (
              <p className="text-muted-foreground">The captured CI inventory is incomplete.</p>
            ) : null}
          </>
        )}
      </DetailSection>
      <p className="text-muted-foreground">
        This is the captured proposal context, not a claim that the merge is safe or complete. The
        server checks current evidence again when you respond and when the merge runs.
      </p>
    </section>
  );
}

/**
 * Shown when a merge call has no usable facts. Nothing here can enable an approval. `reason` is this
 * page's own check; `notes` are what the server said, and only those are labelled as its notes.
 */
export function UnavailableMerge({
  toolId,
  notes,
  reason = null
}: {
  toolId: string;
  notes: string[];
  reason?: string | null;
}) {
  return (
    <section
      aria-label="Merge context unavailable"
      className="gap-sm border-term-yellow p-md grid rounded-none border text-sm"
    >
      <h4 className="font-medium">Merge evidence unavailable · call {toolId}</h4>
      <p>
        The server did not send merge evidence this page can verify. Approve is disabled; Reject
        stays available. Refresh status to check again.
      </p>
      {reason ? <p className="text-muted-foreground">{reason}</p> : null}
      {notes.map((n, i) => (
        <p key={i} className="text-muted-foreground">
          Server note: {n}
        </p>
      ))}
      <p className="text-muted-foreground">
        This page also withholds evidence whose commit SHAs, digests or repository name are not in
        the exact expected form (40 or 64 hex digits, owner/name). That check is stricter than the
        API schema, so valid-looking evidence can be withheld.
      </p>
    </section>
  );
}
