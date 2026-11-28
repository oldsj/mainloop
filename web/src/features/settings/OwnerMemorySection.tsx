import { Link, useLocation } from '@tanstack/react-router';
import { useEffect, useId, useRef, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { dotfilesSourceSchema, type DotfilesSource } from '@/features/dotfiles/model';
import { useOwnerDotfiles } from '@/features/dotfiles/queries';
import { failureText, keepValid, projectRowSchema } from '@/features/projects/model';
import { useProjects } from '@/features/projects/queries';
import { relativeTime } from '@/features/sessions/model';
import {
  confirmedAge,
  dateOnly,
  MAX_REVIEW_DAYS,
  MIN_REVIEW_DAYS,
  OWNER_MEMORY_EXPLANATION,
  ownerMemoryLists,
  ownerMemoryOrigin,
  ownerMemorySchema,
  shortCommit,
  type OwnerMemoryDecision,
  type OwnerMemoryEntry
} from './ownerMemory';
import { useDecideOwnerMemory, useOwnerMemory, useSetOwnerMemorySettings } from './queries';

const FIELD =
  'border-input bg-background text-foreground px-sm min-h-control text-field min-w-0 border disabled:opacity-50';
const LINK = 'text-foreground min-h-control inline-flex items-center underline';

/** How long the review window waits after the last keystroke before it saves. */
const REVIEW_SAVE_DELAY_MS = 400;

/** The clock ages and due dates are measured from. It ticks once a minute, so they stay current. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

type Decide = (entryId: string, decision: OwnerMemoryDecision, projectId?: string) => void;

/**
 * The owner's memory (docs/specs/owner-memory.md#the-owner-view): proposals waiting for the owner,
 * the review window, the entries due for review or lapsed, then every loaded entry. Each decision
 * re-reads the memory, so a refusal shows what is current. The Needs-you item links here with the
 * `memory` hash.
 */
export function OwnerMemorySection() {
  const memory = useOwnerMemory();
  const decide = useDecideOwnerMemory();
  const hash = useLocation({ select: (location) => location.hash });
  const ref = useRef<HTMLElement>(null);
  const now = useNow();

  const parsed = memory.data ? ownerMemorySchema.safeParse(memory.data) : undefined;
  const view = parsed?.success ? parsed.data : undefined;
  const lists = view ? ownerMemoryLists(view.entries, now) : undefined;
  const loaded = lists !== undefined;

  // A link from Needs you lands here once the entries are in, not on the empty section.
  useEffect(() => {
    if (loaded && hash === 'memory') ref.current?.scrollIntoView?.({ block: 'start' });
  }, [loaded, hash]);

  const pendingId = decide.isPending ? decide.variables?.params.path.entry_id : undefined;
  const onDecide: Decide = (entryId, decision, projectId) =>
    decide.mutate({
      params: { path: { entry_id: entryId } },
      body: projectId ? { decision, project_id: projectId } : { decision }
    });
  const byId = new Map(lists?.active.map((entry) => [entry.id, entry]));
  const busy = pendingId !== undefined;

  return (
    <section
      ref={ref}
      id="memory"
      aria-labelledby="owner-memory-heading"
      className="gap-md flex flex-col"
      data-testid="owner-memory"
    >
      <div className="gap-xs flex flex-col">
        <h2 id="owner-memory-heading" className="text-title font-semibold">
          Memory
        </h2>
        <p className="text-muted-foreground max-w-measure text-body">{OWNER_MEMORY_EXPLANATION}</p>
      </div>

      {memory.isPending ? (
        <p role="status" className="text-muted-foreground text-body">
          Loading memory…
        </p>
      ) : null}

      {memory.isError && !view ? (
        <ErrorRecovery
          title="Could not load your memory."
          message={failureText(memory.error)}
          onRetry={() => void memory.refetch()}
          retryLabel="Retry"
          pending={memory.isFetching}
        />
      ) : null}

      {parsed && !parsed.success ? (
        <ErrorRecovery
          title="Could not load your memory."
          message="Mainloop sent memory that does not match the contract."
        />
      ) : null}

      {decide.error ? (
        <p role="alert" className="text-destructive text-body">
          {failureText(decide.error)}
        </p>
      ) : null}

      {view && lists ? (
        <>
          {lists.proposed.length > 0 ? (
            <section
              aria-labelledby="owner-memory-proposals-heading"
              className="gap-sm flex flex-col"
              data-testid="owner-memory-proposals"
            >
              <h3 id="owner-memory-proposals-heading" className="text-body font-medium">
                Waiting for you ({lists.proposed.length})
              </h3>
              <ul className="gap-sm flex flex-col">
                {lists.proposed.map((entry) => (
                  <EntryCard
                    key={entry.id}
                    entry={entry}
                    now={now}
                    replaces={entry.replaces_id ? byId.get(entry.replaces_id) : undefined}
                    busy={busy}
                    pending={pendingId === entry.id}
                    actions={[
                      { decision: 'accept', label: 'Accept', variant: 'default' },
                      { decision: 'reject', label: 'Reject', variant: 'outline' }
                    ]}
                    onDecide={onDecide}
                  />
                ))}
              </ul>
            </section>
          ) : null}

          <ReviewWindow saved={view.review_after_days} />

          {lists.review.length > 0 ? (
            <section
              aria-labelledby="owner-memory-review-heading"
              className="gap-sm flex flex-col"
              data-testid="owner-memory-review"
            >
              <h3 id="owner-memory-review-heading" className="text-body font-medium">
                Still true? ({lists.review.length})
              </h3>
              <ul className="gap-sm flex flex-col">
                {lists.review.map((entry) => (
                  <EntryCard
                    key={entry.id}
                    entry={entry}
                    now={now}
                    busy={busy}
                    pending={pendingId === entry.id}
                    actions={[
                      { decision: 'keep', label: 'Keep', variant: 'default' },
                      { decision: 'forget', label: 'Forget', variant: 'outline' }
                    ]}
                    onDecide={onDecide}
                  />
                ))}
              </ul>
            </section>
          ) : null}

          <section
            aria-labelledby="owner-memory-active-heading"
            className="gap-sm flex flex-col"
            data-testid="owner-memory-active"
          >
            <h3 id="owner-memory-active-heading" className="text-body font-medium">
              Active
            </h3>
            <p className="text-muted-foreground text-caption">
              {view.used_bytes.toLocaleString()} of {view.max_bytes.toLocaleString()} bytes used
            </p>
            {lists.active.length === 0 ? (
              <p className="text-muted-foreground text-body">No memory yet.</p>
            ) : (
              <ul className="gap-sm flex flex-col">
                {lists.active.map((entry) => (
                  <EntryCard
                    key={entry.id}
                    entry={entry}
                    now={now}
                    busy={busy}
                    pending={pendingId === entry.id}
                    actions={[{ decision: 'forget', label: 'Forget', variant: 'outline' }]}
                    onDecide={onDecide}
                    moves
                  />
                ))}
              </ul>
            )}
          </section>
        </>
      ) : null}
    </section>
  );
}

/**
 * "Ask me if still true after N days", saved shortly after it changes. Only a whole number in range
 * is sent; a refusal shows the server's reason and the saved value stays.
 */
function ReviewWindow({ saved }: { saved: number }) {
  const save = useSetOwnerMemorySettings();
  const fieldId = useId();
  const hintId = useId();
  const [draft, setDraft] = useState(String(saved));
  const [editing, setEditing] = useState(false);
  const shown = editing ? draft : String(saved);
  const value = /^\d+$/.test(shown) ? Number(shown) : null;
  const valid = value !== null && value >= MIN_REVIEW_DAYS && value <= MAX_REVIEW_DAYS;
  const { mutate } = save;

  useEffect(() => {
    if (!editing || !valid || value === saved) return;
    const timer = setTimeout(() => {
      mutate({ body: { review_after_days: value } }, { onSettled: () => setEditing(false) });
    }, REVIEW_SAVE_DELAY_MS);
    return () => clearTimeout(timer);
  }, [editing, valid, value, saved, mutate]);

  return (
    <div className="gap-xs flex flex-col" data-testid="owner-memory-review-window">
      <div className="gap-sm text-body flex flex-wrap items-center">
        <label htmlFor={fieldId}>Ask me if still true after</label>
        <input
          id={fieldId}
          type="number"
          inputMode="numeric"
          min={MIN_REVIEW_DAYS}
          max={MAX_REVIEW_DAYS}
          step={1}
          value={shown}
          aria-describedby={hintId}
          aria-invalid={!valid}
          onChange={(event) => {
            setEditing(true);
            setDraft(event.target.value);
          }}
          className={`${FIELD} w-24 tabular-nums`}
        />
        <span>days</span>
        {save.isPending ? (
          <span role="status" className="text-muted-foreground text-caption">
            Saving…
          </span>
        ) : null}
      </div>
      <p id={hintId} className={valid ? 'sr-only' : 'text-destructive text-body'}>
        {valid
          ? `From ${MIN_REVIEW_DAYS} to ${MAX_REVIEW_DAYS} days.`
          : `Enter a whole number of days from ${MIN_REVIEW_DAYS} to ${MAX_REVIEW_DAYS}.`}
      </p>
      {save.error ? (
        <p role="alert" className="text-destructive text-body">
          Not saved: {failureText(save.error)}
        </p>
      ) : null}
    </div>
  );
}

interface EntryCardProps {
  entry: OwnerMemoryEntry;
  now: number;
  /** The active entry a proposal would replace, when it is still active. */
  replaces?: OwnerMemoryEntry;
  busy: boolean;
  pending: boolean;
  actions: { decision: OwnerMemoryDecision; label: string; variant: 'default' | 'outline' }[];
  onDecide: Decide;
  /** Offers Move to project and Move to dotfiles. */
  moves?: boolean;
}

type Panel = 'project' | 'dotfiles' | null;

function EntryCard({
  entry,
  now,
  replaces,
  busy,
  pending,
  actions,
  onDecide,
  moves
}: EntryCardProps) {
  const [panel, setPanel] = useState<Panel>(null);
  const age = confirmedAge(entry, now);
  const toggle = (next: Exclude<Panel, null>) => setPanel((open) => (open === next ? null : next));
  return (
    <li
      className="gap-xs p-md border-border flex flex-col border"
      data-testid={`owner-memory-entry-${entry.id}`}
    >
      <p className="text-body break-words">{entry.text}</p>
      {replaces ? (
        <p className="text-muted-foreground text-caption break-words">Replaces: {replaces.text}</p>
      ) : null}
      <p className="gap-sm text-muted-foreground text-caption flex flex-wrap items-center">
        <span>{ownerMemoryOrigin(entry, (iso) => relativeTime(iso, now))}</span>
        <EntrySource entry={entry} />
        {age ? <span>· {age}</span> : null}
      </p>
      {entry.state === 'lapsed' && entry.lapsed_at ? (
        <p className="text-caption font-medium">Not loaded since {dateOnly(entry.lapsed_at)}</p>
      ) : null}
      {entry.in_dotfiles_commit ? (
        <p className="text-caption font-medium" data-testid="owner-memory-in-dotfiles">
          Already in your dotfiles at <code>{shortCommit(entry.in_dotfiles_commit)}</code>; forget
          it here?
        </p>
      ) : null}
      <div className="gap-sm pt-xs flex flex-wrap">
        {actions.map((action) => (
          <Button
            key={action.decision}
            type="button"
            size="sm"
            variant={action.variant}
            disabled={busy}
            aria-label={`${action.label}: ${entry.text}`}
            onClick={() => onDecide(entry.id, action.decision)}
          >
            {pending ? 'Saving…' : action.label}
          </Button>
        ))}
        {moves ? (
          <>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={busy}
              aria-expanded={panel === 'project'}
              aria-label={`Move to project: ${entry.text}`}
              onClick={() => toggle('project')}
            >
              Move to project
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={busy}
              aria-expanded={panel === 'dotfiles'}
              aria-label={`Move to dotfiles: ${entry.text}`}
              onClick={() => toggle('dotfiles')}
            >
              Move to dotfiles
            </Button>
          </>
        ) : null}
      </div>
      {panel === 'project' ? (
        <MoveToProject
          entry={entry}
          busy={busy}
          onMove={(projectId) => onDecide(entry.id, 'move_to_project', projectId)}
          onCancel={() => setPanel(null)}
        />
      ) : null}
      {panel === 'dotfiles' ? (
        <MoveToDotfiles
          entry={entry}
          busy={busy}
          onMoved={() => onDecide(entry.id, 'moved')}
          onCancel={() => setPanel(null)}
        />
      ) : null}
    </li>
  );
}

/** Where the entry came from: main's turn, or the task or session that proposed it. */
function EntrySource({ entry }: { entry: OwnerMemoryEntry }) {
  if (entry.author === 'main') {
    return (
      <Link to="/" className={LINK}>
        from this turn
      </Link>
    );
  }
  if (entry.source.task_id) {
    return (
      <Link to="/tasks/$id" params={{ id: entry.source.task_id }} className={LINK}>
        the task
      </Link>
    );
  }
  if (entry.source.session_id) {
    return (
      <Link to="/sessions/$id" params={{ id: entry.source.session_id }} className={LINK}>
        the session
      </Link>
    );
  }
  return null;
}

interface MoveProps {
  entry: OwnerMemoryEntry;
  busy: boolean;
  onCancel: () => void;
}

/** Picks one of the owner's projects; the server adds the text to its memory and ends this entry. */
function MoveToProject({
  entry,
  busy,
  onMove,
  onCancel
}: MoveProps & { onMove: (id: string) => void }) {
  const projects = useProjects();
  const rows = keepValid(projects.data, projectRowSchema);
  const [choice, setChoice] = useState('');
  const fieldId = useId();
  const selected = rows.some((row) => row.id === choice) ? choice : '';
  return (
    <div
      className="gap-sm pt-sm flex flex-col"
      data-testid={`owner-memory-move-project-${entry.id}`}
    >
      <label htmlFor={fieldId} className="text-body">
        Move to the memory of
      </label>
      {projects.isPending ? (
        <p role="status" className="text-muted-foreground text-body">
          Loading projects…
        </p>
      ) : rows.length === 0 ? (
        <p className="text-muted-foreground text-body">
          {projects.isError ? failureText(projects.error) : 'You have no projects yet.'}
        </p>
      ) : (
        <select
          id={fieldId}
          className={FIELD}
          value={selected}
          disabled={busy}
          onChange={(event) => setChoice(event.target.value)}
        >
          <option value="">Choose a project</option>
          {rows.map((row) => (
            <option key={row.id} value={row.id}>
              {row.full_name}
            </option>
          ))}
        </select>
      )}
      <div className="gap-sm flex flex-wrap">
        <Button
          type="button"
          size="sm"
          disabled={busy || !selected}
          onClick={() => onMove(selected)}
        >
          Move
        </Button>
        <Button type="button" size="sm" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </div>
  );
}

/** Where the owner's dotfiles AGENTS.md lives, as one line. */
function agentsFile(source: DotfilesSource & { repository: string }): string {
  const file = source.path ? `${source.path}/AGENTS.md` : 'AGENTS.md';
  const ref = source.ref ? ` (${source.ref})` : '';
  return `${file} in ${source.repository}${ref}`;
}

/**
 * The text ready to copy into the dotfiles AGENTS.md. Mainloop never writes there; "I've moved it"
 * ends the entry here. With no dotfiles source, it links to the setup instead.
 */
function MoveToDotfiles({ entry, busy, onMoved, onCancel }: MoveProps & { onMoved: () => void }) {
  const dotfiles = useOwnerDotfiles();
  const parsed = dotfiles.data ? dotfilesSourceSchema.safeParse(dotfiles.data) : undefined;
  const source = parsed?.success ? parsed.data : undefined;
  const [copied, setCopied] = useState(false);
  const fieldId = useId();
  const copy = () => {
    void navigator.clipboard?.writeText(entry.text).then(
      () => setCopied(true),
      () => setCopied(false)
    );
  };

  return (
    <div
      className="gap-sm pt-sm flex flex-col"
      data-testid={`owner-memory-move-dotfiles-${entry.id}`}
    >
      {dotfiles.isPending ? (
        <p role="status" className="text-muted-foreground text-body">
          Loading your dotfiles source…
        </p>
      ) : source?.repository ? (
        <>
          <label htmlFor={fieldId} className="text-body">
            Add this line to <code>{agentsFile({ ...source, repository: source.repository })}</code>
            , then tell Mainloop. Mainloop never writes to your dotfiles.
          </label>
          <textarea
            id={fieldId}
            readOnly
            rows={2}
            value={entry.text}
            className={`${FIELD} py-xs w-full`}
            onFocus={(event) => event.currentTarget.select()}
          />
          <div className="gap-sm flex flex-wrap items-center">
            <Button type="button" size="sm" variant="outline" onClick={copy}>
              {copied ? 'Copied' : 'Copy'}
            </Button>
            <Button type="button" size="sm" disabled={busy} onClick={onMoved}>
              I&apos;ve moved it
            </Button>
            <Button type="button" size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </>
      ) : (
        <>
          <p className="text-body">
            No dotfiles repository is set up.{' '}
            <Link to="/settings" hash="dotfiles" className={LINK}>
              Set up agent dotfiles
            </Link>
          </p>
          <div>
            <Button type="button" size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
