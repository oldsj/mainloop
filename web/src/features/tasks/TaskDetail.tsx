import { Link } from '@tanstack/react-router';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { ShortId } from '@/features/workspaces/ShortId';
import type { TaskView } from '@/lib/contract';
import {
  attemptHistory,
  currentAttempt,
  openOperations,
  providerLabel,
  publicationFacts,
  reasonLabel,
  reportLabel,
  statusLabel,
  isTerminal,
  statusTone,
  unverifiedSummary,
  workspaceFacts,
  type Fact,
  type OperationProgress
} from '@/lib/taskState';
import { attemptSettings, notPublishedYet } from './model';
import { useProviders } from './queries';

const HEADING = 'text-muted-foreground text-xs tracking-wide uppercase';
const PANEL = 'gap-sm p-md border-border flex flex-col border-t';

function FactBadge({ fact }: { fact: Fact }) {
  return <StatusBadge label={fact.label} tone={fact.tone} />;
}

/**
 * What the task is doing now: the provider, where the work is checked out, the publication facts
 * and any operation in progress. Each observed fact carries its own age; none implies another.
 */
export function TaskFacts({
  view,
  onRefresh,
  refreshing
}: {
  view: TaskView;
  onRefresh: () => void;
  refreshing: boolean;
}) {
  const pending = view.projection.pending_approval_ids ?? [];
  const attempt = currentAttempt(view);
  const providers = useProviders();
  const workspace = workspaceFacts(view);
  const facts = publicationFacts(view.projection);
  const operations = openOperations(view);
  const waitingWithoutRequest = view.task.reason === 'approval' && pending.length === 0;

  return (
    <>
      {pending.length > 0 || waitingWithoutRequest ? (
        <section
          aria-label="Owner approval"
          data-testid="task-pending-approvals"
          className="gap-sm p-md border-term-yellow/50 flex flex-col border"
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
                <Button asChild variant="outline">
                  <Link to="/inbox" search={{ item: pending[0] }}>
                    Open Inbox
                  </Link>
                </Button>
              </div>
            </>
          ) : (
            <>
              <p className="text-sm">
                Waiting on owner approval, but the server lists no pending request. Refresh to
                check.
              </p>
              <div>
                <Button type="button" variant="outline" onClick={onRefresh} disabled={refreshing}>
                  {refreshing ? 'Refreshing…' : 'Refresh'}
                </Button>
              </div>
            </>
          )}
        </section>
      ) : null}

      <section aria-labelledby="provider-heading" className={PANEL}>
        <h2 id="provider-heading" className={HEADING}>
          [PROVIDER]
        </h2>
        <p data-testid="task-provider" className="text-sm">
          {providerLabel(view)}
          {attemptSettings(attempt, providers.data ?? [])}
          {attempt ? ` · attempt ${attempt.number}` : ''}
        </p>
        <p className="text-muted-foreground text-sm">
          Status: <span data-testid="task-status-reason">{statusLabel(view.task.status)}</span>
          {view.task.reason ? `, ${reasonLabel(view.task.reason)}` : ''}
        </p>
      </section>

      <section aria-labelledby="workspace-heading" className={PANEL}>
        <h2 id="workspace-heading" className={HEADING}>
          [WORKSPACE]
        </h2>
        <dl className="gap-xs grid grid-cols-[auto_1fr] text-sm">
          <dt className="text-muted-foreground">Repository</dt>
          <dd className="break-all">{workspace.repository ?? 'Not reported'}</dd>
          <dt className="text-muted-foreground">Branch</dt>
          <dd className="break-all">{workspace.branch ?? 'Not reported'}</dd>
          <dt className="text-muted-foreground">Environment</dt>
          <dd className="break-all">{workspace.environmentVersionId ?? 'Not reported'}</dd>
        </dl>
        {workspace.sessionId ? (
          <div className="gap-sm flex flex-wrap items-center">
            <span className="text-muted-foreground text-sm">Session</span>
            <ShortId id={workspace.sessionId} label="session" />
            <Button asChild variant="outline" size="sm">
              <Link to="/sessions/$id" params={{ id: workspace.sessionId }}>
                Open the current session
              </Link>
            </Button>
          </div>
        ) : null}
      </section>

      <section aria-labelledby="publication-heading" className={PANEL}>
        <h2 id="publication-heading" className={HEADING}>
          [PR · CI · MERGE]
        </h2>
        <div data-testid="task-publication" className="gap-xs flex flex-col text-sm">
          <p>Publication: {facts.publication.label}.</p>
          {notPublishedYet(view.projection) ? (
            <p>
              <StatusBadge label="Not published yet" tone="muted" />
            </p>
          ) : null}
          <dl className="gap-xs grid grid-cols-[auto_1fr] items-center text-sm">
            <dt className="text-muted-foreground">PR</dt>
            <dd className="gap-sm flex flex-wrap items-center">
              <FactBadge fact={facts.pr} />
              {facts.prUrl ? (
                <a
                  href={facts.prUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-foreground inline-flex min-h-11 items-center underline"
                >
                  Open PR
                </a>
              ) : null}
            </dd>
            <dt className="text-muted-foreground">CI</dt>
            <dd>
              <FactBadge fact={facts.ci} />
            </dd>
            <dt className="text-muted-foreground">Merge</dt>
            <dd>
              <FactBadge fact={facts.merge} />
            </dd>
          </dl>
        </div>
        <p data-testid="task-observed" className="text-muted-foreground text-xs">
          {facts.observation.text}
          {facts.observation.stale ? ' (stale)' : ''}. Each is observed separately; none implies the
          others.
        </p>
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
                Showing the first 16 KiB of {view.projection.pr_description.byte_length} bytes. Open
                PR to read the full description.
              </p>
            ) : null}
          </div>
        ) : null}
      </section>

      <section aria-labelledby="operations-heading" data-testid="task-operations" className={PANEL}>
        {/* A finished task with nothing open is not working, so its heading does not say so. */}
        <h2 id="operations-heading" className={HEADING}>
          {operations.length === 0 && isTerminal(view.task.status)
            ? '[OPERATIONS]'
            : '[IN PROGRESS]'}
        </h2>
        {operations.length === 0 ? (
          <p className="text-muted-foreground text-sm">No operation in progress.</p>
        ) : (
          <ul className="gap-md flex flex-col">
            {operations.map((progress) => (
              <OperationItem key={progress.operation.id} progress={progress} />
            ))}
          </ul>
        )}
      </section>
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
                  ? 'text-term-cyan'
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

