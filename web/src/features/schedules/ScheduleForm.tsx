import { useState, type FormEvent, type ReactNode } from 'react';
import { Button } from '@/components/ui/button';
import {
  describeCron,
  PROMPT_MAX_BYTES,
  timeZoneOptions,
  validateDraft,
  type AgentOption,
  type DraftErrors,
  type ScheduleConfigInput,
  type ScheduleDraft
} from './model';

const CONTROL =
  'border-border bg-background text-foreground px-md min-h-11 w-full min-w-0 border text-sm aria-[invalid=true]:border-destructive';

export interface ScheduleFormProps {
  mode: 'create' | 'edit';
  /** The starting values. The form keeps its own draft after this, so a refetch never replaces it. */
  initial: ScheduleDraft;
  /** Create only: the Agents the schedule can run on. */
  agents?: AgentOption[];
  agentsLoading?: boolean;
  /** Edit only: the agent the schedule runs on, shown read-only because it cannot change. */
  agentLabel?: string;
  busy: boolean;
  /** Locks every field, for example after the schedule was deleted elsewhere. */
  locked?: boolean;
  /** A message above the buttons, such as a conflict with a newer saved version. */
  notice?: ReactNode;
  error?: string | null;
  onSubmit: (config: ScheduleConfigInput, draft: ScheduleDraft) => void;
  onCancel?: () => void;
}

/**
 * Create and edit share one form. Limits come from `validateDraft`, which the contract's types
 * check. Fields show their problem once the user has tried to save, except the cron line, which is
 * live because it is the one thing a reader cannot judge by eye.
 */
