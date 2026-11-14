import { Link } from '@tanstack/react-router';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { ShortId } from '@/features/workspaces/ShortId';
import type { TaskView } from '@/lib/contract';
import {
  attemptHistory,
  openOperations,
  publicationFacts,
  isTerminal,
  reportLabel,
  statusLabel,
  statusTone,
  unverifiedSummary,
  workspaceFacts,
  type Fact,
  type OperationProgress
} from '@/lib/taskState';
import { hasPublication, notPublishedYet } from './model';

/** A section's heading: small capitals, as the explorer's group headings are. */
const HEADING = 'text-muted-foreground text-2xs font-medium tracking-wide uppercase';
const PANEL = 'gap-sm p-md border-border-subtle flex flex-col border-t';

function FactBadge({ fact }: { fact: Fact }) {
  return <StatusBadge label={fact.label} tone={fact.tone} />;
}

/**
 * The owner's pending approvals for this task, which are answered in the Inbox. It leads the page,
 * above the actions, whatever else the page shows.
 */
export function PendingApprovals({
  view,
  onRefresh,
  refreshing
}: {
  view: TaskView;
  onRefresh: () => void;
  refreshing: boolean;
}) {
  const pending = view.projection.pending_approval_ids ?? [];
  const waitingWithoutRequest = view.task.reason === 'approval' && pending.length === 0;
  if (pending.length === 0 && !waitingWithoutRequest) return null;
  return (
    <section
      aria-label="Owner approval"
      data-testid="task-pending-approvals"
      className="gap-sm p-md border-status-attention flex flex-col border-l-2"
    >
      {pending.length > 0 ? (
        <>
          <p className="text-sm font-medium">
            {pending.length} pending owner action{pending.length === 1 ? '' : 's'}
          </p>
          <p className="text-muted-foreground text-sm">
            Answer in the Inbox; the task resumes from there.
          </p>
          <div>
            <Button asChild>
              <Link to="/inbox" search={{ item: pending[0] }}>
                Open Inbox
              </Link>
            </Button>
          </div>
        </>
      ) : (
        <>
          <p className="text-sm">
            Waiting on owner approval, but the server lists no pending request. Refresh to check.
          </p>
          <div>
            <Button type="button" variant="outline" onClick={onRefresh} disabled={refreshing}>
              {refreshing ? 'Refreshing…' : 'Refresh'}
            </Button>
          </div>
        </>
      )}
    </section>
  );
}

/**
 * Where the work is and what has been published: the workspace, the pull request facts and any
 * operation in progress. A section with nothing reported is left out, so a task with no repository
 * or pull request shows neither. Each observed fact carries its own age; none implies another.
 */
