import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { failureText } from './model';
import { useUpdateWorkspaceService, useWorkspaceService } from './queries';

/** The server's bounds (api/openapi.yaml WorkspaceService): kagent's 64 arguments, less the env(1) prefix. */
export const SERVICE_MAX_ARGS = 61;
export const SERVICE_MAX_ARG_BYTES = 4096;

/**
 * The draft as an argv: one argument per line, spaces at each line's ends dropped, blank lines
 * skipped. An error names the first bound the draft breaks, so it is never sent.
 */
export function commandFromDraft(draft: string): { command: string[]; error: string | null } {
  const command = draft
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);
  if (command.length > SERVICE_MAX_ARGS) {
    return { command, error: `At most ${SERVICE_MAX_ARGS} arguments; this has ${command.length}.` };
  }
  const encoder = new TextEncoder();
  const long = command.findIndex((arg) => encoder.encode(arg).length > SERVICE_MAX_ARG_BYTES);
  if (long >= 0) {
    return {
      command,
      error: `Argument ${long + 1} is over ${SERVICE_MAX_ARG_BYTES} bytes.`
    };
  }
  return { command, error: null };
}

const sameCommand = (a: string[], b: string[]) =>
  a.length === b.length && a.every((arg, index) => arg === b[index]);

function readCommand(data: unknown): string[] | null {
  if (typeof data !== 'object' || data === null) return null;
  const command = (data as { command?: unknown }).command;
  return Array.isArray(command) && command.every((arg) => typeof arg === 'string')
    ? (command as string[])
    : null;
}

/**
 * The project's workspace service: a development server kagent starts in every new workspace's
 * checkout and starts again whenever the workspace wakes. Changing it affects workspaces created
 * afterwards only.
 */
export function WorkspaceServiceSection({ projectId }: { projectId: string }) {
  const service = useWorkspaceService(projectId);
  const update = useUpdateWorkspaceService(projectId);
  const command = service.data === undefined ? undefined : readCommand(service.data);

  return (
    <section
      id="workspace-service"
      aria-labelledby="workspace-service-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="workspace-service"
    >
      <h3 id="workspace-service-heading" className="text-lg font-medium">
        Workspace service
      </h3>
      <p className="text-muted-foreground text-sm">
        A development server Mainloop keeps running in each of this project&apos;s workspaces. It
        starts in the checkout when the workspace starts and again whenever it wakes, with
        MAINLOOP_WORKSPACE_ID and MAINLOOP_PREVIEW_URLS set. Workspaces created after a save use the
        new command; existing ones keep theirs.
      </p>

      {service.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading workspace service…
        </p>
      ) : null}

      {service.isError ? (
        <ErrorRecovery
          title="Couldn't load the workspace service."
          message={failureText(service.error)}
          onRetry={() => void service.refetch()}
          retryLabel="Retry loading workspace service"
          pending={service.isFetching}
        />
      ) : null}

      {command === null ? (
        <ErrorRecovery
          title="Couldn't load the workspace service."
          message="Mainloop sent a workspace service that does not match the contract."
        />
      ) : null}

      {command ? (
        <ServiceForm
          // A saved or re-read command starts a fresh draft from it.
          key={command.join('\n')}
          saved={command}
          busy={update.isPending}
          saveError={update.error ? failureText(update.error) : null}
          onSave={(next) =>
            update.mutate({
              params: { path: { project_id: projectId } },
              body: { command: next }
            })
          }
        />
      ) : null}
    </section>
  );
}

interface ServiceFormProps {
  saved: string[];
  busy: boolean;
  saveError: string | null;
  onSave: (command: string[]) => void;
}

function ServiceForm({ saved, busy, saveError, onSave }: ServiceFormProps) {
  const [draft, setDraft] = useState(saved.join('\n'));
  const { command, error } = commandFromDraft(draft);
  const changed = !sameCommand(command, saved);

  return (
    <form
      className="gap-md flex flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        if (changed && !error && !busy) onSave(command);
      }}
    >
      <p className="text-sm">
        {saved.length === 0 ? (
          'No workspace service. Workspaces start no server of their own.'
        ) : (
          <>
            Runs: <code className="break-all">{saved.join(' ')}</code>
          </>
        )}
      </p>
      <label className="gap-xs flex flex-col text-sm" htmlFor="workspace-service-command">
        Command, one argument per line
      </label>
      <textarea
        id="workspace-service-command"
        aria-describedby="workspace-service-hint"
        className="border-border bg-background text-foreground p-sm min-h-24 border font-mono text-sm"
        value={draft}
        disabled={busy}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        rows={3}
        onChange={(event) => setDraft(event.target.value)}
      />
      <p id="workspace-service-hint" className="text-muted-foreground text-sm">
        The first line is the program, found on the workspace&apos;s PATH or relative to the
        checkout, for example <code>scripts/dev/workspace.sh</code> then <code>run</code>. Leave it
        empty to declare none.
      </p>
      {error ? (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      ) : null}
      {saveError ? (
        <p role="alert" className="text-destructive text-sm">
          {saveError}
        </p>
      ) : null}
      <Button type="submit" className="self-start" disabled={busy || !changed || Boolean(error)}>
        {busy ? 'Saving…' : command.length === 0 && saved.length > 0 ? 'Remove service' : 'Save'}
      </Button>
    </form>
  );
}
