import { useState, type ReactNode } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { ExecutionList } from './ExecutionList';
import {
  agentText,
  createRequestIds,
  draftFromConfig,
  messageOf,
  statusOf,
  type ScheduleConfigInput,
  type ScheduleView
} from './model';
import { ScheduleCard } from './ScheduleCard';
import { ScheduleForm } from './ScheduleForm';
import {
  updateArgs,
  useDeleteSchedule,
  useSchedule,
  useTriggerSchedule,
  useUpdateSchedule
} from './queries';

/**
 * One schedule: its summary, the quick actions (pause, run now, delete), the edit form and its
 * executions. Every write sends the etag it was read at. A 412 means someone else saved first: the
 * form keeps the user's edits and shows the newer saved values beside them.
 */
export function ScheduleDetailPage({ id }: { id: string }) {
  const navigate = useNavigate();
  const schedule = useSchedule(id);
  const update = useUpdateSchedule(id);
  const trigger = useTriggerSchedule(id);
  const remove = useDeleteSchedule(id);
  const [triggerIds] = useState(() => createRequestIds());
  const [formKey, setFormKey] = useState(0);
  const [conflict, setConflict] = useState<'changed' | 'deleted' | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const saved = schedule.data;

  if (schedule.isPending) return <p className="p-md text-sm">Loading schedule…</p>;

  if (statusOf(schedule.error) === 404) {
    return (
      <div className="gap-md p-md flex flex-col">
        <p className="text-sm font-medium">This schedule no longer exists.</p>
        <Link
          to="/schedules"
          className="text-foreground inline-flex min-h-11 items-center underline"
        >
          Back to scheduled runs
        </Link>
      </div>
    );
  }

  if (!saved) {
    return (
      <div className="p-md">
        <ErrorRecovery
          title="Could not load the schedule"
          message={messageOf(schedule.error)}
          onRetry={() => schedule.refetch()}
          pending={schedule.isFetching}
        />
      </div>
    );
  }

  const locked = conflict === 'deleted';
  const busy = update.isPending;
  const paused = saved.config.paused;

  // Pause and resume send the saved configuration with one field flipped, so unsaved form edits are not written.
  function writeFailed(error: unknown, show: (text: string) => void) {
    const status = statusOf(error);
    if (status === 412) {
      setConflict('changed');
      return;
    }
    if (status === 409 || status === 404) {
      setConflict('deleted');
      return;
    }
    show(`Not saved. ${messageOf(error)}`);
  }

  const togglePause = () => {
    setActionError(null);
    setMessage(null);
    update.mutate(updateArgs(id, saved.etag, { ...configOf(saved), paused: !paused }), {
      onSuccess: () => {
        setConflict(null);
        setMessage(paused ? 'Resumed. The next run is scheduled.' : 'Paused. No runs will start.');
      },
      onError: (error) => writeFailed(error, setActionError)
    });
  };

  const save = (config: ScheduleConfigInput) => {
    setSaveError(null);
    setMessage(null);
    update.mutate(updateArgs(id, saved.etag, config), {
      onSuccess: () => {
        setConflict(null);
        setMessage('Saved.');
      },
      onError: (error) => writeFailed(error, setSaveError)
    });
  };

  function runNow() {
    setActionError(null);
    setMessage(null);
    trigger.mutate(
      { params: { path: { schedule_id: id } }, body: { request_id: triggerIds.idFor('trigger') } },
      {
        onSuccess: () => {
          triggerIds.reset();
          setMessage('Run requested. It appears under Executions.');
        },
        onError: (error) => {
          if (statusOf(error) === 409) triggerIds.reset();
          setActionError(`Run not started. ${messageOf(error)}`);
        }
      }
    );
  }

  function deleteSchedule() {
    setActionError(null);
    remove.mutate(
      { params: { path: { schedule_id: id } } },
      {
        onSuccess: () => navigate({ to: '/schedules' }),
        onError: (error) => {
          setConfirmDelete(false);
          setActionError(`Not deleted. ${messageOf(error)}`);
        }
      }
    );
  }

  function discardEdits() {
    setConflict(null);
    setSaveError(null);
    setFormKey((key) => key + 1);
  }

  const conflictNotice: ReactNode =
    conflict === 'changed' ? (
      <div role="alert" className="border-destructive p-md gap-sm flex min-w-0 flex-col border">
        <p className="text-destructive font-medium">Changed elsewhere</p>
        <p className="text-muted-foreground break-words">
          Someone saved this schedule after you opened it. Your edits are still in the form. Saving
          now replaces the newer version.
        </p>
        <SavedValues config={saved.config} />
        <div>
          <Button type="button" variant="outline" onClick={discardEdits}>
            Discard my edits and use the saved version
          </Button>
        </div>
      </div>
    ) : conflict === 'deleted' ? (
      <div role="alert" className="border-destructive p-md gap-sm flex min-w-0 flex-col border">
        <p className="text-destructive font-medium">Deleted elsewhere</p>
        <p className="text-muted-foreground break-words">
          This schedule was deleted after you opened it, so your edits cannot be saved.
        </p>
        <Link
          to="/schedules"
          className="text-foreground inline-flex min-h-11 items-center underline"
        >
          Back to scheduled runs
        </Link>
      </div>
    ) : null;

  return (
    <div className="gap-lg p-md flex min-w-0 flex-col">
      <header className="gap-sm flex flex-col">
        <p className="text-muted-foreground text-sm">
          <Link to="/schedules" className="text-foreground underline">
            Scheduled runs
          </Link>
        </p>
        <h1 className="text-xl font-medium">Schedule</h1>
      </header>

      <div className="border-border p-md min-w-0 border">
        <ScheduleCard
          name={saved.config.name}
          schedule={saved.config.schedule}
          timeZone={saved.config.time_zone}
          agent={agentText(saved.agent)}
          paused={paused}
          nextRun={saved.next_execution_time ?? null}
        />
      </div>

      <div className="gap-sm flex flex-wrap">
        <Button type="button" variant="outline" disabled={busy || locked} onClick={togglePause}>
          {paused ? 'Resume' : 'Pause'}
        </Button>
        <Button type="button" disabled={busy || locked || trigger.isPending} onClick={runNow}>
          {trigger.isPending ? 'Starting…' : 'Run now'}
        </Button>
        <Button
          type="button"
          variant="destructive"
          disabled={locked || remove.isPending}
          onClick={() => setConfirmDelete(true)}
        >
          Delete
        </Button>
      </div>

      {message ? (
        <p role="status" className="text-foreground text-sm break-words">
          {message}
        </p>
      ) : null}
      {actionError ? (
        <p role="alert" className="text-destructive text-sm break-words">
          {actionError}
        </p>
      ) : null}

      <section aria-labelledby="edit-heading" className="gap-md flex min-w-0 flex-col">
        <h2 id="edit-heading" className="text-base font-medium">
          Edit
        </h2>
        <ScheduleForm
          key={formKey}
          mode="edit"
          initial={draftFromConfig(saved.config)}
          agentLabel={agentText(saved.agent)}
          busy={busy}
          locked={locked}
          notice={conflictNotice}
          error={saveError}
          onSubmit={(config) => save(config)}
        />
      </section>

      <ExecutionList scheduleId={id} />

      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title="Delete this schedule?"
        description="Its future runs stop. Past executions and their sessions stay. This cannot be undone."
        confirmLabel="Delete"
        destructive
        pending={remove.isPending}
        onConfirm={deleteSchedule}
      />
    </div>
  );
}

