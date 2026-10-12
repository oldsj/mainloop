import { Link } from '@tanstack/react-router';
import { Check } from 'lucide-react';
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
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
import {
  clipText,
  evidenceKey,
  hasSummary,
  isReviewable,
  statusTone,
  unavailableNotes,
  viewProblem
} from './evidence';
import { ClampedText } from './clamped-text';
import { receiptLine } from './receipt';
import { UnavailableMerge, VerifiedMerge, type LoadMergeDetails } from './merge-review';

export interface HITLRequestProps {
  snapshot: HITLState;
  onRespond: (draft: HITLDraft) => void;
  onRefresh: () => void;
  onLoadMergeDetails: LoadMergeDetails;
  /** When this browser sent the recorded response (ISO time), for the receipt line. */
  answeredAt?: string | null;
}

const link = 'text-primary inline-flex min-h-11 items-center underline underline-offset-4';
const field = 'border-input bg-background text-foreground p-sm w-full rounded-none border text-sm';
const choiceBase = 'aria-pressed:border-foreground';
const approveChoice = cn(
  choiceBase,
  'aria-pressed:bg-primary aria-pressed:text-primary-foreground'
);
// A selected Reject fills as strongly as a selected Approve, so the two read the same at a glance.
const rejectChoice = cn(
  choiceBase,
  'aria-pressed:bg-destructive aria-pressed:text-destructive-foreground'
);
// A choice is a sentence the agent wrote, up to 4096 characters: it must wrap, not scroll the card.
const wrapChoice = 'h-auto min-w-0 max-w-full py-sm text-left whitespace-normal break-words';

function ConversationLink({
  context,
  label = 'Open original conversation'
}: {
  context: HITLContext | undefined;
  label?: string;
}) {
  if (!context?.session_id) return null;
  return context.role === 'main' ? (
    <Link to="/" className={link}>
      {label}
    </Link>
  ) : (
    <Link to="/sessions/$id" params={{ id: context.session_id }} className={link}>
      {label}
    </Link>
  );
}

/** After a clean re-read that shows no decision, the lock stays but the copy says what to do. */
const notRecorded = (snapshot: HITLState): boolean =>
  snapshot.uncertain && !snapshot.stale && !!snapshot.view && !snapshot.view.response;

function errorMessage(snapshot: HITLState): string {
  if (notRecorded(snapshot)) return 'Reload to answer again.';
  return snapshot.error ?? '';
}

/** The record without the given keys. */
function without<T>(record: Record<string, T>, keys: string[]): Record<string, T> {
  return Object.fromEntries(Object.entries(record).filter(([key]) => !keys.includes(key)));
}

