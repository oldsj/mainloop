import { useQueryClient, type QueryKey } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useState } from 'react';
import { keys, TaskApiError } from '@/api';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import type { TaskLoop } from '@/lib/contract';
import { newRequestId } from '@/lib/taskActions';
import { messageTime } from '@/lib/time';
import {
  LOOP_REFUSALS,
  loopLive,
  loopStateLabel,
  loopTrigger,
  sendLoopControl,
  useMainLoops,
  useTaskLoops,
  type LoopControl
} from './loops';

const PANEL = 'gap-sm p-md border-border-subtle flex flex-col border-t';
const HEADING = 'text-muted-foreground text-2xs font-medium tracking-wide uppercase';

/**
 * The task's loops: those watching it and those its supervisor armed, live ones first. Each shows
 * what it waits for, whom it wakes, its firings and its latest messages, with the owner's controls.
 * The section is left out while the task has none.
 */
export function TaskLoops({ taskId, offline }: { taskId: string; offline: boolean }) {
  const loops = useTaskLoops(taskId);
  if (loops.data === undefined || loops.data.length === 0) {
    return loops.isError ? (
      <p role="status" className="px-md pt-sm text-muted-foreground text-xs">
        Loops could not be loaded.
      </p>
    ) : null;
  }
  return (
    <section aria-labelledby="loops-heading" data-testid="task-loops" className={PANEL}>
      <h2 id="loops-heading" className={HEADING}>
        Loops
      </h2>
      {loops.isError ? (
        <p role="status" className="text-muted-foreground text-xs">
          Could not refresh the loops. Showing the last known state.
        </p>
      ) : null}
      <LoopList
        loops={loops.data}
        queryKey={keys.taskLoops(taskId)}
        pageTaskId={taskId}
        offline={offline}
      />
    </section>
  );
}

/**
 * Main's loops, with or without a task, for the main thread's view: main arms the most loops, and
 * one with no task appears on no task page. Nothing is drawn while main has none.
 */
export function MainLoops({ offline }: { offline: boolean }) {
  const loops = useMainLoops();
  if (loops.data === undefined || loops.data.length === 0) {
    return loops.isError ? (
      <p role="status" className="px-md py-xs text-muted-foreground text-xs">
        Main's loops could not be loaded.
      </p>
    ) : null;
  }
  return (
    <section aria-labelledby="main-loops-heading" data-testid="main-loops" className={PANEL}>
      <h2 id="main-loops-heading" className={HEADING}>
        Main's loops
      </h2>
      {loops.isError ? (
        <p role="status" className="text-muted-foreground text-xs">
          Could not refresh the loops. Showing the last known state.
        </p>
      ) : null}
      <LoopList
        loops={loops.data}
        queryKey={keys.mainLoops()}
        pageTaskId={null}
        offline={offline}
      />
    </section>
  );
}

/** How many of main's loops are live, for the main thread's header. */
export function useLiveMainLoopCount(): number {
  return (useMainLoops().data ?? []).filter(loopLive).length;
}

/**
 * Loops with the owner's controls. `pageTaskId` is the task whose page this is, or null on the
 * main thread, where a loop's task is linked instead.
 */
function LoopList({
  loops,
  queryKey,
  pageTaskId,
  offline
}: {
  loops: TaskLoop[];
  queryKey: QueryKey;
  pageTaskId: string | null;
  offline: boolean;
}) {
  return (
    <ul className="gap-md flex flex-col">
      {loops.map((loop) => (
        <LoopItem
          key={loop.id}
          loop={loop}
          queryKey={queryKey}
          pageTaskId={pageTaskId}
          offline={offline}
        />
      ))}
    </ul>
  );
}

