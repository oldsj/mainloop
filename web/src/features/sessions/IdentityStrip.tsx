import { useState, type ReactNode } from 'react';
import { ShortId } from '@/features/workspaces/ShortId';
import { agentLabel } from '@/lib/agentLabel';
import type { Session } from '@/lib/contract';
import { deliveryNotices } from '@/lib/delivery';
import { useConnection } from '@/shell/ConnectionBanner';
import { liveWord, nativeProviderFor } from './model';
import { useNativeSession, useProviders } from './queries';

const DOT_CLASS: Record<string, string> = {
  live: 'bg-term-green',
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
  const live = liveWord({ native: info, offline, status: session.status });
  const last = deliveries.at(-1);
  const problem = last ? deliveryNotices(deliveries).get(last.message_id) : undefined;
  const provider = agentLabel(nativeProviderFor(info.kind, providers.data ?? []));
  const profiles = providers.data ?? [];
  const profile = profiles.find(
    (item) => item.id === info.kind || item.aliases.includes(info.kind)
  );
  const model = info.model_override ?? profile?.default_model ?? info.model ?? session.model;
  const effort = info.effort_override ?? profile?.default_effort;

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
        {effort ? <span className="shrink-0">· {effort} effort</span> : null}
        <span>·</span>
        <span>{live}</span>
        {info.topic ? <span className="hidden truncate sm:inline">· #{info.topic}</span> : null}
        <button
          type="button"
          data-testid="identity-toggle"
          aria-expanded={open}
          aria-controls={open ? 'identity-details' : undefined}
          onClick={() => setOpen((value) => !value)}
          className="hover:text-accent-foreground px-sm ml-auto min-h-11 shrink-0"
        >
          details {open ? '▴' : '▾'}
        </button>
      </div>

      {open ? (
        <dl id="identity-details" className="gap-x-md gap-y-xs flex flex-wrap">
          <Field label="agent" testId="id-kind">
            {info.kind}
          </Field>
          <Field label="model" testId="id-model">
            {model ?? 'unknown yet'}
          </Field>
          {effort ? (
            <Field label="effort" testId="id-effort">
              {effort}
            </Field>
          ) : null}
          <Field label="kagent agent" testId="id-agent">
            {info.agent_name}
          </Field>
          <Field label="session" testId="id-session">
            {info.kagent_session_id ? (
              <ShortId id={info.kagent_session_id} label="session" />
            ) : (
              'pending'
            )}{' '}
            {info.session_state ?? 'unknown'}
          </Field>
          <Field label="turns" testId="id-turns">
            {info.turns}
          </Field>
          {info.role && info.role !== 'agent' ? (
            <Field label="role" testId="id-role">
              {info.role}
            </Field>
          ) : null}
          {info.parent_session_id ? (
            <Field label="parent" testId="id-parent">
              <ShortId id={info.parent_session_id} label="parent session" />
            </Field>
          ) : null}
          {info.topic ? (
            <Field label="topic" testId="id-topic">
              {info.topic}
            </Field>
          ) : null}
        </dl>
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

/** One label and value pair in the details list, so a screen reader reads each value with its label. */
function Field({
  label,
  testId,
  children
}: {
  label: string;
  testId: string;
  children: ReactNode;
}) {
  return (
    <div className="gap-xs flex">
      <dt>{label}</dt>
      <dd data-testid={testId} className="text-foreground">
        {children}
      </dd>
    </div>
  );
}
