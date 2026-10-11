import { Link } from '@tanstack/react-router';
import { useMemo, useState, type ReactNode } from 'react';
import { StatusBadge } from '@/components/shared/status-badge';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import type { HITLContext } from '@/lib/contract';
import {
  buildHITLResponse,
  hitlStatus,
  mergeContexts,
  parseHITL,
  reasonNotice,
  requestTools,
  type HITLDraft,
  type HITLState
} from '@/lib/hitl';
import { clipText, hasSummary, isReviewable, statusTone, unavailableNotes } from './evidence';
import { UnavailableMerge, VerifiedMerge, type LoadMergeDetails } from './merge-review';

export interface HITLRequestProps {
  snapshot: HITLState;
  onRespond: (draft: HITLDraft) => void;
  onRefresh: () => void;
  onLoadMergeDetails: LoadMergeDetails;
}

const link = 'text-primary inline-flex min-h-11 items-center underline underline-offset-4';
const field = 'border-input bg-background text-foreground p-sm w-full rounded-none border text-sm';
const choiceBase = 'aria-pressed:border-foreground';
const approveChoice = cn(
  choiceBase,
  'aria-pressed:bg-primary aria-pressed:text-primary-foreground'
);
const neutralChoice = cn(
  choiceBase,
  'aria-pressed:bg-secondary aria-pressed:text-secondary-foreground'
);

function ConversationLink({ context }: { context: HITLContext | undefined }) {
  if (!context?.session_id) return null;
  return context.role === 'main' ? (
    <Link to="/" className={link}>
      Open original conversation
    </Link>
  ) : (
    <Link to="/sessions/$id" params={{ id: context.session_id }} className={link}>
      Open original conversation
    </Link>
  );
}

function errorTitle(snapshot: HITLState): string {
  if (snapshot.uncertain) return 'Decision delivery uncertain';
  if (snapshot.error?.startsWith('Could not refresh')) return 'Request not up to date';
  return 'Response not accepted';
}

const decisionLabel = (verb: string, tool: { name: string; call_id: string }) =>
  `${verb} ${tool.name}, call ${tool.call_id}`;

/**
 * One native approval or question. It owns the draft (decisions, reasons, answers) and nothing
 * else: reads, the submit lock and polling live in the shared per-request state, so the inbox,
 * chat and health mounts of one request agree. Every control is off while the state cannot take
 * a decision, and the server decides whether a request is answerable.
 */