/** The task's history: attempts, the provider's own summary, reports and child tasks. */
export function TaskHistory({ view, childTasks }: { view: TaskView; childTasks: TaskView[] }) {
  const providers = useProviders();
  const attempts = attemptHistory(view);
  const summary = unverifiedSummary(view);

  return (
    <>
      <section aria-labelledby="attempts-heading" data-testid="task-attempts" className={PANEL}>
        <h2 id="attempts-heading" className={HEADING}>
          [ATTEMPTS]
        </h2>
        {attempts.length === 0 ? (
          <p className="text-muted-foreground text-sm">No attempt yet.</p>
        ) : (
          <ul className="gap-md flex flex-col">
            {attempts.map(({ attempt, current, superseded, predecessorNumber }) => (
              <li key={attempt.id} className="gap-xs flex flex-col text-sm">
                <p className="font-medium">
                  Attempt {attempt.number} · {attempt.native_provider}
                  {attemptSettings(attempt, providers.data ?? [])}
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
                      className="text-foreground inline-flex min-h-11 items-center text-xs underline"
                    >
                      session
                    </Link>
                    <ShortId id={attempt.session_id} label="attempt session" />
                  </div>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      {summary ? (
        <section
          aria-labelledby="summary-heading"
          data-testid="task-unverified-summary"
          className={PANEL}
        >
          <h2 id="summary-heading" className={HEADING}>
            [{summary.label}]
          </h2>
          <p className="text-sm break-words whitespace-pre-wrap">{summary.text}</p>
          <p className="text-muted-foreground text-xs">
            The provider&apos;s own account of the work. Mainloop has not verified it.
          </p>
        </section>
      ) : null}

      <section aria-labelledby="reports-heading" className={PANEL}>
        <h2 id="reports-heading" className={HEADING}>
          [REPORTS]
        </h2>
        {view.reports.length === 0 ? (
          <p className="text-muted-foreground text-sm">No reports yet.</p>
        ) : (
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
        )}
      </section>

      <section aria-labelledby="children-heading" className={PANEL}>
        <h2 id="children-heading" className={HEADING}>
          [CHILD TASKS]
        </h2>
        {childTasks.length === 0 ? (
          <p className="text-muted-foreground text-sm">No child tasks.</p>
        ) : (
          <ul className="gap-xs flex flex-col">
            {childTasks.map((child) => (
              <li key={child.task.id}>
                <Link
                  to="/tasks/$id"
                  params={{ id: child.task.id }}
                  className="gap-sm flex min-h-11 items-center text-sm"
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
        )}
      </section>
    </>
  );
}
