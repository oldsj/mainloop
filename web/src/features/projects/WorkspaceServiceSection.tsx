import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { failureText } from './model';
import { useUpdateWorkspaceService, useWorkspaceService } from './queries';

/** The server's bounds (api/openapi.yaml WorkspaceService): kagent's 64 arguments, less the env(1) prefix. */
export const SERVICE_MAX_ARGS = 61;
export const SERVICE_MAX_ARG_BYTES = 4096;
/** At most 16 ports, so MAINLOOP_PREVIEW_URLS fits one argument. */
export const SERVICE_MAX_PORTS = 16;

const PORT_NAME = /^[a-z][a-z0-9-]{0,31}$/;

export interface ServicePort {
  name: string;
  number: number;
}

interface Service {
  command: string[];
  ports: ServicePort[];
}

const lines = (draft: string) =>
  draft
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);

/**
 * The draft as an argv: one argument per line, spaces at each line's ends dropped, blank lines
 * skipped. An error names the first bound the draft breaks, so it is never sent.
 */
export function commandFromDraft(draft: string): { command: string[]; error: string | null } {
  const command = lines(draft);
  if (command.length > SERVICE_MAX_ARGS) {
    return { command, error: `At most ${SERVICE_MAX_ARGS} arguments; this has ${command.length}.` };
  }
  const encoder = new TextEncoder();
  const long = command.findIndex((arg) => encoder.encode(arg).length > SERVICE_MAX_ARG_BYTES);
  if (long >= 0) {
    return { command, error: `Argument ${long + 1} is over ${SERVICE_MAX_ARG_BYTES} bytes.` };
  }
  return { command, error: null };
}

/**
 * The draft as preview ports: one `name=number` per line, blank lines skipped, checked as the
 * server checks them (name, range, unique name and number, at most 16).
 */
export function portsFromDraft(draft: string): { ports: ServicePort[]; error: string | null } {
  const ports: ServicePort[] = [];
  for (const line of lines(draft)) {
    const [, name = '', digits = ''] = /^([^=\s]+)\s*=\s*(\d+)$/.exec(line) ?? [];
    const number = Number(digits);
    if (!PORT_NAME.test(name) || !Number.isInteger(number) || number < 1 || number > 65535) {
      return {
        ports,
        error: `"${line}" is not a port. Write name=number, for example web=3000; names are lowercase letters, digits and dashes, starting with a letter.`
      };
    }
    if (ports.some((port) => port.name === name || port.number === number)) {
      return { ports, error: 'Each port needs its own name and number.' };
    }
    ports.push({ name, number });
  }
  if (ports.length > SERVICE_MAX_PORTS) {
    return { ports, error: `At most ${SERVICE_MAX_PORTS} ports; this has ${ports.length}.` };
  }
  return { ports, error: null };
}

const portsText = (ports: ServicePort[]) =>
  ports.map((port) => `${port.name}=${port.number}`).join('\n');

const sameService = (a: Service, b: Service) =>
  a.command.join('\n') === b.command.join('\n') && portsText(a.ports) === portsText(b.ports);

function readService(data: unknown): Service | null {
  if (typeof data !== 'object' || data === null) return null;
  const { command, ports } = data as { command?: unknown; ports?: unknown };
  const commandOk = Array.isArray(command) && command.every((arg) => typeof arg === 'string');
  const portsOk =
    Array.isArray(ports) &&
    ports.every(
      (port: unknown) =>
        typeof port === 'object' &&
        port !== null &&
        typeof (port as ServicePort).name === 'string' &&
        Number.isInteger((port as ServicePort).number)
    );
  return commandOk && portsOk
    ? { command: command as string[], ports: ports as ServicePort[] }
    : null;
}

/**
 * The project's workspace service: a development server kagent starts in every new workspace's
 * checkout and starts again whenever the workspace wakes, and the preview ports those workspaces
 * declare. Changing either affects workspaces created afterwards only.
 */
