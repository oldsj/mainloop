import { useState } from 'react';
import { ApiError } from '@/api';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { failureText, keepValid } from '@/features/projects/model';
import { RegisterForm } from './RegisterForm';
import { SelectionForm, type SelectionBody } from './SelectionForm';
import {
  conflictText,
  environmentRowSchema,
  registerRequest,
  registrationNotice,
  savedLine,
  versionRowSchema,
  type DevEnvironment,
  type EnvironmentVersion,
  type RegisterDraft
} from './model';
import {
  useAcceptEnvironmentDefault,
  useEnvironmentVersions,
  useEnvironments,
  useProjectSelection,
  useRefreshEnvironments,
  useRegisterEnvironment,
  useSelectProjectEnvironment
} from './queries';

/** A save that lost a race gets the refreshed-selection wording, so the owner reviews before saving again. */
function saveFailure(error: unknown): string {
  if (error instanceof ApiError && error.status === 409) return conflictText(error.message);
  return failureText(error);
}

/**
 * The project's environment: the saved selection, the owner's draft, accepting a default, and
 * registering a public image. The requests live here rather than in the form, so a failed write is
 * still shown after the form re-renders, and the registration lock survives refetches.
 */
export function EnvironmentSection({ projectId }: { projectId: string }) {
  const selection = useProjectSelection(projectId);
  const environmentList = useEnvironments();
  const environments = keepValid<DevEnvironment>(environmentList.data, environmentRowSchema);
  const versionQueries = useEnvironmentVersions(environments.map((item) => item.id));
  const versions: Record<string, EnvironmentVersion[]> = {};
  environments.forEach((item, index) => {
    versions[item.id] = keepValid<EnvironmentVersion>(
      versionQueries[index]?.data,
      versionRowSchema
    );
  });

  const select = useSelectProjectEnvironment(projectId);
  const accept = useAcceptEnvironmentDefault();
  const refresh = useRefreshEnvironments();
  const [uncertain, setUncertain] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const register = useRegisterEnvironment({
    onRegistered: (result) => setNotice(registrationNotice(result)),
    onUncertain: () => setUncertain(true)
  });

  const saved = selection.data?.selection ?? null;
  const savedEnvironment = environments.find((item) => item.id === saved?.environment_id);

  const save = (body: SelectionBody) => {
    select.mutate(
      { params: { path: { project_id: projectId } }, body },
      {
        onSuccess: () => setNotice('Environment selection saved for new sessions.')
      }
    );
  };

  const acceptDefault = (environmentId: string, versionId: string) => {
    accept.mutate(
      {
        params: { path: { environment_id: environmentId } },
        body: { version_id: versionId }
      },
      {
        onSuccess: () =>
          setNotice(
            'Accepted default updated. Projects following this environment use it for new sessions.'
          )
      }
    );
  };

  const submitRegistration = (draft: RegisterDraft) => {
    register.mutate({ body: registerRequest(draft) });
  };

  // An uncertain reply already says what to do, so its raw error is not repeated under the form.
  const registerError = register.error && !uncertain ? failureText(register.error) : null;

  return (
    <section
      aria-labelledby="environment-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="project-environment"
    >
      <div className="gap-sm flex flex-wrap items-center justify-between">
        <h2 id="environment-heading" className="text-lg font-medium">
          Environment
        </h2>
        <Button
          type="button"
          variant="outline"
          disabled={selection.isFetching || environmentList.isFetching}
          onClick={() => void refresh()}
        >
          Refresh saved selection and environments
        </Button>
      </div>

      {selection.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading environment…
        </p>
      ) : null}
      {selection.isError && !selection.data ? (
        <ErrorRecovery
          title="Couldn't load the environment"
          message={failureText(selection.error)}
          onRetry={() => void selection.refetch()}
          retryLabel="Retry loading environment"
          pending={selection.isFetching}
        />
      ) : null}
      {selection.isError && selection.data ? (
        <div data-testid="environment-refresh-error" className="gap-sm flex flex-col">
          <ErrorRecovery
            title="Couldn't refresh the environment"
            message={`${failureText(selection.error)} Showing the last saved selection.`}
            onRetry={() => void selection.refetch()}
            retryLabel="Retry loading environment"
            pending={selection.isFetching}
          />
        </div>
      ) : null}

      {saved ? (
        <p className="text-sm">{savedLine(saved, savedEnvironment)}</p>
      ) : selection.data ? (
        <p className="text-muted-foreground text-sm">
          No selection. New sessions use the configured native agent image.
        </p>
      ) : null}

      {environmentList.isError && !environmentList.data ? (
        <ErrorRecovery
          title="Couldn't load environments"
          message={failureText(environmentList.error)}
          onRetry={() => void environmentList.refetch()}
          retryLabel="Retry loading environments"
          pending={environmentList.isFetching}
        />
      ) : null}
      {environmentList.isError && environmentList.data ? (
        <ErrorRecovery
          title="Couldn't refresh environments"
          message={failureText(environmentList.error)}
          onRetry={() => void environmentList.refetch()}
          retryLabel="Retry loading environments"
          pending={environmentList.isFetching}
        />
      ) : null}
      {environmentList.isSuccess && environments.length === 0 ? (
        <p className="text-muted-foreground text-sm">
          No user-owned environments. Register a public image below to begin.
        </p>
      ) : null}

      {selection.data ? (
        <SelectionForm
          saved={saved}
          environments={environments}
          versions={versions}
          saving={select.isPending}
          saveError={select.error ? saveFailure(select.error) : null}
          accepting={accept.isPending}
          acceptError={accept.error ? failureText(accept.error) : null}
          onSave={save}
          onAccept={acceptDefault}
        />
      ) : null}

      {notice ? (
        <p role="status" className="text-sm">
          {notice}
        </p>
      ) : null}

      <RegisterForm
        busy={register.isPending}
        uncertain={uncertain}
        error={registerError}
        onSubmit={submitRegistration}
      />
    </section>
  );
}
