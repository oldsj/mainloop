import { useState } from 'react';
import { agentLabel } from '@/lib/agentLabel';
import type { Session } from '@/lib/contract';
import { deliveryNotices } from '@/lib/delivery';
import { useConnection } from '@/shell/ConnectionBanner';
import { liveWord, nativeProviderFor } from './model';
import { useNativeSession, useProviders } from './queries';

const DOT_CLASS: Record<string, string> = {
  ready: 'bg-term-green',
  working: 'bg-term-cyan',
  'last message failed': 'bg-term-red',
  'last message unconfirmed': 'bg-term-yellow'
};

/**
 * The session's identity: which agent, which model, and whether the last message got through.
 * One summary line is always shown; the other fields sit behind the details toggle. The native
 * read polls only while a turn is in flight (see `nativePollInterval`).
 */
export function IdentityStrip({ session }: { session: Session }) {
  const [open, setOpen] = useState(false);
  const offline = useConnection().status === 'offline';
  const native = useNativeSession(session.id);
  const providers = useProviders();

  const info = native.data;
  if (!info) return null;

  const deliveries = info.deliveries;
  const live = liveWord({ native: info, offline });
  const last = deliveries.at(-1);
  const problem = last ? deliveryNotices(deliveries).get(last.message_id) : undefined;
  const provider = agentLabel(nativeProviderFor(info.kind, providers.data ?? []));
  const model = info.model ?? session.model;

  return (
    <section
      data-testid="identity-strip"
      aria-label="Session identity"
      className="gap-xs border-border bg-card text-muted-foreground px-md py-sm border-b font-mono text-xs"
    >
      <div className="gap-sm flex items-center">
        <span
          className={`size-2 shrink-0 ${DOT_CLASS[live] ?? 'bg-term-fg-muted'}`}
          aria-hidden="true"
        />
        <span className="text-foreground">{provider}</span>
        <span>·</span>
        <span className="truncate">{(model ?? 'model pending').replace(/^claude-/, '')}</span>
        <span>·</span>
        <span>{live}</span>
        {info.topic ? <span className="hidden truncate sm:inline">· #{info.topic}</span> : null}
        <button
          type="button"
          data-testid="identity-toggle"
          aria-expanded={open}
          aria-controls="identity-details"
          onClick={() => setOpen((value) => !value)}
          className="hover:text-accent-foreground px-sm ml-auto min-h-11 shrink-0"
        >
          details {open ? '▴' : '▾'}
        </button>
      </div>

      {open ? (
        <div id="identity-details" className="gap-x-md gap-y-xs flex flex-wrap">
          <span>
            agent{' '}
            <b data-testid="id-kind" className="text-foreground">
              {info.kind}
            </b>
          </span>
          <span>
            model{' '}
            <b data-testid="id-model" className="text-foreground">
              {model ?? 'unknown yet'}
            </b>
          </span>
          <span>
            kagent agent{' '}
            <b data-testid="id-agent" className="text-foreground">
              {info.agent_name}
            </b>
          </span>
          <span>
            session{' '}
            <b data-testid="id-session" className="text-foreground">
              {info.kagent_session_id?.slice(0, 8) ?? 'pending'}
            </b>{' '}
            {info.session_state ?? 'unknown'}
          </span>
          <span>
            turns{' '}
            <b data-testid="id-turns" className="text-foreground">
              {info.turns}
            </b>
          </span>
          {info.role && info.role !== 'agent' ? (
            <span>
              role{' '}
              <b data-testid="id-role" className="text-foreground">
                {info.role}
              </b>
            </span>
          ) : null}
          {info.parent_session_id ? (
            <span>
              parent{' '}
              <b data-testid="id-parent" className="text-foreground">
                {info.parent_session_id.slice(0, 8)}
              </b>
            </span>
          ) : null}
          {info.topic ? (
            <span>
              topic{' '}
              <b data-testid="id-topic" className="text-foreground">
                {info.topic}
              </b>
            </span>
          ) : null}
        </div>
      ) : null}

      {info.note ? (
        <p data-testid="id-note" className="text-term-yellow">
          {info.note}
        </p>
      ) : null}

      {problem ? (
        <p
          data-testid="id-delivery-problem"
          className={problem.state === 'failed' ? 'text-destructive' : 'text-term-yellow'}
        >
          {problem.label}
          {problem.reason ? `: ${problem.reason}` : ''}
        </p>
      ) : null}

      {open && deliveries.length > 0 ? (
        <p data-testid="id-deliveries">
          deliveries: {deliveries.length}{' '}
          {deliveries.slice(-3).map((delivery) => (
            <span
              key={delivery.message_id}
              className="mr-sm"
              title={delivery.detail ?? delivery.evidence_ref ?? ''}
            >
              {delivery.state}
            </span>
          ))}
        </p>
      ) : null}
    </section>
  );
}