export function WorkspaceServiceSection({ projectId }: { projectId: string }) {
  const service = useWorkspaceService(projectId);
  const update = useUpdateWorkspaceService(projectId);
  const saved = service.data === undefined ? undefined : readService(service.data);

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
        A development server Mainloop keeps running in each of this project&apos;s workspaces, and
        the ports their preview links reach. The server starts in the checkout when the workspace
        starts and again whenever it wakes, with MAINLOOP_WORKSPACE_ID and MAINLOOP_PREVIEW_URLS
        set. Workspaces created after a save use the new settings; existing ones keep theirs.
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

      {saved === null ? (
        <ErrorRecovery
          title="Couldn't load the workspace service."
          message="Mainloop sent a workspace service that does not match the contract."
        />
      ) : null}

      {saved ? (
        <ServiceForm
          // A saved or re-read service starts a fresh draft from it.
          key={`${saved.command.join('\n')}\u0000${portsText(saved.ports)}`}
          saved={saved}
          busy={update.isPending}
          saveError={update.error ? failureText(update.error) : null}
          onSave={(next) =>
            update.mutate({ params: { path: { project_id: projectId } }, body: next })
          }
        />
      ) : null}
    </section>
  );
}

interface ServiceFormProps {
  saved: Service;
  busy: boolean;
  saveError: string | null;
  onSave: (service: Service) => void;
}

function ServiceForm({ saved, busy, saveError, onSave }: ServiceFormProps) {
  const [commandDraft, setCommandDraft] = useState(saved.command.join('\n'));
  const [portsDraft, setPortsDraft] = useState(portsText(saved.ports));
  const { command, error: commandError } = commandFromDraft(commandDraft);
  const { ports, error: portsError } = portsFromDraft(portsDraft);
  const changed = !sameService({ command, ports }, saved);
  const invalid = Boolean(commandError || portsError);

  return (
    <form
      className="gap-md flex flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        if (changed && !invalid && !busy) onSave({ command, ports });
      }}
    >
      <p className="text-sm">
        {saved.command.length === 0 ? (
          'No workspace service. Workspaces start no server of their own.'
        ) : (
          <>
            Runs: <code className="break-all">{saved.command.join(' ')}</code>
          </>
        )}
      </p>
      <p className="text-sm">
        {saved.ports.length === 0
          ? 'No preview ports.'
          : `Preview ports: ${saved.ports.map((port) => `${port.name} ${port.number}`).join(', ')}`}
      </p>

      <label className="gap-xs flex flex-col text-sm" htmlFor="workspace-service-command">
        Command, one argument per line
      </label>
      <textarea
        id="workspace-service-command"
        aria-describedby="workspace-service-hint"
        className="border-border bg-background text-foreground p-sm min-h-24 border font-mono text-sm"
        value={commandDraft}
        disabled={busy}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        rows={3}
        onChange={(event) => setCommandDraft(event.target.value)}
      />
      <p id="workspace-service-hint" className="text-muted-foreground text-sm">
        The first line is the program, found on the workspace&apos;s PATH or relative to the
        checkout, for example <code>scripts/dev/workspace.sh</code> then <code>run</code>. Leave it
        empty to declare none.
      </p>
      {commandError ? (
        <p role="alert" className="text-destructive text-sm">
          {commandError}
        </p>
      ) : null}

      <label className="gap-xs flex flex-col text-sm" htmlFor="workspace-service-ports">
        Preview ports, one name=number per line
      </label>
      <textarea
        id="workspace-service-ports"
        aria-describedby="workspace-service-ports-hint"
        className="border-border bg-background text-foreground p-sm min-h-16 border font-mono text-sm"
        value={portsDraft}
        disabled={busy}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        rows={2}
        onChange={(event) => setPortsDraft(event.target.value)}
      />
      <p id="workspace-service-ports-hint" className="text-muted-foreground text-sm">
        For example <code>web=3000</code>. Each port gets a preview link in every new workspace,
        including task workspaces. At most {SERVICE_MAX_PORTS}.
      </p>
      {portsError ? (
        <p role="alert" className="text-destructive text-sm">
          {portsError}
        </p>
      ) : null}

      {saveError ? (
        <p role="alert" className="text-destructive text-sm">
          {saveError}
        </p>
      ) : null}
      <Button type="submit" className="self-start" disabled={busy || !changed || invalid}>
        {busy ? 'Saving…' : 'Save'}
      </Button>
    </form>
  );
}
