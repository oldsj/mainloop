import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { ApiError } from '@/api';
import { relativeTime } from '@/features/sessions/model';
import { useInstructionVersions, useInstructions, useUpdateInstructions } from './queries';
import {
  failureText,
  instructionsDraft,
  instructionsStatus,
  instructionVersionListSchema,
  INSTRUCTIONS_EXPLANATION,
  projectInstructionsSchema
} from './model';

/**
 * The owner's standing instructions for the project's agents (docs/specs/project-context.md).
 *
 * The draft lives here rather than in a form that remounts, so nothing the owner typed is lost when
 * the saved instructions are re-read. A draft remembers the version it started from: when the saved
 * version moves under it (another save, or a 409 that re-read it) the section shows the current text
 * and the owner saves over it explicitly or discards the draft.
 */
export function InstructionsSection({ projectId }: { projectId: string }) {
  const instructions = useInstructions(projectId);
  const update = useUpdateInstructions(projectId);
  const [draft, setDraft] = useState<string | null>(null);
  const [base, setBase] = useState(0);

  const parsed = instructions.data
    ? projectInstructionsSchema.safeParse(instructions.data)
    : undefined;
  const view = parsed?.success ? parsed.data : undefined;

  return (
    <section
      aria-labelledby="instructions-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="instructions"
    >
      <h3 id="instructions-heading" className="text-lg font-medium">
        Instructions
      </h3>

      {instructions.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading instructions…
        </p>
      ) : null}

      {instructions.isError && !view ? (
        <ErrorRecovery
          title="Could not load the instructions."
          message={failureText(instructions.error)}
          onRetry={() => void instructions.refetch()}
          retryLabel="Retry"
          pending={instructions.isFetching}
        />
      ) : null}

      {parsed && !parsed.success ? (
        <ErrorRecovery
          title="Could not load the instructions."
          message="Mainloop sent instructions that do not match the contract."
        />
      ) : null}

      {view ? (
        <InstructionsForm
          view={view}
          draft={draft}
          conflict={draft !== null && base !== view.version}
          busy={update.isPending}
          saveError={update.error ?? null}
          onEdit={(value) => {
            if (draft === null) setBase(view.version);
            setDraft(value);
          }}
          onDiscard={() => {
            setDraft(null);
            update.reset();
          }}
          onSave={(text) =>
            update.mutate(
              {
                params: { path: { project_id: projectId } },
                body: { text, expected_version: view.version }
              },
              { onSuccess: () => setDraft(null) }
            )
          }
        />
      ) : null}

      {view && view.version > 0 ? <EarlierVersions projectId={projectId} /> : null}
    </section>
  );
}

interface InstructionsFormProps {
  view: { version: number; text: string; max_bytes: number; updated_at: string | null };
  draft: string | null;
  conflict: boolean;
  busy: boolean;
  /** The failed save, as the client threw it; null while there is none. */
  saveError: unknown;
  onEdit: (value: string) => void;
  onDiscard: () => void;
  onSave: (text: string) => void;
}

function InstructionsForm({
  view,
  draft,
  conflict,
  busy,
  saveError,
  onEdit,
  onDiscard,
  onSave
}: InstructionsFormProps) {
  const value = draft ?? view.text;
  const state = instructionsDraft(value, view.text, view.max_bytes);
  const canSave = draft !== null && state.changed && !state.over && !busy;
  const staleSave = saveError instanceof ApiError && saveError.status === 409;

  return (
    <div className="gap-md flex flex-col">
      <p className="text-sm">{instructionsStatus(view, (iso) => relativeTime(iso))}</p>
      <p className="text-muted-foreground text-sm">{INSTRUCTIONS_EXPLANATION}</p>

      {conflict ? (
        <div role="alert" className="gap-sm flex flex-col" data-testid="instructions-conflict">
          <p className="text-term-yellow text-sm">
            The instructions changed to version {view.version} while you were editing. Your draft is
            kept. Review the current version, then save over it or discard your draft.
          </p>
          <details className="text-sm" open>
            <summary className="py-sm min-h-control cursor-pointer">
              Current version {view.version}
            </summary>
            <pre className="p-sm border-border border font-mono text-xs break-words whitespace-pre-wrap">
              {view.text || '(empty)'}
            </pre>
          </details>
        </div>
      ) : null}

      <form
        className="gap-md flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (canSave) onSave(value);
        }}
      >
        <label className="gap-xs flex flex-col text-sm">
          Instructions for agents
          <textarea
            aria-describedby="instructions-size"
            className="border-input bg-background text-foreground p-sm w-full rounded-none border font-mono text-sm"
            rows={10}
            value={value}
            disabled={busy}
            placeholder="For example: UI demos use fictional data. Ask before changing billing."
            onChange={(event) => onEdit(event.target.value)}
          />
        </label>
        <p
          id="instructions-size"
          className={state.over ? 'text-destructive text-sm' : 'text-muted-foreground text-sm'}
        >
          {state.bytes.toLocaleString()} of {view.max_bytes.toLocaleString()} bytes
          {state.over ? ` (${(state.bytes - view.max_bytes).toLocaleString()} over)` : ''}
        </p>

        {saveError && !(staleSave && conflict) ? (
          <p role="alert" className="text-destructive text-sm">
            {failureText(saveError)}
          </p>
        ) : null}

        <div className="gap-sm flex flex-wrap">
          <Button type="submit" disabled={!canSave}>
            {busy
              ? 'Saving…'
              : conflict
                ? `Save over version ${view.version}`
                : 'Save instructions'}
          </Button>
          {draft !== null ? (
            <Button type="button" variant="outline" onClick={onDiscard} disabled={busy}>
              Discard draft
            </Button>
          ) : null}
        </div>
      </form>
    </div>
  );
}

/** Earlier versions, read when the owner opens the list. */
function EarlierVersions({ projectId }: { projectId: string }) {
  const [open, setOpen] = useState(false);
  const versions = useInstructionVersions(projectId, open);
  const parsed = versions.data ? instructionVersionListSchema.safeParse(versions.data) : undefined;

  return (
    <details className="text-sm" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary className="py-sm min-h-control cursor-pointer">Earlier versions</summary>
      {versions.isPending && open ? (
        <p role="status" className="text-muted-foreground">
          Loading versions…
        </p>
      ) : null}
      {versions.isError ? (
        <ErrorRecovery
          title="Could not load earlier versions."
          message={failureText(versions.error)}
          onRetry={() => void versions.refetch()}
          retryLabel="Retry"
          pending={versions.isFetching}
        />
      ) : null}
      {parsed && !parsed.success ? (
        <p className="text-destructive">Mainloop sent versions that do not match the contract.</p>
      ) : null}
      {parsed?.success ? (
        <ol className="gap-sm pt-sm flex flex-col">
          {parsed.data.versions.map((v) => (
            <li key={v.version} className="gap-xs flex flex-col">
              <span>
                Version {v.version},{' '}
                <time dateTime={v.created_at}>{new Date(v.created_at).toLocaleString()}</time>
              </span>
              <pre className="p-sm border-border border font-mono text-xs break-words whitespace-pre-wrap">
                {v.text || '(cleared)'}
              </pre>
            </li>
          ))}
        </ol>
      ) : null}
    </details>
  );
}