function LoopItem({
  loop,
  queryKey,
  pageTaskId,
  offline
}: {
  loop: TaskLoop;
  queryKey: QueryKey;
  pageTaskId: string | null;
  offline: boolean;
}) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState<LoopControl | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // Fire's request id is kept until the server answers, so a resend does not fire twice.
  const [fireId, setFireId] = useState<string | null>(null);
  const live = loopLive(loop);

  async function control(kind: LoopControl) {
    const requestId = kind === 'fire' ? (fireId ?? newRequestId()) : '';
    if (kind === 'fire') setFireId(requestId);
    setBusy(kind);
    setError(null);
    try {
      const updated = await sendLoopControl(loop.id, kind, requestId);
      if (kind === 'fire') setFireId(null);
      queryClient.setQueryData<TaskLoop[]>(queryKey, (list) =>
        list?.map((l) => (l.id === updated.id ? updated : l))
      );
    } catch (e) {
      if (e instanceof TaskApiError) {
        // A refusal recorded nothing: the next fire is a new request.
        if (kind === 'fire') setFireId(null);
        setError((e.reason && LOOP_REFUSALS[e.reason]) ?? e.message);
      } else {
        setError(
          kind === 'fire'
            ? 'Mainloop did not confirm. Fire again to check; it will not fire twice.'
            : 'Mainloop did not confirm. Try again.'
        );
      }
    } finally {
      setBusy(null);
      setConfirmDelete(false);
      void queryClient.invalidateQueries({ queryKey });
    }
  }

  const facts = [
    pageTaskId === null
      ? null
      : loop.recipient === 'main'
        ? 'wakes main'
        : loop.recipient_task_id === pageTaskId
          ? 'wakes this task'
          : 'wakes its supervisor',
    `${loop.firings} of ${loop.max_firings} firings`,
    loop.next_fire_at ? `next ${messageTime(loop.next_fire_at)}` : null,
    loop.expect_change ? 'expects change' : null,
    live ? `expires ${messageTime(loop.expires_at)}` : null
  ].filter((f): f is string => f !== null);
  const disabled = offline || busy !== null;

  return (
    <li className="gap-xs flex flex-col text-sm" data-testid="task-loop">
      <div className="gap-sm flex flex-wrap items-center">
        <p className="font-medium break-words">{loopTrigger(loop, pageTaskId)}</p>
        <StatusBadge
          label={loopStateLabel(loop)}
          tone={loop.state === 'armed' ? 'active' : loop.stop_reason ? 'attention' : 'muted'}
        />
      </div>
      <p className="text-muted-foreground text-xs">
        {facts.join(' · ')}
        {pageTaskId === null && loop.task_id ? (
          <>
            {' · '}
            <Link to="/tasks/$id" params={{ id: loop.task_id }} className="underline">
              open its task
            </Link>
          </>
        ) : null}
      </p>
      <p className="break-words whitespace-pre-wrap">{loop.note}</p>
      {loop.stop_reason === 'no_progress' ? (
        <p className="text-muted-foreground text-xs">
          Three checks in a row found the task unchanged, so the loop stopped and asked you to
          decide.
        </p>
      ) : null}
      {loop.recent_firings.length > 0 ? (
        <details>
          <summary className="text-muted-foreground min-h-control cursor-pointer text-xs">
            Latest firings
          </summary>
          <ul className="gap-sm pt-xs flex flex-col">
            {loop.recent_firings.map((f) => (
              <li key={f.number} className="text-xs">
                <p className="text-muted-foreground">
                  #{f.number} · {messageTime(f.created_at)} · {f.delivery_state}
                </p>
                <ul className="list-disc pl-4">
                  {f.facts.map((fact, i) => (
                    <li key={i} className="break-words">
                      {fact}
                    </li>
                  ))}
                </ul>
                {f.folded > 0 ? (
                  <p className="text-muted-foreground">{f.folded} more folded in</p>
                ) : null}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {live ? (
        <div className="gap-sm flex flex-wrap items-center">
          {loop.state === 'armed' ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={disabled}
              onClick={() => void control('pause')}
            >
              Pause
            </Button>
          ) : (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={disabled}
              onClick={() => void control('resume')}
            >
              Resume
            </Button>
          )}
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={disabled}
            onClick={() => void control('fire')}
          >
            Fire now
          </Button>
          {confirmDelete ? (
            <>
              <Button
                type="button"
                variant="destructive"
                size="sm"
                disabled={disabled}
                onClick={() => void control('delete')}
              >
                Delete loop
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={busy !== null}
                onClick={() => setConfirmDelete(false)}
              >
                Keep
              </Button>
            </>
          ) : (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={disabled}
              onClick={() => setConfirmDelete(true)}
            >
              Delete
            </Button>
          )}
        </div>
      ) : null}
      {error ? (
        <p role="alert" className="text-xs">
          {error}
        </p>
      ) : null}
    </li>
  );
}
