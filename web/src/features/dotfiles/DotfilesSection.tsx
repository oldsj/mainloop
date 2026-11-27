import { useId, useState } from 'react';
import { ApiError } from '@/api/errors';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import {
  dotfilesSourceSchema,
  REPOSITORY,
  sourceText,
  sourceUrl,
  type DotfilesSource
} from './model';
import {
  useOwnerDotfiles,
  useProjectDotfiles,
  useSetOwnerDotfiles,
  useSetProjectDotfiles
} from './queries';

const FIELD =
  'border-input bg-background text-foreground px-sm min-h-control text-field w-full min-w-0 border disabled:opacity-50';

/** What a failed save says: a stale version asks for a reload, else the server's own sentence. */
function saveError(error: unknown): string {
  if (error instanceof ApiError && error.status === 409)
    return 'Not saved: the source changed elsewhere. Reload and try again.';
  if (error instanceof ApiError && error.message) return `Not saved: ${error.message}`;
  return 'Not saved. The source is unchanged.';
}

/** The owner-wide source, on Settings. */
export function OwnerDotfilesSection() {
  const read = useOwnerDotfiles();
  const save = useSetOwnerDotfiles();
  return (
    <section
      id="dotfiles"
      aria-labelledby="dotfiles-heading"
      className="gap-md flex flex-col"
      data-testid="dotfiles"
    >
      <div className="gap-xs flex flex-col">
        <h2 id="dotfiles-heading" className="text-title font-semibold">
          Agent dotfiles
        </h2>
        <p className="text-muted-foreground max-w-measure text-body">
          A GitHub repository directory whose <code>AGENTS.md</code>, skills, subagents and commands
          every agent session gets as its own user-level files, read-only. A project can set its
          own. New sessions pin the commit; running ones keep theirs.
        </p>
      </div>
      <SourceBody
        read={read}
        busy={save.isPending}
        error={save.error ? saveError(save.error) : null}
        onSave={(source, expected) =>
          save.mutate({ body: { ...source, expected_version: expected } })
        }
        clearLabel="Remove"
      />
    </section>
  );
}

/** A project's own source, on the project page, replacing the owner-wide one while set. */
export function ProjectDotfilesSection({ projectId }: { projectId: string }) {
  const read = useProjectDotfiles(projectId);
  const owner = useOwnerDotfiles();
  const save = useSetProjectDotfiles(projectId);
  const ownerSource = owner.data ? dotfilesSourceSchema.safeParse(owner.data) : undefined;
  const inherited = ownerSource?.success ? sourceText(ownerSource.data) : null;
  return (
    <section
      aria-labelledby="project-dotfiles-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="dotfiles"
    >
      <h3 id="project-dotfiles-heading" className="text-title font-semibold">
        Agent dotfiles
      </h3>
      <SourceBody
        read={read}
        busy={save.isPending}
        error={save.error ? saveError(save.error) : null}
        onSave={(source, expected) =>
          save.mutate({
            params: { path: { project_id: projectId } },
            body: { ...source, expected_version: expected }
          })
        }
        clearLabel="Use the owner-wide source"
        unset={
          inherited
            ? `This project uses the owner-wide source: ${inherited}.`
            : 'No source is set for this project or owner-wide.'
        }
      />
    </section>
  );
}

interface SourceBodyProps {
  read: {
    data?: unknown;
    isError: boolean;
    isFetching: boolean;
    refetch: () => unknown;
  };
  busy: boolean;
  error: string | null;
  onSave: (source: Pick<DotfilesSource, 'repository' | 'ref' | 'path'>, expected: number) => void;
  clearLabel: string;
  unset?: string;
}

