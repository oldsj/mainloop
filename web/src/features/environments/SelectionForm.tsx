import { useState } from 'react';
import { Button } from '@/components/ui/button';
import {
  canAcceptDefault,
  canSaveSelection,
  draftFrom,
  eligible,
  needsAcceptedDefault,
  selectionRequest,
  versionLabel,
  type Draft,
  type EnvironmentRow,
  type Selection,
  type VersionRow
} from './model';

export type SelectionBody = ReturnType<typeof selectionRequest>;

/** One environment's version read, as the draft's version choice needs it. */
export interface VersionRead {
  /** The last loaded list. After a failed refresh it is the list from before the failure. */
  versions: VersionRow[];
  /** A fresh read has landed, so the list and anything derived from it can be trusted. */
  ready: boolean;
  /** Why the last read failed, in words for the owner. Null when it did not. */
  failure: string | null;
  loading: boolean;
  retry: () => void;
}

interface SelectionFormProps {
  saved: Selection | null;
  environments: EnvironmentRow[];
  reads: Record<string, VersionRead>;
  saving: boolean;
  saveError: string | null;
  accepting: boolean;
  acceptError: string | null;
  onSave: (body: SelectionBody) => void;
  onAccept: (environmentId: string, versionId: string) => void;
}

/**
 * The owner's draft selection. The draft starts from the saved selection once, when this form
 * mounts, and a refetch never overwrites it. The parent owns the requests, so a failed save or
 * accept stays on screen after the form is re-rendered.
 */
export function SelectionForm({
  saved,
  environments,
  reads,
  saving,
  saveError,
  accepting,
  acceptError,
  onSave,
  onAccept
}: SelectionFormProps) {
  const [draft, setDraft] = useState<Draft>(() => draftFrom(saved));
  const environment = environments.find((item) => item.id === draft.environmentId);
  const read = draft.environmentId ? reads[draft.environmentId] : undefined;
  const candidates = read?.versions ?? [];
  const canSave = canSaveSelection({
    draft,
    saved,
    environment,
    versions: candidates,
    busy: saving
  });
  const canAccept = canAcceptDefault({
    draft,
    environment,
    versions: candidates,
    busy: accepting
  });

  return (
    <form
      className="gap-md flex flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        if (canSave) onSave(selectionRequest(draft, saved));
      }}
    >
      <fieldset className="gap-md p-md border-border flex flex-col border">
        <legend className="text-sm font-medium">Draft selection</legend>

        <label className="gap-xs flex flex-col text-sm">
          Environment
          <select
            aria-label="Environment"
            className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
            value={draft.environmentId}
            onChange={(event) =>
              setDraft({ ...draft, environmentId: event.target.value, versionId: '' })
            }
          >
            <option value="">Choose an environment</option>
            {environments.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
        </label>

        <label className="gap-sm flex min-h-11 items-center text-sm">
          <input
            type="checkbox"
            className="size-5"
            checked={draft.follow}
            disabled={!draft.environmentId}
            onChange={(event) => setDraft({ ...draft, follow: event.target.checked })}
          />
          Follow this environment’s accepted default
        </label>
        {draft.follow && environment ? (
          <p className="text-muted-foreground text-xs">
            Accepted default: {environment.accepted_default_version_id ?? 'None'}. Future accepted
            defaults also apply to new sessions.
          </p>
        ) : null}
        {/* Until a fresh read lands, an empty list would wrongly read as "no accepted default". */}
        {read?.ready && needsAcceptedDefault(draft, environment, candidates) ? (
          <p className="text-term-yellow text-sm">
            An accepted current-policy version is required. Uncheck follow-default, choose a
            validated version, then accept it below.
          </p>
        ) : null}

        <label className="gap-xs flex flex-col text-sm">
          Version
          <select
            aria-label="Version"
            className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
            value={draft.versionId}
            disabled={draft.follow || !draft.environmentId}
            onChange={(event) => setDraft({ ...draft, versionId: event.target.value })}
          >
            <option value="">Choose a version</option>
            {candidates.map((version) => (
              <option key={version.id} value={version.id} disabled={!eligible(version)}>
                {versionLabel(version)}
              </option>
            ))}
          </select>
        </label>
        {read?.loading && !read.ready && !read.failure ? (
          <p role="status" className="text-muted-foreground text-xs">
            Loading versions…
          </p>
        ) : null}
        {read?.failure ? (
          <div role="alert" className="gap-xs flex flex-col text-sm">
            <p className="text-destructive">
              {read.failure}
              {candidates.length > 0 ? ' Showing the last loaded versions.' : ''}
            </p>
            <div>
              <Button type="button" variant="outline" onClick={read.retry} disabled={read.loading}>
                Retry loading versions
              </Button>
            </div>
          </div>
        ) : null}
      </fieldset>

      {!draft.follow &&
      draft.versionId &&
      draft.versionId !== environment?.accepted_default_version_id ? (
        <div className="gap-xs flex flex-col">
          <Button
            type="button"
            variant="outline"
            disabled={!canAccept}
            onClick={() => onAccept(draft.environmentId, draft.versionId)}
          >
            {accepting ? 'Accepting…' : 'Accept selected version as environment default'}
          </Button>
          <p className="text-muted-foreground text-xs">
            This changes the default for every project following this environment.
          </p>
          {acceptError ? (
            <p role="alert" className="text-destructive text-sm">
              {acceptError}
            </p>
          ) : null}
        </div>
      ) : null}

      {saveError ? (
        <p role="alert" className="text-destructive text-sm">
          {saveError}
        </p>
      ) : null}
      <Button type="submit" disabled={!canSave}>
        {saving ? 'Saving…' : 'Save selection'}
      </Button>
    </form>
  );
}
