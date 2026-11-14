import { Info } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { SessionBar } from '@/components/shared/session-bar';
import { cn } from '@/components/ui/cn';
import { agentLabel } from '@/lib/agentLabel';
import { barStatus, threadWords, type LiveStatus } from '@/lib/barStatus';
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

/** The words for a status. A failed or unconfirmed last delivery is never called ready. */
export function statusWords(status: ThreadStatus): string {
  return threadWords(status);
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
  /**
   * The live view under this header: its status joins the thread's into one word, and its wake and
   * model controls sit in this row, so the live view draws no row of its own.
   */
  live?: { status: LiveStatus; controls: ReactNode };
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
        className="border-border bg-background text-foreground px-sm min-h-control border text-sm disabled:opacity-50"
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
  const buttonClass =
    'border-border text-foreground px-md min-h-control border disabled:opacity-50';

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
  provider,
  live
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
    <SessionBar
      data-testid="main-thread-header"
      status={barStatus(status, live?.status ?? null)}
      statusTestId="mt-state"
      lead={
        pending > 0 ? (
          <>
            <span aria-hidden="true">·</span>
            <span data-testid="mt-pending">{pending} pending</span>
          </>
        ) : null
      }
      trail={
        <>
          {onStop && native?.turn_in_flight ? (
            <button
              type="button"
              className="border-border-subtle text-foreground hover:text-term-red px-md min-h-control border disabled:opacity-50"
              disabled={stopping || offline}
              onClick={onStop}
              data-testid="mt-stop-turn"
            >
              {stopping ? 'stopping…' : 'stop'}
            </button>
          ) : null}
          {provider ? <ProviderSelect info={info} provider={provider} /> : null}
          {/* The live view's own model control replaces the thread's model text. */}
          {live ? live.controls : <span data-testid="mt-model">{model}</span>}
          <button
            type="button"
            className={cn(
              'hover:text-foreground min-h-control w-control inline-flex items-center justify-center',
              open && 'text-foreground'
            )}
            aria-label="Session info"
            aria-expanded={open}
            aria-controls="main-thread-details"
            onClick={() => setOpen((value) => !value)}
            data-testid="mt-details-toggle"
          >
            <Info className="size-4" aria-hidden="true" />
          </button>
        </>
      }
    >
      {provider && providerInline ? (
        <ProviderRow info={info} offline={offline} provider={provider} />
      ) : null}

      {open ? (
        <div id="main-thread-details" className="border-border-subtle border-t">
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
    </SessionBar>
  );
}
