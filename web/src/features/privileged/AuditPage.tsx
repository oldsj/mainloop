import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { PAGE } from '@/components/shared/measure';
import { BACK_LINK, BackIcon, PageHeader } from '@/components/shared/page-header';
import { cn } from '@/components/ui/cn';
import { messageTime } from '@/lib/time';
import {
  requester,
  STATE,
  type PrivilegedActionState,
  type PrivilegedActionSummary
} from './model';
import { usePrivilegedAction, usePrivilegedActions } from './queries';

const TONE = {
  open: 'text-status-working',
  ok: 'text-foreground',
  bad: 'text-status-failed',
  off: 'text-muted-foreground'
} as const;

function StateLabel({ state }: { state: PrivilegedActionState }) {
  const s = STATE[state];
  return <span className={cn('flex-none text-xs font-medium', TONE[s.tone])}>{s.label}</span>;
}

/**
 * Every privileged action an agent asked for, newest first: what it was, who asked, and how it
 * ended. Read-only; approving happens on the Needs-you card.
 */
export function PrivilegedAuditPage() {
  const actions = usePrivilegedActions();
  return (
    <div className={`${PAGE} gap-md`}>
      <PageHeader
        title="Privileged actions"
        description="What agents asked to do with credentials they do not hold, and what came of it. Approve or deny from Needs you."
        back={
          <Link to="/" aria-label="Back to the main chat" className={BACK_LINK}>
            <BackIcon />
          </Link>
        }
      />
      {actions.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading…
        </p>
      ) : actions.isError ? (
        <ErrorRecovery
          title="Could not load privileged actions"
          message="Mainloop could not read the audit. Try again."
          onRetry={() => void actions.refetch()}
          pending={actions.isFetching}
        />
      ) : actions.data.length === 0 ? (
        <p className="text-muted-foreground text-sm">No agent has asked for a privileged action.</p>
      ) : (
        <ol className="gap-sm flex flex-col" aria-label="Privileged actions">
          {actions.data.map((a) => (
            <Row key={a.id} action={a} />
          ))}
        </ol>
      )}
    </div>
  );
}

function Row({ action: a }: { action: PrivilegedActionSummary }) {
  return (
    <li className="border-border-subtle pb-sm border-b">
      <Link
        to="/privileged/$id"
        params={{ id: a.id }}
        className="gap-xs hover:bg-muted p-xs flex flex-col"
      >
        <span className="gap-sm flex min-w-0 items-baseline">
          <span className="min-w-0 flex-1 truncate text-sm font-medium">{a.title}</span>
          <StateLabel state={a.state} />
        </span>
        <span className="text-muted-foreground text-xs">
          {requester(a)} · {messageTime(a.created_at)}
        </span>
        {(a.last_error ?? a.decision_note) ? (
          <span className="text-muted-foreground text-xs [overflow-wrap:anywhere]">
            {a.last_error ?? a.decision_note}
          </span>
        ) : null}
      </Link>
    </li>
  );
}

/** One action: the card as the owner saw it, the agent's claim, the outcome and every step. */
export function PrivilegedActionPage({ id }: { id: string }) {
  const action = usePrivilegedAction(id);
  return (
    <div className={`${PAGE} gap-md`}>
      <PageHeader
        title={action.data?.action.title ?? 'Privileged action'}
        back={
          <Link to="/privileged" aria-label="Back to privileged actions" className={BACK_LINK}>
            <BackIcon />
          </Link>
        }
      />
      {action.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading…
        </p>
      ) : action.isError ? (
        <ErrorRecovery
          title="Could not load the action"
          message="It may not exist, or Mainloop could not read it. Try again."
          onRetry={() => void action.refetch()}
          pending={action.isFetching}
        />
      ) : (
        <Detail d={action.data} />
      )}
    </div>
  );
}

function Detail({ d }: { d: NonNullable<ReturnType<typeof usePrivilegedAction>['data']> }) {
  const a = d.action;
  const facts: [string, string | null][] = [
    ['State', STATE[a.state].label],
    ['Type', a.type],
    ['Asked by', requester(a)],
    ['Session', a.requester_session_id],
    ['Asked', messageTime(a.created_at)],
    ['Expires', messageTime(a.expires_at)],
    ['Decided', a.decided_at ? messageTime(a.decided_at) : null],
    [
      'Signed',
      d.approval
        ? `with Touch ID on ${d.approval.device_id}, ${messageTime(d.approval.signed_at)}`
        : null
    ],
    ['Finished', a.finished_at ? messageTime(a.finished_at) : null],
    ['Note', a.decision_note],
    ['Error', a.last_error],
    ['Digest', d.digest]
  ];
  return (
    <>
      <dl className="gap-x-md gap-y-xs grid grid-cols-[auto_1fr] text-sm">
        {facts
          .filter(([, v]) => v)
          .map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="min-w-0 [overflow-wrap:anywhere]">{v}</dd>
            </div>
          ))}
      </dl>
      <section className="gap-xs flex flex-col" aria-label="Agent's reason">
        <h2 className="text-sm font-medium">Agent&apos;s reason (its claim)</h2>
        <p className="text-sm [overflow-wrap:anywhere] whitespace-pre-wrap">{a.reason}</p>
      </section>
      {d.body ? (
        <section className="gap-xs flex flex-col" aria-label="What was approved">
          <h2 className="text-sm font-medium">The card</h2>
          <p className="font-mono text-xs [overflow-wrap:anywhere] whitespace-pre-wrap">{d.body}</p>
        </section>
      ) : null}
      <section className="gap-xs flex flex-col" aria-label="Steps">
        <h2 className="text-sm font-medium">Steps</h2>
        <ol className="gap-xs flex flex-col text-xs">
          {d.events.map((e) => (
            <li key={e.id} className="gap-sm flex">
              <span className="text-muted-foreground flex-none">{messageTime(e.created_at)}</span>
              <span className="flex-none font-medium">{e.step}</span>
              <span className="min-w-0 [overflow-wrap:anywhere]">{e.detail}</span>
            </li>
          ))}
        </ol>
      </section>
    </>
  );
}
