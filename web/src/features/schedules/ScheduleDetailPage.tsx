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

type Config = ScheduleView['config'];

/** A saved version of the schedule: the etag a write names, and the configuration it holds. */
interface Version {
  etag: string;
  config: Config;
}

const versionOf = (view: ScheduleView): Version => ({ etag: view.etag, config: view.config });

/**
 * One schedule: its summary, the quick actions (pause, run now, delete), the edit form and its
 * executions. Writes go against the version the page holds, not the newest read, so a refetch never
 * lets an old draft overwrite a newer save. A 412 means someone else saved first: the form keeps the
 * user's edits and shows the newer saved values beside them.
 */
export function ScheduleDetailPage({ id }: { id: string }) {
  const schedule = useSchedule(id);

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

  if (!schedule.data) {
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

  return <ScheduleEditor id={id} latest={schedule.data} />;
}

/**
 * The loaded page. It mounts once the schedule has loaded, so its first version is the one the form
 * opens at. `latest` is the newest read and moves with every refetch; `base` does not.
 */
function ScheduleEditor({ id, latest }: { id: string; latest: ScheduleView }) {
  const navigate = useNavigate();
  const update = useUpdateSchedule(id);
  const trigger = useTriggerSchedule(id);
  const remove = useDeleteSchedule(id);
  const [triggerIds] = useState(() => createRequestIds());
  const [base, setBase] = useState<Version>(() => versionOf(latest));
  const [formKey, setFormKey] = useState(0);
  const [conflict, setConflict] = useState<'changed' | 'deleted' | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const locked = conflict === 'deleted';
  // Pause and resume wait while a conflict shows. They would accept the newer version under edits made to the old one.
  const holding = conflict === 'changed';
  const busy = update.isPending;
  // The label reads the newest read, as the card badge does, so the two cannot disagree. The write
  // still goes to `base`'s etag, so a change made elsewhere is a conflict, not a silent overwrite.
  const paused = latest.config.paused;

  // A write that succeeded is the version the page holds from now on.
  function accept(view: ScheduleView) {
    setBase(versionOf(view));
    setConflict(null);
  }

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

  // Pause and resume write the held version with only the paused flag flipped, so unsaved edits are not written.
  const togglePause = () => {
    setActionError(null);
    setMessage(null);
    update.mutate(updateArgs(id, base.etag, { ...configOf(base.config), paused: !paused }), {
      onSuccess: (view) => {
        accept(view);
        setMessage(
          view.config.paused ? 'Paused. No runs will start.' : 'Resumed. The next run is scheduled.'
        );
      },
      onError: (error) => writeFailed(error, setActionError)
    });
  };

  const save = (config: ScheduleConfigInput) => {
    // After a conflict, saving replaces the newer version that the notice showed, so it names that version.
    const target = conflict === 'changed' ? versionOf(latest) : base;
    setSaveError(null);
    setMessage(null);
    update.mutate(updateArgs(id, target.etag, { ...config, paused: target.config.paused }), {
      onSuccess: (view) => {
        accept(view);
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
    setBase(versionOf(latest));
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
          now replaces the newer version, and pause and resume wait until you save or discard.
        </p>
        <SavedValues config={latest.config} />
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
          name={latest.config.name}
          schedule={latest.config.schedule}
          timeZone={latest.config.time_zone}
          agent={agentText(latest.agent)}
          paused={latest.config.paused}
          nextRun={latest.next_execution_time ?? null}
        />
      </div>

      <div className="gap-sm flex flex-wrap">
        <Button
          type="button"
          variant="outline"
          disabled={busy || locked || holding}
          onClick={togglePause}
        >
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
          initial={draftFromConfig(base.config)}
          agentLabel={agentText(latest.agent)}
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
function SavedValues({ config }: { config: Config }) {
  return (
    <div className="gap-xs flex min-w-0 flex-col text-sm">
      <p className="font-medium">Latest saved values</p>
      <dl className="gap-x-md gap-y-xs grid grid-cols-[auto_1fr] text-xs">
        <dt className="text-muted-foreground">Name</dt>
        <dd className="min-w-0 [overflow-wrap:anywhere]">{config.name || '—'}</dd>
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

/** The configuration with every field kept, for a write that changes only one of them. */
function configOf(config: Config): ScheduleConfigInput {
  const { name, schedule, time_zone, prompt, paused, execution_timeout_seconds } = config;
  return { name, schedule, time_zone, prompt, paused, execution_timeout_seconds };
}
