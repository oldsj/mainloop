import { useState, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { agentLabel } from '@/lib/agentLabel';
import { threadStatus, type ThreadStatus } from '@/lib/delivery';
import {
  boundedText,
  MAIN_PROVIDERS,
  providerLabel,
  providerNeedsAttention,
  providerStateWords,
  type MainProviderControl
} from './mainProvider';
import type { MainProvider, MainThreadInfo } from './types';

const DOT: Record<ThreadStatus, string> = {
  ready: 'bg-term-green',
  idle: 'bg-term-fg-muted',
  unreachable: 'bg-term-red',
  failed: 'bg-term-red',
  unconfirmed: 'bg-term-yellow',
  working: 'bg-term-yellow animate-pulse'
};

/** The words for a status. A failed or unconfirmed last delivery is never called ready. */
export function statusWords(status: ThreadStatus): string {
  if (status === 'failed') return 'last message failed';
  if (status === 'unconfirmed') return 'last message unconfirmed';
  return status;
}

export interface MainThreadHeaderProps {
  info: MainThreadInfo;
  /** The backend is unreachable, so the last state fetched is stale. */
  offline: boolean;
  onStop?: () => void;
  stopping?: boolean;
  /** The native identity strip for the main session; the sessions slice supplies it. */
  renderIdentity?: (sessionId: string) => ReactNode;
  /** The owner's main-provider controls. Omitted, the provider row is not shown. */
  provider?: MainProviderControl;
}

/** The select the owner uses to switch the main provider; one per page, always in the top strip. */
function ProviderSelect({
  info,
  provider
}: {
  info: MainThreadInfo;
  provider: MainProviderControl;
}) {
  const desired: MainProvider | null = info.provider?.desired_provider ?? null;

  return (
    <>
      <label htmlFor="main-provider-select" className="sr-only">
        Main provider
      </label>
      <select
        id="main-provider-select"
        className="border-border bg-background text-foreground px-sm min-h-11 border text-sm disabled:opacity-50"
        value={desired ?? ''}
        disabled={provider.switchBlocked}
        onChange={(event) => provider.select(event.target.value as MainProvider)}
        data-testid="mt-provider-select"
      >
        {desired ? null : (
          <option value="" disabled>
            unknown
          </option>
        )}
        {MAIN_PROVIDERS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </>
  );
}

/** The main provider's status row: the confirmed provider, the status, retry/restart and any notice. The select lives in the top strip, not here. */
function ProviderRow({
  info,
  offline,
  provider
}: {
  info: MainThreadInfo;
  offline: boolean;
  provider: MainProviderControl;
}) {
  const current = info.provider;
  const words = providerStateWords(current, {
    offline,
    selecting: provider.selecting,
    unconfirmed: provider.unconfirmed
  });
  const stateError =
    current?.status === 'failed' && current.error ? boundedText(current.error) : null;
  const buttonClass = 'border-border text-foreground px-md min-h-11 border disabled:opacity-50';

  return (
    <div className="gap-sm px-md pb-xs flex flex-wrap items-center" data-testid="mt-provider">
      <span data-testid="mt-provider-effective">
        effective:{' '}
        {current?.effective_provider ? providerLabel(current.effective_provider) : 'none confirmed'}
      </span>
      <span className="text-foreground" role="status" data-testid="mt-provider-status">
        {words}
      </span>
      {provider.retryOffered ? (
        <button
          type="button"
          className={buttonClass}
          disabled={provider.retryBlocked}
          onClick={provider.retry}
          data-testid="mt-provider-retry"
        >
          Retry
        </button>
      ) : null}
      {provider.restartOffered ? (
        <button
          type="button"
          className={buttonClass}
          disabled={provider.switchBlocked}
          onClick={provider.restart}
          data-testid="mt-provider-restart"
        >
          Restart
        </button>
      ) : null}
      {provider.notice ? (
        <p
          className="text-destructive w-full [overflow-wrap:anywhere]"
          role="alert"
          data-testid="mt-provider-notice"
        >
          {provider.notice}
        </p>
      ) : null}
      {stateError ? (
        <p
          className="text-destructive w-full [overflow-wrap:anywhere]"
          data-testid="mt-provider-error"
        >
          {stateError}
        </p>
      ) : null}
    </div>
  );
}

/** State, model and pending count of the main thread, with the stop button and topic index. */
export function MainThreadHeader({
  info,
  offline,
  onStop,
  stopping = false,
  renderIdentity,
  provider
}: MainThreadHeaderProps) {
  const [open, setOpen] = useState(false);
  const { native } = info;
  // The provider row is a detail while the provider is ready; a failed, switching or unavailable
  // provider shows it inline so the owner sees it without opening anything.
  const providerInline =
    provider !== undefined &&
    providerNeedsAttention(info, {
      offline,
      selecting: provider.selecting,
      unconfirmed: provider.unconfirmed,
      notice: provider.notice
    });
  const model = (
    native?.model ?? agentLabel(native?.kind as 'claude' | 'codex' | undefined)
  ).replace(/^claude-/, '');
  const pending = info.topics.reduce((total, topic) => total + topic.pending, 0);
  const status = threadStatus({
    offline,
    deliveries: native?.deliveries ?? [],
    sessionState: native?.session_state
  });

  return (
    <div
      className="border-border bg-card text-muted-foreground shrink-0 border-b text-xs"
      data-testid="main-thread-header"
    >
      <div className="gap-sm px-md flex min-h-11 flex-wrap items-center py-1">
        <span className={cn('size-2 shrink-0', DOT[status])} aria-hidden="true" />
        <span className="text-foreground" data-testid="mt-state" role="status">
          {statusWords(status)}
        </span>
        <span aria-hidden="true">·</span>
        {provider ? <ProviderSelect info={info} provider={provider} /> : null}
        <span data-testid="mt-model">{model}</span>
        {pending > 0 ? (
          <>
            <span aria-hidden="true">·</span>
            <span data-testid="mt-pending">{pending} pending</span>
          </>
        ) : null}
        {onStop && native?.turn_in_flight ? (
          <button
            type="button"
            className="border-border text-foreground hover:text-term-red px-md min-h-11 border disabled:opacity-50"
            disabled={stopping || offline}
            onClick={onStop}
            data-testid="mt-stop-turn"
          >
            {stopping ? 'stopping…' : 'stop'}
          </button>
        ) : null}
        <button
          type="button"
          className="hover:text-term-accent px-sm ml-auto min-h-11"
          aria-expanded={open}
          aria-controls="main-thread-details"
          onClick={() => setOpen((value) => !value)}
          data-testid="mt-details-toggle"
        >
          details {open ? '▴' : '▾'}
        </button>
      </div>

      {provider && providerInline ? (
        <ProviderRow info={info} offline={offline} provider={provider} />
      ) : null}

      {open ? (
        <div id="main-thread-details" className="border-border border-t">
          {provider && !providerInline ? (
            <ProviderRow info={info} offline={offline} provider={provider} />
          ) : null}
          {provider ? (
            <p className="px-md py-xs" data-testid="mt-provider-help">
              Conversation and tasks stay. Switching starts a fresh native session; prior native
              history is retained.
            </p>
          ) : null}
          {info.session_id && renderIdentity ? renderIdentity(info.session_id) : null}
          <div className="px-md py-xs font-mono" data-testid="topic-index">
            topics:{' '}
            {info.topics.length ? (
              info.topics.map((topic) => (
                <span key={topic.name} className="mr-md" data-testid="topic-line">
                  {topic.name}
                  {topic.status_line ? ` (${topic.status_line})` : ''} [{topic.pending} pending]
                </span>
              ))
            ) : (
              <span>none yet</span>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}
