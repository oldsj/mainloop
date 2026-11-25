import { useId, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { useProviders, useSupervisorSettings, useUpdateSupervisorSettings } from './queries';
import {
  changesProvider,
  failureText,
  providerName,
  supervisorChanged,
  supervisorChoice,
  supervisorOptions,
  supervisorProfiles,
  supervisorRefusalText,
  supervisorSettingsSchema,
  withProvider,
  type SupervisorChoice,
  type SupervisorSettings
} from './model';

const FIELD =
  'border-input bg-background text-foreground px-sm min-h-control text-field w-full min-w-0 border disabled:opacity-50';

/**
 * The provider, model and effort of the project's standing supervisor. Each has a "default" choice,
 * which sends nothing of the project's own. The three are saved together with one button, since a
 * change of provider restarts the supervisor and must be meant.
 */
export function SupervisorSettingsSection({ projectId }: { projectId: string }) {
  const settings = useSupervisorSettings(projectId);
  const providers = useProviders();
  const update = useUpdateSupervisorSettings(projectId);
  const view =
    settings.data && supervisorSettingsSchema.safeParse(settings.data).success
      ? settings.data
      : undefined;
  // Every profile, for names and for the lists of a provider in force that can no longer be
  // chosen; the picker offers only those that can run a supervisor.
  const known = providers.data ?? [];
  const profiles = supervisorProfiles(known);
  // The answer to a provider change names the handoff it started; the note stays until the next save.
  const restarted = update.data?.operation_id != null;

  return (
    <section
      aria-labelledby="supervisor-settings-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="supervisor-settings"
    >
      <h3 id="supervisor-settings-heading" className="text-title font-semibold">
        Supervisor
      </h3>

      {settings.isPending ? (
        <p role="status" className="text-muted-foreground text-body">
          Loading the supervisor’s settings…
        </p>
      ) : null}

      {settings.isError && !view ? (
        <ErrorRecovery
          title="Could not load the supervisor’s settings."
          message={failureText(settings.error)}
          onRetry={() => void settings.refetch()}
          retryLabel="Try again"
          pending={settings.isFetching}
        />
      ) : null}

      {settings.data && !view ? (
        <ErrorRecovery
          title="Could not load the supervisor’s settings."
          message="Mainloop sent supervisor settings that do not match the contract."
        />
      ) : null}

      {view ? (
        <SettingsForm
          // The form starts again from what is saved whenever that changes.
          key={`${view.provider_profile_id}|${view.model}|${view.effort}`}
          view={view}
          known={known}
          profiles={profiles}
          busy={update.isPending}
          saveError={update.error ? supervisorRefusalText(update.error) : null}
          restarted={restarted}
          onSave={(body) => update.mutate({ params: { path: { project_id: projectId } }, body })}
        />
      ) : null}
    </section>
  );
}

function SettingsForm({
  view,
  known,
  profiles,
  busy,
  saveError,
  restarted,
  onSave
}: {
  view: SupervisorSettings;
  known: ReturnType<typeof supervisorProfiles>;
  profiles: ReturnType<typeof supervisorProfiles>;
  busy: boolean;
  saveError: string | null;
  restarted: boolean;
  onSave: (choice: SupervisorChoice) => void;
}) {
  const id = useId();
  const [draft, setDraft] = useState<SupervisorChoice>(() => supervisorChoice(view));
  const options = supervisorOptions(view, draft, known);
  const inForce = providerName(known, view.effective_provider_profile_id);
  const changed = supervisorChanged(view, draft);
  const moves = changed && changesProvider(view, draft);
  // The project's own provider stays in its picker even if it can no longer run a supervisor.
  const listed = profiles.some((profile) => profile.id === view.provider_profile_id);

  return (
    <form
      className="gap-md flex flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        if (changed && !busy) onSave(draft);
      }}
    >
      <p className="text-body" data-testid="supervisor-settings-summary">
        {view.provider_profile_id === null
          ? `Runs on ${inForce}, the project’s default provider.`
          : `Runs on ${inForce}, this project’s own choice.`}
      </p>

      <div className="gap-sm grid sm:grid-cols-3">
        <label htmlFor={`${id}-provider`} className="gap-xs flex min-w-0 flex-col">
          <span className="text-muted-foreground text-caption">Provider</span>
          <select
            id={`${id}-provider`}
            className={FIELD}
            value={draft.provider_profile_id ?? ''}
            disabled={busy}
            onChange={(event) =>
              setDraft(withProvider(view, draft, event.target.value || null, known))
            }
          >
            <option value="">
              {view.provider_profile_id === null
                ? `Project default (${inForce})`
                : 'Project default'}
            </option>
            {profiles.map((profile) => (
              <option key={profile.id} value={profile.id}>
                {profile.display_name}
              </option>
            ))}
            {view.provider_profile_id !== null && !listed ? (
              <option value={view.provider_profile_id}>
                {providerName(known, view.provider_profile_id)} (cannot run a supervisor)
              </option>
            ) : null}
          </select>
        </label>

        <label htmlFor={`${id}-model`} className="gap-xs flex min-w-0 flex-col">
          <span className="text-muted-foreground text-caption">Model</span>
          <select
            id={`${id}-model`}
            className={FIELD}
            value={draft.model ?? ''}
            disabled={busy || options.models === null}
            onChange={(event) => setDraft({ ...draft, model: event.target.value || null })}
          >
            <option value="">Default</option>
            {(options.models ?? []).map((model) => (
              <option key={model.id} value={model.id}>
                {model.label}
              </option>
            ))}
          </select>
        </label>

        <label htmlFor={`${id}-effort`} className="gap-xs flex min-w-0 flex-col">
          <span className="text-muted-foreground text-caption">Effort</span>
          <select
            id={`${id}-effort`}
            className={FIELD}
            value={draft.effort ?? ''}
            disabled={busy || options.efforts === null}
            onChange={(event) =>
              setDraft({
                ...draft,
                effort: (event.target.value || null) as SupervisorChoice['effort']
              })
            }
          >
            <option value="">Default</option>
            {(options.efforts ?? []).map((level) => (
              <option key={level} value={level}>
                {level}
              </option>
            ))}
          </select>
        </label>
      </div>

      <p className="text-muted-foreground text-body">
        {options.models === null
          ? 'The project default’s models and efforts can be chosen once it is saved as the provider.'
          : 'Default uses your worker session default for the provider, or else the provider’s own.'}
      </p>

      {moves ? (
        <p className="text-term-yellow text-body" data-testid="supervisor-settings-restart-warning">
          Saving this restarts the supervisor on the new provider. Its workers keep running.
        </p>
      ) : null}

      {saveError ? (
        <p role="alert" className="text-destructive text-body">
          {saveError} Nothing was changed.
        </p>
      ) : null}

      {restarted && !changed ? (
        <p role="status" className="text-body" data-testid="supervisor-settings-restarting">
          The supervisor is restarting on {inForce}. Its workers keep running.
        </p>
      ) : null}

      <div className="gap-sm flex flex-wrap items-center">
        <Button type="submit" disabled={!changed || busy}>
          {busy ? 'Saving…' : 'Save'}
        </Button>
        {changed ? (
          <Button
            type="button"
            variant="outline"
            disabled={busy}
            onClick={() => setDraft(supervisorChoice(view))}
          >
            Undo changes
          </Button>
        ) : null}
      </div>
    </form>
  );
}
