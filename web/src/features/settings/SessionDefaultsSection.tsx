import { useId, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { ApiError } from '@/api/errors';
import {
  defaultFor,
  effortChoices,
  effortStale,
  modelChoices,
  modelLabel,
  providerDefaultLabel,
  ROLES,
  selectableProfiles,
  type ProviderProfile,
  type SessionDefault,
  type SessionDefaultRole
} from './model';
import { useProviders, useSessionDefaults, useSetSessionDefault } from './queries';

const FIELD =
  'border-input bg-background text-foreground px-sm min-h-control text-ui w-full min-w-0 border disabled:opacity-50';

/** What a failed save says. The server's own sentence when it sent one. */
function saveError(error: unknown): string {
  if (error instanceof ApiError && error.message) return `Not saved: ${error.message}`;
  return 'Not saved. The default is unchanged.';
}

/**
 * The owner's default model and reasoning effort for new turns, by role and provider. Each choice
 * saves as it is made. "Provider default" sends nothing, so the turn runs on the provider's own.
 */
export function SessionDefaultsSection() {
  const defaults = useSessionDefaults();
  const providers = useProviders();
  const profiles = selectableProfiles(providers.data);

  return (
    <section aria-labelledby="session-defaults-heading" className="gap-md flex flex-col">
      <div className="gap-xs flex flex-col">
        <h2 id="session-defaults-heading" className="text-base font-semibold">
          Session defaults
        </h2>
        <p className="text-muted-foreground max-w-measure text-sm">
          The model and reasoning effort a new turn uses when the session has none of its own. A
          session’s <code>/model</code> or <code>/effort</code>, or a delegation’s choice, wins over
          these. A change reaches running sessions from their next turn.
        </p>
      </div>

      {defaults.isError || providers.isError ? (
        <ErrorRecovery
          title="Could not load the session defaults"
          message="Nothing was changed."
          onRetry={() => {
            void defaults.refetch();
            void providers.refetch();
          }}
        />
      ) : defaults.data === undefined || providers.data === undefined ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading session defaults…
        </p>
      ) : profiles.length === 0 ? (
        <p className="text-muted-foreground text-sm">
          No provider is configured, so there is nothing to set a default for.
        </p>
      ) : (
        ROLES.map((role) => (
          <div
            key={role.id}
            role="group"
            aria-labelledby={`session-defaults-${role.id}`}
            className="gap-sm flex flex-col"
            data-testid={`session-defaults-${role.id}`}
          >
            <div className="gap-sm flex flex-wrap items-baseline">
              <h3 id={`session-defaults-${role.id}`} className="text-sm font-semibold">
                {role.label}
              </h3>
              <span className="text-muted-foreground text-xs">{role.covers}</span>
            </div>
            <div className="border-border-subtle divide-border-subtle divide-y border">
              {profiles.map((profile) => (
                <DefaultRow
                  key={profile.id}
                  role={role.id}
                  roleLabel={role.label}
                  profile={profile}
                  saved={defaultFor(defaults.data.defaults, role.id, profile.id)}
                />
              ))}
            </div>
          </div>
        ))
      )}
    </section>
  );
}

function DefaultRow({
  role,
  roleLabel,
  profile,
  saved
}: {
  role: SessionDefaultRole;
  roleLabel: string;
  profile: ProviderProfile;
  saved: SessionDefault;
}) {
  const id = useId();
  const save = useSetSessionDefault();
  // What the last save said, for this row only. It is cleared when the next one starts.
  const [notice, setNotice] = useState<{ ok: boolean; text: string } | null>(null);
  const model = saved.model ?? null;
  const effort = saved.effort ?? null;

  const set = (next: Pick<SessionDefault, 'model' | 'effort'>) => {
    setNotice(null);
    save.mutate(
      { body: { role, provider: profile.id, model: next.model, effort: next.effort } },
      {
        onSuccess: () => setNotice({ ok: true, text: 'Saved.' }),
        onError: (error) => setNotice({ ok: false, text: saveError(error) })
      }
    );
  };

  return (
    <div
      className="gap-sm p-sm grid grid-cols-1 items-end sm:grid-cols-[minmax(0,10rem)_minmax(0,1fr)_minmax(0,1fr)]"
      data-testid={`session-default-${role}-${profile.id}`}
    >
      <div className="text-sm font-medium sm:pb-1">{profile.display_name}</div>
      <label htmlFor={`${id}-model`} className="gap-xs flex min-w-0 flex-col">
        <span className="text-muted-foreground text-xs">Model</span>
        <select
          id={`${id}-model`}
          aria-label={`${roleLabel}, ${profile.display_name}: model`}
          className={FIELD}
          value={model ?? ''}
          disabled={save.isPending}
          onChange={(event) => set({ model: event.target.value || null, effort })}
        >
          <option value="">
            {providerDefaultLabel(modelLabel(profile, profile.default_model))}
          </option>
          {modelChoices(profile, model).map((choice) => (
            <option key={choice.id} value={choice.id}>
              {choice.label}
            </option>
          ))}
        </select>
      </label>
      <label htmlFor={`${id}-effort`} className="gap-xs flex min-w-0 flex-col">
        <span className="text-muted-foreground text-xs">Effort</span>
        <select
          id={`${id}-effort`}
          aria-label={`${roleLabel}, ${profile.display_name}: effort`}
          className={FIELD}
          value={effort ?? ''}
          disabled={save.isPending}
          onChange={(event) =>
            set({ model, effort: (event.target.value || null) as SessionDefault['effort'] })
          }
        >
          <option value="">{providerDefaultLabel(profile.default_effort)}</option>
          {effortChoices(profile, effort).map((choice) => (
            <option key={choice.id} value={choice.id}>
              {choice.label}
            </option>
          ))}
        </select>
      </label>
      {effortStale(profile, effort) ? (
        <p className="text-term-yellow text-xs sm:col-span-3">
          {profile.display_name} no longer offers “{effort}”, so turns leave the effort to the
          provider until another level is chosen.
        </p>
      ) : null}
      {/* Always present, so a save is announced without the row changing height for it. */}
      <p
        role={notice && !notice.ok ? 'alert' : 'status'}
        className={
          notice && !notice.ok
            ? 'text-destructive min-h-4 text-xs sm:col-span-3'
            : 'text-muted-foreground min-h-4 text-xs sm:col-span-3'
        }
        data-testid={`session-default-${role}-${profile.id}-notice`}
      >
        {save.isPending ? 'Saving…' : (notice?.text ?? '')}
      </p>
    </div>
  );
}