/** The saved values, shown beside a conflict so the user can compare them with their edits. */
function SavedValues({ config }: { config: ScheduleView['config'] }) {
  return (
    <div className="gap-xs flex min-w-0 flex-col text-sm">
      <p className="font-medium">Latest saved values</p>
      <dl className="gap-x-md gap-y-xs grid grid-cols-[auto_1fr] text-xs">
        <dt className="text-muted-foreground">Name</dt>
        <dd className="break-words">{config.name || '—'}</dd>
        <dt className="text-muted-foreground">Schedule</dt>
        <dd className="font-mono break-all">{config.schedule}</dd>
        <dt className="text-muted-foreground">Time zone</dt>
        <dd className="break-all">{config.time_zone}</dd>
        <dt className="text-muted-foreground">Timeout (s)</dt>
        <dd>{config.execution_timeout_seconds}</dd>
        <dt className="text-muted-foreground">State</dt>
        <dd>{config.paused ? 'Paused' : 'Active'}</dd>
      </dl>
      <details>
        <summary className="text-foreground inline-flex min-h-11 cursor-pointer items-center text-xs underline">
          Latest saved prompt
        </summary>
        <pre className="text-xs break-words whitespace-pre-wrap">{config.prompt}</pre>
      </details>
    </div>
  );
}

/** The saved configuration with every field kept, for a write that changes only one of them. */
function configOf(view: ScheduleView): ScheduleConfigInput {
  const { name, schedule, time_zone, prompt, paused, execution_timeout_seconds } = view.config;
  return { name, schedule, time_zone, prompt, paused, execution_timeout_seconds };
}