export function HITLRequest({
  snapshot,
  onRespond,
  onRefresh,
  onLoadMergeDetails
}: HITLRequestProps) {
  const [decisions, setDecisions] = useState<HITLDraft['decisions']>({});
  const [reasons, setReasons] = useState<Record<string, string>>({});
  const [answers, setAnswers] = useState<string[][]>([]);
  const { view } = snapshot;
  const rawPayload = view?.request.payload;
  const payload = useMemo(() => parseHITL(rawPayload), [rawPayload]);
  const mergeReviews = useMemo(() => (view ? mergeContexts(view) : []), [view]);
  const decisionOf = (id: string) => (Object.hasOwn(decisions, id) ? decisions[id] : undefined);

  const disabled =
    snapshot.busy ||
    snapshot.stale ||
    snapshot.uncertain ||
    !view?.answerable ||
    !view.writes_enabled ||
    !!view.response;

  const unreviewedApproval = mergeReviews.some(
    (ctx) => decisionOf(ctx.toolId) === 'approve' && !isReviewable(ctx.facts)
  );
  const validation = useMemo(() => {
    if (unreviewedApproval)
      return 'This merge summary is stale or unavailable. Reject the call or refresh the request.';
    try {
      buildHITLResponse(rawPayload, { decisions, reasons, answers });
      return null;
    } catch (error) {
      return (error as Error).message;
    }
  }, [unreviewedApproval, rawPayload, decisions, reasons, answers]);

  function choose(index: number, choice: string, multiple: boolean) {
    setAnswers((all) => {
      const next = [...all];
      const current = next[index] ?? [];
      next[index] = multiple
        ? current.includes(choice)
          ? current.filter((a) => a !== choice)
          : [...current, choice]
        : [choice];
      return next;
    });
  }

  function submit() {
    // Only an approval backed by reviewable facts carries the digest the owner saw.
    const reviewedContext = Object.fromEntries(
      mergeReviews.flatMap(({ toolId, facts }) =>
        decisionOf(toolId) === 'approve' && isReviewable(facts)
          ? [[toolId, facts.summary_digest]]
          : []
      )
    );
    onRespond({ decisions, reasons, answers, reviewedContext });
  }

  if (!view) {
    return (
      <section aria-label="Session input" className="gap-sm p-md grid">
        {snapshot.error ? (
          <ErrorRecovery
            title={errorTitle(snapshot)}
            message={snapshot.error}
            onRetry={onRefresh}
            retryLabel="Refresh status"
            pending={snapshot.busy}
          />
        ) : (
          <p role="status" className="text-muted-foreground">
            Loading session input…
          </p>
        )}
      </section>
    );
  }

  const { context } = view;
  const reasonText = (id: string) => (Object.hasOwn(reasons, id) ? reasons[id] : '');
  const tools = payload ? requestTools(payload) : [];
  let body: ReactNode;
  if (!payload) {
    body = <p role="alert">Malformed or unsupported request. Controls are unavailable.</p>;
  } else if (payload.type === 'tool_approval_request') {
    body = (
      <>
        {payload.hint ? <p className="text-muted-foreground">Agent note: {payload.hint}</p> : null}
        {tools.map((tool) => {
          const ctx = mergeReviews.find((item) => item.toolId === tool.id);
          const decision = decisionOf(tool.id);
          return (
            <div key={tool.id} className="gap-sm grid">
              {ctx ? (
                ctx.facts && hasSummary(ctx.facts) ? (
                  <VerifiedMerge
                    facts={ctx.facts}
                    conversation={<ConversationLink context={context} />}
                    load={onLoadMergeDetails}
                  />
                ) : (
                  <UnavailableMerge toolId={tool.id} notes={unavailableNotes(view, tool.id)} />
                )
              ) : null}
              <fieldset
                disabled={disabled}
                className="gap-sm border-border pt-sm grid min-w-0 border-t"
              >
                <legend className="pr-sm font-medium">{tool.name}</legend>
                <p className="text-muted-foreground">
                  Call: <code>{tool.call_id}</code>
                </p>
                <pre className="bg-muted p-sm max-h-64 overflow-auto break-words whitespace-pre-wrap">
                  {clipText(JSON.stringify(tool.args, null, 2), 16384)}
                </pre>
                <div className="gap-sm flex flex-wrap">
                  <Button
                    variant="outline"
                    className={approveChoice}
                    aria-label={decisionLabel('Approve', tool)}
                    aria-pressed={decision === 'approve'}
                    disabled={!!ctx && !isReviewable(ctx.facts)}
                    onClick={() => {
                      setDecisions((d) => ({ ...d, [tool.id]: 'approve' }));
                      setReasons((r) =>
                        Object.fromEntries(Object.entries(r).filter(([id]) => id !== tool.id))
                      );
                    }}
                  >
                    Approve
                  </Button>
                  <Button
                    variant="outline"
                    className={neutralChoice}
                    aria-label={decisionLabel('Reject', tool)}
                    aria-pressed={decision === 'reject'}
                    onClick={() => setDecisions((d) => ({ ...d, [tool.id]: 'reject' }))}
                  >
                    Reject
                  </Button>
                </div>
                {decision === 'reject' ? (
                  <label className="gap-xs grid">
                    Rejection reason (optional)
                    <textarea
                      className={field}
                      maxLength={8192}
                      rows={2}
                      value={reasonText(tool.id)}
                      onChange={(event) => {
                        const value = event.currentTarget.value;
                        setReasons((r) => ({ ...r, [tool.id]: value }));
                      }}
                    />
                  </label>
                ) : null}
              </fieldset>
            </div>
          );
        })}
        <p className="text-muted-foreground">{reasonNotice(context?.provider)}</p>
      </>
    );
  } else {
    body = (
      <>
        {payload.questions.map((question, i) => (
          <fieldset
            key={i}
            disabled={disabled}
            className="gap-sm border-border pt-sm grid min-w-0 border-t"
          >
            <legend className="pr-sm font-medium">{question.question}</legend>
            {question.choices?.length ? (
              <>
                <p className="text-muted-foreground">
                  {question.multiple ? 'Choose one or more.' : 'Choose one.'}
                </p>
                <div className="gap-sm flex flex-wrap">
                  {question.choices.map((choice) => (
                    <Button
                      key={choice}
                      variant="outline"
                      className={approveChoice}
                      aria-pressed={answers[i]?.includes(choice) ?? false}
                      onClick={() => choose(i, choice, question.multiple)}
                    >
                      {choice}
                    </Button>
                  ))}
                </div>
              </>
            ) : (
              <label className="gap-xs grid">
                Your answer
                <textarea
                  className={field}
                  maxLength={8192}
                  rows={3}
                  value={answers[i]?.[0] ?? ''}
                  onChange={(event) => {
                    const value = event.currentTarget.value;
                    setAnswers((all) => {
                      const next = [...all];
                      next[i] = [value];
                      return next;
                    });
                  }}
                />
              </label>
            )}
          </fieldset>
        ))}
      </>
    );
  }

  const receipt = view.response;
  return (
    <section
      aria-label="Session input"
      className="gap-sm p-md text-foreground grid text-sm break-words"
    >
      <header className="gap-xs grid">
        <h3 className="font-medium">
          {payload?.type === 'ask_user_request' ? 'Questions for you' : 'Session needs input'}
        </h3>
        <p role="status">
          <StatusBadge label={hitlStatus(view)} tone={statusTone(view)} />
        </p>
      </header>
      <p className="text-muted-foreground">
        {context?.project_id ? (
          <>
            <Link to="/projects/$id" params={{ id: context.project_id }} className={link}>
              {context.project_name ?? 'Project'}
            </Link>{' '}
            ·{' '}
          </>
        ) : null}
        {context?.title ?? 'Observed session'}
      </p>
      <details>
        <summary className="py-sm min-h-11 cursor-pointer">Session and task</summary>
        <p>
          Session: <code>{view.request.outer.runtime_session_id}</code>
        </p>
        <p>
          Task: <code>{view.request.outer.task_id}</code>
        </p>
      </details>
      {context?.session_id ? (
        <div>
          <ConversationLink context={context} />
        </div>
      ) : (
        <p className="text-muted-foreground">This observed session has no Mainloop conversation.</p>
      )}
      {view.route_request_id !== view.request.id ? (
        <div>
          <Link to="/" search={{ hitl: view.route_request_id }} className={link}>
            Open continuation request
          </Link>
        </div>
      ) : null}
      {view.unavailable_reason ? (
        <p className="text-muted-foreground">{view.unavailable_reason}</p>
      ) : null}
      {!view.writes_enabled && !receipt ? <p>Responding disabled by the server.</p> : null}
      {body}
      {receipt ? (
        <section aria-label="Recorded response" className="gap-sm grid">
          <h4 className="font-medium">Your recorded response</h4>
          {receipt.response.type === 'tool_approval_response'
            ? receipt.response.approvals.map((approval) => (
                <div key={approval.id}>
                  <p>
                    <code>{approval.id}</code>: {approval.approved ? 'Approved' : 'Rejected'}
                  </p>
                  {approval.rejection_reason ? (
                    <p className="whitespace-pre-wrap">{approval.rejection_reason}</p>
                  ) : null}
                </div>
              ))
            : receipt.response.answers.map((answer, i) => (
                <p key={i}>
                  Answer {i + 1}: {answer.answer.join(', ')}
                </p>
              ))}
          <p className="text-muted-foreground">
            Delivery status applies to this decision, not to completion of the task.
          </p>
        </section>
      ) : payload ? (
        <>
          {validation && !disabled ? <p className="text-muted-foreground">{validation}</p> : null}
          <div>
            <Button disabled={disabled || !!validation} onClick={submit}>
              {snapshot.busy ? 'Recording…' : 'Send response'}
            </Button>
          </div>
        </>
      ) : null}
      <details>
        <summary className="py-sm min-h-11 cursor-pointer">Request data (agent supplied)</summary>
        <pre className="bg-muted p-sm max-h-64 overflow-auto break-words whitespace-pre-wrap">
          {clipText(JSON.stringify(view.request.payload, null, 2))}
        </pre>
      </details>
      {snapshot.error ? (
        <ErrorRecovery title={errorTitle(snapshot)} message={snapshot.error} />
      ) : null}
      <div>
        <Button variant="outline" disabled={snapshot.busy} onClick={onRefresh}>
          Refresh status
        </Button>
      </div>
    </section>
  );
}
