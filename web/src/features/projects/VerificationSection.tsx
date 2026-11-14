import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { useUpdateVerification, useVerification } from './queries';
import {
  failureText,
  projectVerificationSchema,
  VERIFICATION_EXPLANATION,
  VERIFICATION_LABEL,
  verificationText,
  type VerificationMode
} from './model';

const MODES: VerificationMode[] = ['auto', 'ci', 'hybrid', 'local'];

/**
 * Where the project's heavy checks run. Automatic follows the CI Mainloop detected on the default
 * branch; the owner can pin CI, hybrid or the workspace instead.
 */
export function VerificationSection({ projectId }: { projectId: string }) {
  const verification = useVerification(projectId);
  const update = useUpdateVerification(projectId);
  const parsed = verification.data
    ? projectVerificationSchema.safeParse(verification.data)
    : undefined;
  const view = parsed?.success ? parsed.data : undefined;

  return (
    <section
      aria-labelledby="verification-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="verification"
    >
      <h3 id="verification-heading" className="text-lg font-medium">
        Checks
      </h3>

      {verification.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading checks setting…
        </p>
      ) : null}

      {verification.isError ? (
        <ErrorRecovery
          title="Could not load the checks setting."
          message={failureText(verification.error)}
          onRetry={() => void verification.refetch()}
          retryLabel="Retry"
          pending={verification.isFetching}
        />
      ) : null}

      {parsed && !parsed.success ? (
        <ErrorRecovery
          title="Could not load the checks setting."
          message="Mainloop sent a checks setting that does not match the contract."
        />
      ) : null}

      {view ? (
        <VerificationForm
          key={view.mode}
          mode={view.mode}
          summary={verificationText(view)}
          explanation={VERIFICATION_EXPLANATION[view.effective_mode]}
          detectError={view.detect_error ?? null}
          busy={update.isPending}
          saveError={update.error ? failureText(update.error) : null}
          onSave={(mode) =>
            update.mutate({ params: { path: { project_id: projectId } }, body: { mode } })
          }
        />
      ) : null}
    </section>
  );
}

interface VerificationFormProps {
  mode: VerificationMode;
  summary: string;
  explanation: string;
  detectError: string | null;
  busy: boolean;
  saveError: string | null;
  onSave: (mode: VerificationMode) => void;
}

function VerificationForm({
  mode,
  summary,
  explanation,
  detectError,
  busy,
  saveError,
  onSave
}: VerificationFormProps) {
  const [draft, setDraft] = useState<VerificationMode>(mode);
  return (
    <div className="gap-md flex flex-col">
      <p className="text-sm">{summary}</p>
      <p className="text-muted-foreground text-sm">{explanation}</p>
      {detectError ? (
        <p className="text-term-yellow text-sm">
          Could not re-check the project's CI: {detectError}
        </p>
      ) : null}
      <form
        className="gap-md flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (draft !== mode && !busy) onSave(draft);
        }}
      >
        <label className="gap-xs flex flex-col text-sm">
          Heavy checks run in
          <select
            aria-label="Heavy checks run in"
            className="border-border bg-background text-foreground px-md min-h-control border text-sm"
            value={draft}
            disabled={busy}
            onChange={(event) => setDraft(event.target.value as VerificationMode)}
          >
            {MODES.map((m) => (
              <option key={m} value={m}>
                {VERIFICATION_LABEL[m]}
              </option>
            ))}
          </select>
        </label>
        {saveError ? (
          <p role="alert" className="text-destructive text-sm">
            {saveError}
          </p>
        ) : null}
        <Button type="submit" disabled={busy || draft === mode}>
          {busy ? 'Saving…' : 'Save'}
        </Button>
      </form>
    </div>
  );
}
