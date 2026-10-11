import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { useMergePolicy, useUpdateMergePolicy } from './queries';
import {
  canSavePolicy,
  failureText,
  mergePolicyViewSchema,
  POLICY_EXPLANATION,
  POLICY_LABEL,
  policyText,
  type MergePolicy,
  type MergePolicyView
} from './model';

/**
 * The project's merge policy. Editing is possible only when the server reports writes enabled. The
 * save request lives here, not in the form, so a failed save stays on screen after the form renders
 * again. A refresh remounts the form from the refreshed policy, so the owner reviews it before saving.
 */
export function MergePolicySection({ projectId }: { projectId: string }) {
  const policy = useMergePolicy(projectId);
  const update = useUpdateMergePolicy(projectId);
  const [generation, setGeneration] = useState(0);

  const rawView = policy.data;
  const view = rawView && mergePolicyViewSchema.safeParse(rawView).success ? rawView : undefined;
  const busy = update.isPending;
  const stale = update.isError;

  const save = (draft: MergePolicy) => {
    if (!view) return;
    update.mutate({
      params: { path: { project_id: projectId } },
      body: { merge_policy: draft, expected_version: view.merge_policy_version }
    });
  };

  // The form remounts only after the re-read settles, so its draft starts from the refreshed policy.
  const refresh = () => {
    update.reset();
    void policy.refetch().then(() => setGeneration((value) => value + 1));
  };

  return (
    <section
      aria-labelledby="merge-policy-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="merge-policy"
    >
      <h2 id="merge-policy-heading" className="text-lg font-medium">
        Merge policy
      </h2>

      {policy.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading merge policy…
        </p>
      ) : null}

      {policy.isError && !view ? (
        <ErrorRecovery
          title="Could not load the current policy. Refresh before editing."
          message={failureText(policy.error)}
          onRetry={refresh}
          retryLabel="Refresh policy"
          pending={policy.isFetching}
        />
      ) : null}

      {policy.isError && view ? (
        <div data-testid="merge-policy-refresh-error">
          <ErrorRecovery
            title="Could not refresh the current policy. Refresh before editing."
            message={failureText(policy.error)}
            onRetry={refresh}
            retryLabel="Refresh policy"
            pending={policy.isFetching}
          />
        </div>
      ) : null}

      {rawView && !view ? (
        <ErrorRecovery
          title="Could not load the current policy. Refresh before editing."
          message="Mainloop sent a merge policy that does not match the contract."
        />
      ) : null}

      {view ? (
        <PolicyForm
          key={generation}
          view={view}
          busy={busy}
          stale={stale}
          onSave={save}
          onRefresh={refresh}
          refreshing={policy.isFetching}
          saveError={update.error ? failureText(update.error) : null}
        />
      ) : null}
    </section>
  );
}

interface PolicyFormProps {
  view: MergePolicyView;
  busy: boolean;
  stale: boolean;
  saveError: string | null;
  refreshing: boolean;
  onSave: (draft: MergePolicy) => void;
  onRefresh: () => void;
}

function PolicyForm({
  view,
  busy,
  stale,
  saveError,
  refreshing,
  onSave,
  onRefresh
}: PolicyFormProps) {
  const [draft, setDraft] = useState<MergePolicy>(view.merge_policy);
  const editable = view.writes_enabled && !busy && !stale;
  const canSave = canSavePolicy({
    draft,
    saved: view.merge_policy,
    writesEnabled: view.writes_enabled,
    busy,
    stale
  });

  return (
    <div className="gap-md flex flex-col">
      <p className="text-sm">{policyText(view)}</p>
      <p className="text-muted-foreground text-sm">{POLICY_EXPLANATION}</p>

      {!view.writes_enabled ? (
        <p className="text-term-yellow text-sm">
          Editing disabled by the server. The current policy remains in effect.
        </p>
      ) : null}

      <form
        className="gap-md flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (canSave) onSave(draft);
        }}
      >
        <label className="gap-xs flex flex-col text-sm">
          Policy
          <select
            aria-label="Policy"
            className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
            value={draft}
            disabled={!editable}
            onChange={(event) => setDraft(event.target.value as MergePolicy)}
          >
            <option value="auto">{POLICY_LABEL.auto}</option>
            <option value="approval">{POLICY_LABEL.approval}</option>
          </select>
        </label>

        {saveError ? (
          <div role="alert" className="gap-sm flex flex-col">
            <p className="text-destructive text-sm">
              {saveError}. Review the current policy before saving again.
            </p>
            <div>
              <Button type="button" variant="outline" onClick={onRefresh} disabled={refreshing}>
                Refresh policy
              </Button>
            </div>
          </div>
        ) : null}

        <Button type="submit" disabled={!canSave}>
          {busy ? 'Saving…' : 'Save policy'}
        </Button>
      </form>

      <details className="text-sm">
        <summary className="py-sm min-h-11 cursor-pointer">Protected paths (read-only)</summary>
        <ul className="gap-xs pt-sm flex flex-col">
          {view.protected_globs.map((glob) => (
            <li key={glob}>
              <code className="font-mono text-xs break-all">{glob}</code>
            </li>
          ))}
        </ul>
      </details>
    </div>
  );
}