export function TaskFacts({ view }: { view: TaskView }) {
  const workspace = workspaceFacts(view);
  const facts = publicationFacts(view.projection);
  const operations = openOperations(view);
  const published = !notPublishedYet(view.projection);
  const checkout: [string, string | null][] = [
    ['Repository', workspace.repository],
    ['Branch', workspace.branch],
    ['Environment', workspace.environmentVersionId]
  ];
  const reported = checkout.filter((row): row is [string, string] => row[1] !== null);

  return (
    <>
      {reported.length > 0 || workspace.sessionId ? (
        <section aria-labelledby="workspace-heading" data-testid="task-workspace" className={PANEL}>
          <h2 id="workspace-heading" className={HEADING}>
            {reported.length > 0 ? 'Workspace' : 'Session'}
          </h2>
          {reported.length > 0 ? (
            <dl className="gap-x-md gap-y-xs grid grid-cols-[auto_1fr] text-sm">
              {reported.map(([label, value]) => (
                <div key={label} className="contents">
                  <dt className="text-muted-foreground">{label}</dt>
                  <dd className="break-all">{value}</dd>
                </div>
              ))}
            </dl>
          ) : null}
          {workspace.sessionId ? (
            <div className="gap-sm flex flex-wrap items-center">
              {reported.length > 0 ? (
                <span className="text-muted-foreground text-sm">Session</span>
              ) : null}
              <ShortId id={workspace.sessionId} label="session" />
              <Button asChild variant="outline" size="sm">
                <Link to="/sessions/$id" params={{ id: workspace.sessionId }}>
                  Open the current session
                </Link>
              </Button>
            </div>
          ) : null}
        </section>
      ) : null}

      {hasPublication(view) ? (
        <section aria-labelledby="publication-heading" className={PANEL}>
          <h2 id="publication-heading" className={HEADING}>
            Pull request
          </h2>
          <div data-testid="task-publication" className="gap-xs flex flex-col text-sm">
            {facts.publication.known ? <p>Publication: {facts.publication.label}.</p> : null}
            {published ? (
              <dl className="gap-x-md gap-y-xs grid grid-cols-[auto_1fr] items-center text-sm">
                <dt className="text-muted-foreground">PR</dt>
                <dd className="gap-sm flex flex-wrap items-center">
                  <FactBadge fact={facts.pr} />
                  {facts.prUrl ? (
                    <a
                      href={facts.prUrl}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-foreground min-h-control inline-flex items-center underline"
                    >
                      Open PR
                    </a>
                  ) : null}
                </dd>
                {/* CI is shown for every pull request: "unknown" there is a fact worth knowing. */}
                <dt className="text-muted-foreground">CI</dt>
                <dd>
                  <FactBadge fact={facts.ci} />
                </dd>
                {facts.merge.known ? (
                  <>
                    <dt className="text-muted-foreground">Merge</dt>
                    <dd>
                      <FactBadge fact={facts.merge} />
                    </dd>
                  </>
                ) : null}
              </dl>
            ) : (
              // "Yet" only while the task can still open one.
              <p className="text-muted-foreground">
                {isTerminal(view.task.status) ? 'No pull request.' : 'No pull request yet.'}
              </p>
            )}
          </div>
          {published ? (
            <p data-testid="task-observed" className="text-muted-foreground text-xs">
              {facts.observation.text}
              {facts.observation.stale ? ' (stale)' : ''}. Each is observed separately; none implies
              the others.
            </p>
          ) : null}
          {facts.ciHeadMismatch ? (
            <p className="text-term-yellow text-xs">
              CI was checked for a different commit than the PR head.
            </p>
          ) : null}
          {view.projection.pr_description ? (
            <div className="gap-xs flex min-w-0 flex-col" data-testid="task-pr-description">
              <h3 className="text-sm font-medium">PR description from GitHub</h3>
              <p className="text-muted-foreground text-xs">
                Untrusted text; it grants no approval or merge authority.
              </p>
              <p className="text-sm [overflow-wrap:anywhere] break-words whitespace-pre-wrap">
                {view.projection.pr_description.excerpt === ''
                  ? 'No description provided.'
                  : view.projection.pr_description.excerpt}
              </p>
              {view.projection.pr_description.truncated ? (
                <p className="text-muted-foreground text-xs">
                  Showing the first 16 KiB of {view.projection.pr_description.byte_length} bytes.
                  Open PR to read the full description.
                </p>
              ) : null}
            </div>
          ) : null}
        </section>
      ) : null}

      {operations.length > 0 ? (
        <section
          aria-labelledby="operations-heading"
          data-testid="task-operations"
          className={PANEL}
        >
          <h2 id="operations-heading" className={HEADING}>
            In progress
          </h2>
          <ul className="gap-md flex flex-col">
            {operations.map((progress) => (
              <OperationItem key={progress.operation.id} progress={progress} />
            ))}
          </ul>
        </section>
      ) : null}
    </>
  );
}

