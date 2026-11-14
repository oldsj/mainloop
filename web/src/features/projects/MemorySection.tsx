import { Link, useLocation } from '@tanstack/react-router';
import { useEffect, useRef } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { relativeTime } from '@/features/sessions/model';
import { useDecideMemory, useProjectMemory } from './queries';
import {
  failureText,
  MEMORY_EXPLANATION,
  memoryLists,
  memoryOrigin,
  projectMemorySchema,
  type ProjectMemoryEntry
} from './model';

type Decision = 'accept' | 'reject' | 'revoke';

/**
 * The project's memory (docs/specs/project-context.md#project-memory): proposals waiting for the
 * owner, then the active entries every new brief carries. The owner accepts or rejects a proposal
 * and revokes an active entry; each decision re-reads the memory, so a refusal shows what is
 * current. The Needs-you review item links here with the `project-memory` hash.
 */
export function MemorySection({ projectId }: { projectId: string }) {
  const memory = useProjectMemory(projectId);
  const decide = useDecideMemory(projectId);
  const hash = useLocation({ select: (location) => location.hash });
  const ref = useRef<HTMLElement>(null);

  const parsed = memory.data ? projectMemorySchema.safeParse(memory.data) : undefined;
  const view = parsed?.success ? parsed.data : undefined;
  const lists = view ? memoryLists(view.entries) : undefined;
  const loaded = lists !== undefined;

  // A link from Needs you lands here once the entries are in, not on the empty section.
  useEffect(() => {
    if (loaded && hash === 'project-memory') ref.current?.scrollIntoView?.({ block: 'start' });
  }, [loaded, hash]);

  const pendingId = decide.isPending ? decide.variables?.params.path.entry_id : undefined;
  const onDecide = (entryId: string, decision: Decision) =>
    decide.mutate({
      params: { path: { project_id: projectId, entry_id: entryId } },
      body: { decision }
    });
  const byId = new Map(lists?.active.map((entry) => [entry.id, entry]));

  return (
    <section
      ref={ref}
      id="project-memory"
      aria-labelledby="project-memory-heading"
      className="gap-md flex flex-col"
      data-testid="project-memory"
    >
      <h2 id="project-memory-heading" className="text-lg font-medium">
        Memory
      </h2>
      <p className="text-muted-foreground text-sm">{MEMORY_EXPLANATION}</p>

      {memory.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading memory…
        </p>
      ) : null}

      {memory.isError && !view ? (
        <ErrorRecovery
          title="Could not load the memory."
          message={failureText(memory.error)}
          onRetry={() => void memory.refetch()}
          retryLabel="Retry"
          pending={memory.isFetching}
        />
      ) : null}

      {parsed && !parsed.success ? (
        <ErrorRecovery
          title="Could not load the memory."
          message="Mainloop sent memory that does not match the contract."
        />
      ) : null}

      {decide.error ? (
        <p role="alert" className="text-destructive text-sm">
          {failureText(decide.error)}
        </p>
      ) : null}

      {view && lists ? (
        <>
          {lists.proposed.length > 0 ? (
            <section
              aria-labelledby="memory-proposals-heading"
              className="gap-sm flex flex-col"
              data-testid="memory-proposals"
            >
              <h3 id="memory-proposals-heading" className="text-sm font-medium">
                Waiting for you ({lists.proposed.length})
              </h3>
              <ul className="gap-sm flex flex-col">
                {lists.proposed.map((entry) => (
                  <MemoryEntry
                    key={entry.id}
                    entry={entry}
                    replaces={entry.replaces_id ? byId.get(entry.replaces_id) : undefined}
                    busy={pendingId !== undefined}
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

          <section
            aria-labelledby="memory-active-heading"
            className="gap-sm flex flex-col"
            data-testid="memory-active"
          >
            <h3 id="memory-active-heading" className="text-sm font-medium">
              Active
            </h3>
            <p className="text-muted-foreground text-xs">
              {view.used_bytes.toLocaleString()} of {view.max_bytes.toLocaleString()} bytes used
            </p>
            {lists.active.length === 0 ? (
              <p className="text-muted-foreground text-sm">No memory yet.</p>
            ) : (
              <ul className="gap-sm flex flex-col">
                {lists.active.map((entry) => (
                  <MemoryEntry
                    key={entry.id}
                    entry={entry}
                    busy={pendingId !== undefined}
                    pending={pendingId === entry.id}
                    actions={[{ decision: 'revoke', label: 'Revoke', variant: 'outline' }]}
                    onDecide={onDecide}
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

interface MemoryEntryProps {
  entry: ProjectMemoryEntry;
  /** The active entry a proposal would replace, when it is still active. */
  replaces?: ProjectMemoryEntry;
  busy: boolean;
  pending: boolean;
  actions: { decision: Decision; label: string; variant: 'default' | 'outline' }[];
  onDecide: (entryId: string, decision: Decision) => void;
}

function MemoryEntry({ entry, replaces, busy, pending, actions, onDecide }: MemoryEntryProps) {
  return (
    <li
      className="gap-xs p-md border-border flex flex-col border"
      data-testid={`memory-entry-${entry.id}`}
    >
      <p className="text-sm break-words">{entry.text}</p>
      {replaces ? (
        <p className="text-muted-foreground text-xs break-words">Replaces: {replaces.text}</p>
      ) : null}
      <p className="gap-sm text-muted-foreground flex flex-wrap items-center text-xs">
        <span>{memoryOrigin(entry, (iso) => relativeTime(iso))}</span>
        <EntrySource entry={entry} />
      </p>
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
      </div>
    </li>
  );
}

/** Where the entry came from: the main thread, the task, or the session that recorded it. */
function EntrySource({ entry }: { entry: ProjectMemoryEntry }) {
  const linkClass = 'text-foreground min-h-control inline-flex items-center underline';
  if (entry.author === 'main') {
    return (
      <Link to="/" className={linkClass}>
        main thread
      </Link>
    );
  }
  if (entry.source.task_id) {
    return (
      <Link to="/tasks/$id" params={{ id: entry.source.task_id }} className={linkClass}>
        the task
      </Link>
    );
  }
  if (entry.source.session_id) {
    return (
      <Link to="/sessions/$id" params={{ id: entry.source.session_id }} className={linkClass}>
        the session
      </Link>
    );
  }
  return null;
}