export function ScheduleForm({
  mode,
  initial,
  agents = [],
  agentsLoading = false,
  agentLabel,
  busy,
  locked = false,
  notice,
  error,
  onSubmit,
  onCancel
}: ScheduleFormProps) {
  const [draft, setDraft] = useState<ScheduleDraft>(initial);
  const [attempted, setAttempted] = useState(false);
  const [zones] = useState(timeZoneOptions);

  const set = <K extends keyof ScheduleDraft>(key: K, value: ScheduleDraft[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  const result = validateDraft(draft);
  const errors: DraftErrors = result.ok ? {} : result.errors;
  const cron = describeCron(draft.schedule);
  const promptBytes = new TextEncoder().encode(draft.prompt).length;
  const disabled = busy || locked;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttempted(true);
    if (result.ok && (mode === 'edit' || draft.agent)) onSubmit(result.config, draft);
  }

  // A field's problem shows after the first save attempt; the cron line is live.
  const shown = (field: keyof DraftErrors) => (attempted ? errors[field] : undefined);

  return (
    <form className="gap-lg flex min-w-0 flex-col" noValidate onSubmit={submit}>
      <fieldset disabled={disabled} className="gap-lg flex min-w-0 flex-col">
        {mode === 'edit' && agentLabel ? (
          <p className="gap-xs flex flex-col text-sm">
            <span className="font-medium">Agent</span>
            <span className="text-muted-foreground break-all">{agentLabel}</span>
            <span className="text-muted-foreground text-xs">
              An agent cannot change after the schedule is created.
            </span>
          </p>
        ) : null}

        {mode === 'create' ? (
          <Field
            id="schedule-agent"
            label="Agent"
            hint={
              !agentsLoading && agents.length === 0
                ? 'No enabled agents. Enable a provider first.'
                : undefined
            }
            error={
              attempted && !draft.agent ? 'Choose the agent this schedule runs on.' : undefined
            }
          >
            {(describedBy) => (
              <select
                id="schedule-agent"
                required
                className={CONTROL}
                aria-describedby={describedBy}
                aria-invalid={attempted && !draft.agent ? true : undefined}
                value={draft.agent}
                onChange={(event) => set('agent', event.target.value)}
              >
                <option value="">{agentsLoading ? 'Loading agents…' : 'Choose an agent'}</option>
                {agents.map((agent) => (
                  <option key={agent.value} value={agent.value}>
                    {agent.label}
                  </option>
                ))}
              </select>
            )}
          </Field>
        ) : null}

        <Field id="schedule-name" label="Name" error={shown('name')}>
          {(describedBy) => (
            <input
              id="schedule-name"
              className={CONTROL}
              maxLength={200}
              placeholder="Nightly triage"
              aria-describedby={describedBy}
              aria-invalid={shown('name') ? true : undefined}
              value={draft.name}
              onChange={(event) => set('name', event.target.value)}
            />
          )}
        </Field>

        <Field
          id="schedule-cron"
          label="Schedule (five fields)"
          hint={
            <span className={cron.ok ? 'text-muted-foreground' : 'text-destructive'}>
              {cron.ok ? cron.text : cron.message}
            </span>
          }
          error={cron.ok ? shown('schedule') : undefined}
        >
          {(describedBy) => (
            <input
              id="schedule-cron"
              required
              className={`${CONTROL} font-mono`}
              maxLength={256}
              spellCheck={false}
              autoComplete="off"
              placeholder="0 2 * * *"
              aria-describedby={describedBy}
              aria-invalid={!cron.ok || shown('schedule') ? true : undefined}
              value={draft.schedule}
              onChange={(event) => set('schedule', event.target.value)}
            />
          )}
        </Field>

        <Field
          id="schedule-zone"
          label="Time zone"
          hint="Blank means UTC. Starts as this browser's zone."
          error={shown('time_zone')}
        >
          {(describedBy) => (
            <>
              <input
                id="schedule-zone"
                list="schedule-zone-options"
                className={`${CONTROL} font-mono`}
                autoComplete="off"
                spellCheck={false}
                placeholder="UTC"
                aria-describedby={describedBy}
                aria-invalid={shown('time_zone') ? true : undefined}
                value={draft.timeZone}
                onChange={(event) => set('timeZone', event.target.value)}
              />
              <datalist id="schedule-zone-options">
                {zones.map((zone) => (
                  <option key={zone} value={zone} />
                ))}
              </datalist>
            </>
          )}
        </Field>

        <Field
          id="schedule-prompt"
          label="Prompt"
          hint={`Sent to the agent on each run. ${promptBytes} of ${PROMPT_MAX_BYTES} bytes used.`}
          error={shown('prompt')}
        >
          {(describedBy) => (
            <textarea
              id="schedule-prompt"
              required
              rows={6}
              maxLength={PROMPT_MAX_BYTES}
              className={`${CONTROL} py-sm min-h-32`}
              aria-describedby={describedBy}
              aria-invalid={shown('prompt') ? true : undefined}
              value={draft.prompt}
              onChange={(event) => set('prompt', event.target.value)}
            />
          )}
        </Field>

        <Field
          id="schedule-timeout"
          label="Execution timeout (seconds)"
          hint="Includes queueing and provisioning. Default 900 (15 minutes)."
          error={shown('execution_timeout_seconds')}
        >
          {(describedBy) => (
            <input
              id="schedule-timeout"
              type="number"
              inputMode="decimal"
              step="any"
              min="0"
              className={CONTROL}
              aria-describedby={describedBy}
              aria-invalid={shown('execution_timeout_seconds') ? true : undefined}
              value={draft.timeoutSeconds}
              onChange={(event) => set('timeoutSeconds', event.target.value)}
            />
          )}
        </Field>

        {mode === 'create' ? (
          <label className="gap-sm px-md border-border flex min-h-11 cursor-pointer items-center border text-sm">
            <input
              type="checkbox"
              className="size-4"
              checked={draft.paused}
              onChange={(event) => set('paused', event.target.checked)}
            />
            Start paused
          </label>
        ) : null}
      </fieldset>

      {notice ? <div className="gap-sm flex flex-col text-sm">{notice}</div> : null}
      {error ? (
        <p role="alert" className="text-destructive text-sm break-words">
          {error}
        </p>
      ) : null}

      <div className="gap-sm flex flex-wrap">
        <Button type="submit" disabled={disabled}>
          {busy ? 'Saving…' : mode === 'create' ? 'Create schedule' : 'Save changes'}
        </Button>
        {onCancel ? (
          <Button type="button" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        ) : null}
      </div>
    </form>
  );
}

/** A labelled control with its hint and error linked to it, so a screen reader reads all three. */
function Field({
  id,
  label,
  hint,
  error,
  children
}: {
  id: string;
  label: string;
  hint?: ReactNode;
  error?: string;
  children: (describedBy: string | undefined) => ReactNode;
}) {
  const describedBy =
    [hint ? `${id}-hint` : null, error ? `${id}-error` : null].filter(Boolean).join(' ') ||
    undefined;
  return (
    <div className="gap-xs flex min-w-0 flex-col">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children(describedBy)}
      {hint ? (
        <p id={`${id}-hint`} className="text-muted-foreground text-xs break-words">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={`${id}-error`} className="text-destructive text-xs break-words">
          {error}
        </p>
      ) : null}
    </div>
  );
}