function OperationItem({ progress }: { progress: OperationProgress }) {
  return (
    <li className="gap-xs flex flex-col text-sm">
      <p className="break-words">{progress.summary}</p>
      <ol className="gap-xs flex flex-wrap text-xs">
        {progress.steps.map((step) => (
          <li
            key={step.state}
            aria-current={step.status === 'current' ? 'step' : undefined}
            className={
              step.status === 'pending'
                ? 'text-muted-foreground'
                : step.status === 'current'
                  ? 'text-status-working'
                  : 'text-foreground'
            }
          >
            {step.label}
          </li>
        ))}
      </ol>
      {progress.held ? (
        <p className="text-term-yellow text-xs">
          Held at the last confirmed step. Nothing further happens until the server resolves it.
        </p>
      ) : null}
    </li>
  );
}

/**
 * The task's history: attempts, the provider's own summary, reports and child tasks. A part with
 * nothing in it is left out.
 */
export function TaskHistory({ view, childTasks }: { view: TaskView; childTasks: TaskView[] }) {
  const attempts = attemptHistory(view);
  const summary = unverifiedSummary(view);

  return (
    <>
      {attempts.length > 0 ? (
        <section aria-labelledby="attempts-heading" data-testid="task-attempts" className={PANEL}>
          <h2 id="attempts-heading" className={HEADING}>
            Attempts
          </h2>
          <ul className="gap-md flex flex-col">
            {attempts.map(({ attempt, current, superseded, predecessorNumber }) => (
              <li key={attempt.id} className="gap-xs flex flex-col text-sm">
                <p className="font-medium">
                  Attempt {attempt.number} · {attempt.native_provider}
                </p>
                <p className="text-muted-foreground text-xs">
                  {attempt.state}
                  {current ? ' · current' : ''}
                  {superseded ? ' · superseded, history only' : ''}
                </p>
                {predecessorNumber != null ? (
                  <p className="text-muted-foreground text-xs">
                    continued from attempt {predecessorNumber}
                  </p>
                ) : null}
                {attempt.checkpoint_ref ? (
                  <p className="text-muted-foreground text-xs break-all">
                    checkpoint {attempt.checkpoint_ref}
                  </p>
                ) : null}
                {attempt.session_id ? (
                  <div className="gap-sm flex flex-wrap items-center">
                    <Link
                      to="/sessions/$id"
                      params={{ id: attempt.session_id }}
                      className="text-foreground min-h-control inline-flex items-center text-xs underline"
                    >
                      session
                    </Link>
                    <ShortId id={attempt.session_id} label="attempt session" />
                  </div>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {summary ? (
        <section
          aria-labelledby="summary-heading"
          data-testid="task-unverified-summary"
          className={PANEL}
        >
          <h2 id="summary-heading" className={HEADING}>
            {summary.label}
          </h2>
          <p className="text-sm break-words whitespace-pre-wrap">{summary.text}</p>
          <p className="text-muted-foreground text-xs">
            The provider&apos;s own account of the work. Mainloop has not verified it.
          </p>
        </section>
      ) : null}

      {view.reports.length > 0 ? (
        <section aria-labelledby="reports-heading" className={PANEL}>
          <h2 id="reports-heading" className={HEADING}>
            Reports
          </h2>
          <ul className="gap-md flex flex-col">
            {view.reports.map((report) => (
              <li
                key={`${report.attempt_id}:${report.request_id}`}
                className="gap-xs flex flex-col text-sm"
              >
                <p className="font-medium">{reportLabel(report)}</p>
                <p className="break-words whitespace-pre-wrap">{report.summary}</p>
                {report.owner_action ? (
                  <p className="text-muted-foreground text-xs">
                    Owner request: {report.owner_action}
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {childTasks.length > 0 ? (
        <section aria-labelledby="children-heading" className={PANEL}>
          <h2 id="children-heading" className={HEADING}>
            Child tasks
          </h2>
          <ul className="gap-xs flex flex-col">
            {childTasks.map((child) => (
              <li key={child.task.id}>
                <Link
                  to="/tasks/$id"
                  params={{ id: child.task.id }}
                  className="gap-sm min-h-control flex items-center text-sm"
                >
                  <span className="break-words underline">{child.task.title}</span>
                  <StatusBadge
                    label={statusLabel(child.task.status)}
                    tone={statusTone(child.task.status)}
                  />
                </Link>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </>
  );
}