function SourceBody({ read, busy, error, onSave, clearLabel, unset }: SourceBodyProps) {
  if (read.isError) {
    return (
      <ErrorRecovery
        title="Could not load the dotfiles source."
        message="Nothing was changed."
        onRetry={() => void read.refetch()}
        retryLabel="Retry"
        pending={read.isFetching}
      />
    );
  }
  if (read.data === undefined) {
    return (
      <p role="status" className="text-muted-foreground text-body">
        Loading dotfiles source…
      </p>
    );
  }
  const parsed = dotfilesSourceSchema.safeParse(read.data);
  if (!parsed.success) {
    return (
      <ErrorRecovery
        title="Could not load the dotfiles source."
        message="Mainloop sent a dotfiles source that does not match the contract."
      />
    );
  }
  const source = parsed.data;
  return (
    <SourceForm
      // A saved or reloaded source resets the draft.
      key={source.version}
      source={source}
      busy={busy}
      error={error}
      onSave={onSave}
      clearLabel={clearLabel}
      unset={unset}
    />
  );
}

interface SourceFormProps {
  source: DotfilesSource;
  busy: boolean;
  error: string | null;
  onSave: SourceBodyProps['onSave'];
  clearLabel: string;
  unset?: string;
}

function SourceForm({ source, busy, error, onSave, clearLabel, unset }: SourceFormProps) {
  const id = useId();
  const [repository, setRepository] = useState(source.repository ?? '');
  const [ref, setRef] = useState(source.ref);
  const [path, setPath] = useState(source.path);
  const text = sourceText(source);
  const link = sourceUrl(source);
  const trimmed = repository.trim();
  const invalid = trimmed !== '' && !REPOSITORY.test(trimmed);
  const changed =
    trimmed !== (source.repository ?? '') ||
    ref.trim() !== source.ref ||
    path.trim() !== source.path;

  return (
    <div className="gap-md flex flex-col">
      {text ? (
        <p className="text-body" data-testid="dotfiles-current">
          Current:{' '}
          {link ? (
            <a href={link} target="_blank" rel="noopener noreferrer" className="underline">
              {text}
            </a>
          ) : (
            text
          )}
        </p>
      ) : (
        <p className="text-muted-foreground text-body" data-testid="dotfiles-current">
          {unset ?? 'No source is set: sessions get no dotfiles.'}
        </p>
      )}
      <form
        className="gap-md flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (busy || invalid || trimmed === '' || !changed) return;
          onSave({ repository: trimmed, ref: ref.trim(), path: path.trim() }, source.version);
        }}
      >
        <label htmlFor={`${id}-repo`} className="gap-xs text-body flex flex-col">
          Repository
          <input
            id={`${id}-repo`}
            className={FIELD}
            value={repository}
            placeholder="owner/name"
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
            aria-invalid={invalid || undefined}
            onChange={(event) => setRepository(event.target.value)}
          />
        </label>
        {invalid ? (
          <p className="text-destructive text-body">Write the repository as owner/name.</p>
        ) : null}
        <label htmlFor={`${id}-ref`} className="gap-xs text-body flex flex-col">
          Branch, tag or commit
          <input
            id={`${id}-ref`}
            className={FIELD}
            value={ref}
            placeholder="default branch"
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
            onChange={(event) => setRef(event.target.value)}
          />
        </label>
        <label htmlFor={`${id}-path`} className="gap-xs text-body flex flex-col">
          Directory
          <input
            id={`${id}-path`}
            className={FIELD}
            value={path}
            placeholder="repository root"
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
            onChange={(event) => setPath(event.target.value)}
          />
        </label>
        {error ? (
          <p role="alert" className="text-destructive text-body">
            {error}
          </p>
        ) : null}
        <div className="gap-sm flex flex-wrap">
          <Button type="submit" disabled={busy || invalid || trimmed === '' || !changed}>
            {busy ? 'Saving…' : 'Save'}
          </Button>
          {source.repository ? (
            <Button
              type="button"
              variant="outline"
              disabled={busy}
              onClick={() => onSave({ repository: null, ref: '', path: '' }, source.version)}
            >
              {clearLabel}
            </Button>
          ) : null}
        </div>
      </form>
    </div>
  );
}