function errorTitle(snapshot: HITLState): string {
  if (notRecorded(snapshot)) return 'No decision was recorded';
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
  onLoadMergeDetails,
  answeredAt = null
}: HITLRequestProps) {
  const [chosen, setDecisions] = useState<HITLDraft['decisions']>({});
  // The evidence each Approve was chosen against. An Approve counts only while it still matches.
  const [approvedKeys, setApprovedKeys] = useState<Record<string, string>>({});
  const [reasons, setReasons] = useState<Record<string, string>>({});
  const [answers, setAnswers] = useState<string[][]>([]);
  // The Approvals whose evidence moved on since the owner chose them, so the owner can review again.
  const [evidenceChanged, setEvidenceChanged] = useState<string[]>([]);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const receiptRef = useRef<HTMLParagraphElement>(null);
  // Set by Send. Focus then moves to the receipt once it is recorded, never to <body>.
  const sending = useRef(false);
  const { view } = snapshot;
  const recorded = !!view?.response;
  useEffect(() => {
    if (!recorded || !sending.current) return;
    sending.current = false;
    receiptRef.current?.focus();
  }, [recorded]);
  const problem = view ? viewProblem(view) : null;
  const rawPayload = view && !problem ? view.request.payload : undefined;
  const payload = useMemo(() => parseHITL(rawPayload), [rawPayload]);
  const mergeReviews = useMemo(
    () => (view && !problem ? mergeContexts(view) : []),
    [view, problem]
  );
  const decisions = useMemo(() => {
    const keyOf = (id: string) =>
      evidenceKey(mergeReviews.find((item) => item.toolId === id)?.facts ?? null);
    return Object.fromEntries(
      Object.entries(chosen).filter(
        ([id, decision]) =>
          decision !== 'approve' ||
          (Object.hasOwn(approvedKeys, id) && approvedKeys[id] === keyOf(id))
      )
    ) as HITLDraft['decisions'];
  }, [chosen, approvedKeys, mergeReviews]);
  const decisionOf = (id: string) => (Object.hasOwn(decisions, id) ? decisions[id] : undefined);
  // An Approve that the filter above drops is removed from the draft, not just hidden, so it cannot
  // come back if the evidence returns to what it was. The owner is asked to approve again. React
  // applies this during render and re-renders once, so the next pass finds nothing dropped.
  const dropped = Object.keys(chosen).filter(
    (id) => chosen[id] === 'approve' && decisions[id] !== 'approve'
  );
  if (dropped.length > 0) {
    setDecisions((d) => without(d, dropped) as HITLDraft['decisions']);
    setApprovedKeys((k) => without(k, dropped));
    setEvidenceChanged(dropped);
  }

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
    // Send is disabled, then replaced by the receipt. Focus moves to the heading first so it is not lost.
    sending.current = true;
    headingRef.current?.focus();
    onRespond({ decisions, reasons, answers, reviewedContext });
  }

  if (!view || problem) {
    return (
      <section aria-label="Session input" className="gap-sm p-md grid">
        {problem ? (
          <ErrorRecovery
            title="Request cannot be shown"
            message={`The server sent a request this page cannot display: ${problem}. Controls are unavailable.`}
            onRetry={onRefresh}
            retryLabel="Refresh status"
            pending={snapshot.busy}
          />
        ) : snapshot.error ? (
          <ErrorRecovery
            title={errorTitle(snapshot)}
            message={errorMessage(snapshot)}
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
        {evidenceChanged.length > 0 ? (
          <p role="alert">The merge evidence changed. Review it and approve again.</p>
        ) : null}
        {tools.map((tool) => {
          const ctx = mergeReviews.find((item) => item.toolId === tool.id);
          const decision = decisionOf(tool.id);
          return (
            <div key={tool.id} className="gap-sm grid">
              {ctx ? (
                ctx.facts && hasSummary(ctx.facts) ? (
                  <VerifiedMerge
                    key={evidenceKey(ctx.facts)}
                    facts={ctx.facts}
                    conversation={
                      <ConversationLink context={context} label="Open agent conversation" />
                    }
                    load={onLoadMergeDetails}
                  />
                ) : (
                  <UnavailableMerge
                    toolId={tool.id}
                    reason={
                      ctx.facts?.summary && !hasSummary(ctx.facts)
                        ? 'The summary and the request context name different pull requests.'
                        : null
                    }
                    notes={unavailableNotes(view, tool.id)}
                  />
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
                <ClampedText
                  what="arguments"
                  text={clipText(JSON.stringify(tool.args, null, 2), 16384)}
                />
                <div className="gap-sm flex flex-wrap">
                  <Button
                    variant="outline"
                    className={approveChoice}
                    aria-label={decisionLabel('Approve', tool)}
                    aria-pressed={decision === 'approve'}
                    disabled={!!ctx && !isReviewable(ctx.facts)}
                    onClick={() => {
                      setDecisions((d) => ({ ...d, [tool.id]: 'approve' }));
                      setApprovedKeys((k) => ({
                        ...k,
                        [tool.id]: evidenceKey(ctx?.facts ?? null)
                      }));
                      setReasons((r) =>
                        Object.fromEntries(Object.entries(r).filter(([id]) => id !== tool.id))
                      );
                      setEvidenceChanged((ids) => ids.filter((id) => id !== tool.id));
                    }}
                  >
                    {decision === 'approve' ? (
                      <Check className="size-4" aria-hidden="true" />
                    ) : null}
                    {decision === 'approve' ? 'Approved' : 'Approve'}
                  </Button>
                  <Button
                    variant="outline"
                    className={rejectChoice}
                    aria-label={decisionLabel('Reject', tool)}
                    aria-pressed={decision === 'reject'}
                    onClick={() => {
                      setDecisions((d) => ({ ...d, [tool.id]: 'reject' }));
                      setEvidenceChanged((ids) => ids.filter((id) => id !== tool.id));
                    }}
                  >
                    {decision === 'reject' ? <Check className="size-4" aria-hidden="true" /> : null}
                    {decision === 'reject' ? 'Rejected' : 'Reject'}
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
                      className={cn(approveChoice, wrapChoice)}
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
        <h3 ref={headingRef} tabIndex={-1} className="font-medium focus:outline-none">
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
          <p
            ref={receiptRef}
            tabIndex={-1}
            data-testid="hitl-receipt"
            className="font-medium focus:outline-none"
          >
            {receiptLine(view, answeredAt)}
          </p>
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
        {/* Already folded by the disclosure, so it shows whole and grows with the page. */}
        <pre className="bg-muted p-sm [overflow-wrap:anywhere] break-words whitespace-pre-wrap">
          {clipText(JSON.stringify(view.request.payload, null, 2))}
        </pre>
      </details>
      {snapshot.error ? (
        <ErrorRecovery title={errorTitle(snapshot)} message={errorMessage(snapshot)} />
      ) : null}
      <div>
        <Button variant="outline" disabled={snapshot.busy} onClick={onRefresh}>
          Refresh status
        </Button>
      </div>
    </section>
  );
}
