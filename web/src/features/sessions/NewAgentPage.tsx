import { useNavigate } from '@tanstack/react-router';
import { useState, type KeyboardEvent } from 'react';
import { ApiError } from '@/api';
import { Button } from '@/components/ui/button';
import { useConnection } from '@/shell/ConnectionBanner';
import { useCreateSession } from './queries';

type Kind = 'claude' | 'codex';

const KINDS: { value: Kind; label: string }[] = [
  { value: 'claude', label: 'Claude' },
  { value: 'codex', label: 'Codex' }
];

/**
 * Starts a native agent session from a title, a kind and a first prompt, then opens the session's
 * page. Ctrl/Cmd+Enter in the prompt box submits.
 */
export function NewAgentPage() {
  const navigate = useNavigate();
  const offline = useConnection().status === 'offline';
  const create = useCreateSession();
  const [kind, setKind] = useState<Kind>('claude');
  // null means the title follows the agent kind until the owner types one.
  const [titleOverride, setTitleOverride] = useState<string | null>(null);
  const [prompt, setPrompt] = useState('');

  const title = titleOverride ?? `${kind} session`;
  const canStart = !offline && !create.isPending && prompt.trim().length > 0;

  function start() {
    if (!canStart) return;
    create.mutate(
      {
        body: {
          title: title.trim() || `${kind} session`,
          description: `Native ${kind} agent session`,
          prompt: prompt.trim(),
          agent_kind: kind
        }
      },
      {
        onSuccess: (session) => {
          void navigate({
            to: '/sessions/$id',
            params: { id: session.id },
            search: (prev) => ({ ...prev, tab: undefined })
          });
        }
      }
    );
  }

  function onPromptKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      start();
    }
  }

  const failure =
    create.error instanceof ApiError
      ? create.error.message
      : create.isError
        ? 'Failed to start the agent session'
        : null;

  return (
    <form
      className="gap-md p-md flex max-w-2xl flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        start();
      }}
    >
      <h1 className="text-lg font-medium">New agent session</h1>

      <fieldset className="gap-xs flex flex-col">
        <legend className="text-sm font-medium">Agent</legend>
        <div className="gap-sm flex">
          {KINDS.map((option) => (
            <label
              key={option.value}
              className="gap-xs px-md border-border has-checked:bg-accent has-checked:text-accent-foreground flex min-h-11 cursor-pointer items-center border text-sm"
            >
              <input
                type="radio"
                name="agent-kind"
                value={option.value}
                data-testid={`kind-${option.value}`}
                checked={kind === option.value}
                onChange={() => setKind(option.value)}
                className="size-4"
              />
              {option.label}
            </label>
          ))}
        </div>
      </fieldset>

      <label className="gap-xs flex flex-col text-sm">
        Title
        <input
          data-testid="agent-title"
          value={title}
          onChange={(event) => setTitleOverride(event.target.value)}
          className="border-border bg-background text-foreground px-md min-h-11 w-full border text-sm"
        />
      </label>

      <label className="gap-xs flex flex-col text-sm">
        First prompt
        <textarea
          data-testid="agent-prompt"
          rows={6}
          value={prompt}
          onChange={(event) => setPrompt(event.target.value)}
          onKeyDown={onPromptKeyDown}
          className="border-border bg-background text-foreground px-md py-sm w-full border text-sm"
        />
      </label>

      {offline ? (
        <p className="text-term-yellow text-sm">
          {"Backend unreachable: can't start a session right now."}
        </p>
      ) : null}
      {failure ? (
        <p role="alert" className="text-destructive text-sm">
          {failure}
        </p>
      ) : null}

      <div>
        <Button type="submit" data-testid="agent-start" disabled={!canStart}>
          {create.isPending ? 'Starting…' : 'Start session'}
        </Button>
      </div>
    </form>
  );
}
