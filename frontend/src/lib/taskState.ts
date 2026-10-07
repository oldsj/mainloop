/**
 * What the owner sees about a durable task, derived from the owner task read contract
 * (docs/specs/tasks.md). Pure: nothing here calls the backend or decides what is allowed.
 *
 * Every observation (PR, CI, merge, activity) is a read projection. Missing or stale values are
 * shown as unknown; a report or a finished turn never reads as completion.
 */
import type {
  Task,
  TaskArtifact,
  TaskAttempt,
  TaskOperation,
  TaskOperationState,
  TaskProjection,
  TaskReason,
  TaskReportRecord,
  TaskStatus,
  TaskView
} from './api';

/** An observation older than this is flagged stale: the backend may have stopped refreshing it. */
export const STALE_OBSERVATION_MS = 10 * 60 * 1000;
/** Display bound for the provider's own summary (the backend caps it at 8 KiB). */
export const SUMMARY_DISPLAY_LIMIT = 8192;

export const UNVERIFIED_SUMMARY_LABEL = 'Provider summary; unverified';

const STATUS_LABELS: Record<TaskStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  waiting: 'Waiting',
  blocked: 'Blocked',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled'
};

const REASON_LABELS: Record<TaskReason, string> = {
  awaiting_child: 'awaiting a child task',
  approval: 'awaiting owner approval',
  ci: 'awaiting CI',
  publication: 'awaiting publication',
  handoff: 'provider handoff in progress',
  reconciliation: 'reconciling uncertain state',
  provisioning_unavailable: 'workspace provisioning is not available yet',
  handoff_unavailable: 'handoff is not available yet',
  cancel_unavailable: 'cancellation is not available yet'
};

export type Tone = 'active' | 'attention' | 'ok' | 'bad' | 'muted';

const STATUS_TONES: Record<TaskStatus, Tone> = {
  queued: 'muted',
  running: 'active',
  waiting: 'attention',
  blocked: 'attention',
  completed: 'ok',
  failed: 'bad',
  cancelled: 'muted'
};

export function statusLabel(status: string): string {
  return STATUS_LABELS[status as TaskStatus] ?? `Unknown (${status})`;
}

export function reasonLabel(reason: string | null | undefined): string | null {
  if (!reason) return null;
  return REASON_LABELS[reason as TaskReason] ?? reason.replaceAll('_', ' ');
}

export function statusTone(status: string): Tone {
  return STATUS_TONES[status as TaskStatus] ?? 'muted';
}

export function isTerminal(status: string): boolean {
  return status === 'completed' || status === 'failed' || status === 'cancelled';
}

/** Fill the collections an older or partial payload omits, so views never read `undefined`. */
export function normalizeView(raw: Partial<TaskView> & { task: Task }): TaskView {
  return {
    task: raw.task,
    attempts: raw.attempts ?? [],
    operations: raw.operations ?? [],
    artifacts: raw.artifacts ?? [],
    reports: raw.reports ?? [],
    projection: raw.projection ?? {},
    actions: raw.actions ?? {}
  };
}

export function currentAttempt(view: TaskView): TaskAttempt | null {
  const id = view.task.current_attempt_id;
  return id ? (view.attempts.find((attempt) => attempt.id === id) ?? null) : null;
}

export interface AttemptRow {
  attempt: TaskAttempt;
  current: boolean;
  /** Replaced by a successor: its history is read-only and it can no longer write. */
  superseded: boolean;
  /** Number of the attempt this one continued from, when recorded. */
  predecessorNumber: number | null;
}

/** Newest first; superseded attempts stay listed as history. */
export function attemptHistory(view: TaskView): AttemptRow[] {
  const byId = new Map(view.attempts.map((attempt) => [attempt.id, attempt]));
  return [...view.attempts]
    .sort((a, b) => b.number - a.number)
    .map((attempt) => ({
      attempt,
      current: attempt.id === view.task.current_attempt_id,
      superseded: attempt.state === 'superseded' || attempt.superseded_at != null,
      predecessorNumber: attempt.predecessor_id
        ? (byId.get(attempt.predecessor_id)?.number ?? null)
        : null
    }));
}

export interface TreeRow {
  view: TaskView;
  depth: number;
  /** The parent exists but was not loaded (or is out of scope); the row is shown at top level. */
  detached: boolean;
}

/**
 * Parents with their children beneath them. A task whose parent is not among `views` is a
 * top-level row, so a filtered list never hides work. Cycles cannot loop: each task appears once.
 */
export function buildTree(views: TaskView[]): TreeRow[] {
  const byId = new Map(views.map((view) => [view.task.id, view]));
  const children = new Map<string, TaskView[]>();
  const roots: TaskView[] = [];
  for (const view of views) {
    const parent = view.task.parent_task_id;
    if (parent && byId.has(parent) && parent !== view.task.id) {
      const siblings = children.get(parent) ?? [];
      siblings.push(view);
      children.set(parent, siblings);
    } else {
      roots.push(view);
    }
  }
  const byCreated = (a: TaskView, b: TaskView) =>
    a.task.created_at.localeCompare(b.task.created_at) || a.task.id.localeCompare(b.task.id);
  const rows: TreeRow[] = [];
  const seen = new Set<string>();
  const visit = (view: TaskView, depth: number) => {
    if (seen.has(view.task.id)) return;
    seen.add(view.task.id);
    rows.push({ view, depth, detached: depth === 0 && view.task.parent_task_id != null });
    for (const child of (children.get(view.task.id) ?? []).sort(byCreated)) visit(child, depth + 1);
  };
  // Newest roots first, so current work leads the list.
  for (const root of roots.sort((a, b) => byCreated(b, a))) visit(root, 0);
  // Members of a parent cycle have no root; list them rather than drop them.
  for (const view of [...views].sort(byCreated)) visit(view, 0);
  return rows;
}

/** Where the task is directly owned: ancestors up to the root, nearest parent last. */
export function ancestors(views: TaskView[], id: string): TaskView[] {
  const byId = new Map(views.map((view) => [view.task.id, view]));
  const chain: TaskView[] = [];
  const seen = new Set([id]);
  let next = byId.get(id)?.task.parent_task_id;
  while (next && !seen.has(next)) {
    seen.add(next);
    const parent = byId.get(next);
    if (!parent) break;
    chain.unshift(parent);
    next = parent.task.parent_task_id;
  }
  return chain;
}

export function childrenOf(views: TaskView[], id: string): TaskView[] {
  return views
    .filter((view) => view.task.parent_task_id === id)
    .sort((a, b) => a.task.created_at.localeCompare(b.task.created_at));
}

/** The task a session belongs to, whether through its current or a superseded attempt. */
export function taskForSession(
  views: TaskView[],
  sessionId: string
): { view: TaskView; attempt: TaskAttempt; current: boolean } | null {
  return findAttempt(views, (attempt) => attempt.session_id === sessionId);
}

export function taskForWorkspace(
  views: TaskView[],
  workspaceId: string
): { view: TaskView; attempt: TaskAttempt; current: boolean } | null {
  return findAttempt(views, (attempt) => attempt.workspace_id === workspaceId);
}

function findAttempt(views: TaskView[], match: (attempt: TaskAttempt) => boolean) {
  for (const view of views) {
    const attempt = view.attempts.find(match);
    if (attempt) return { view, attempt, current: attempt.id === view.task.current_attempt_id };
  }
  return null;
}

/** The task whose projection lists this HITL request as awaiting the owner. */
export function taskForApproval(views: TaskView[], requestId: string): TaskView | null {
  return (
    views.find((view) => (view.projection.pending_approval_ids ?? []).includes(requestId)) ?? null
  );
}

export interface Fact {
  label: string;
  /** False when the backend has not reported this; shown as unknown, never as "none". */
  known: boolean;
  tone: Tone;
}

export interface Observation {
  text: string;
  /** No observation yet, or one older than STALE_OBSERVATION_MS. */
  stale: boolean;
  known: boolean;
}

export function observationAge(
  observedAt: string | null | undefined,
  now: number = Date.now()
): Observation {
  const at = observedAt ? Date.parse(observedAt) : NaN;
  if (!Number.isFinite(at) || at > now)
    return { text: 'not observed yet', stale: true, known: false };
  const age = Math.max(0, now - at);
  return {
    text: `observed ${relativeAge(age)} ago`,
    stale: age > STALE_OBSERVATION_MS,
    known: true
  };
}

export function relativeAge(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export interface Publication {
  pr: Fact;
  ci: Fact;
  merge: Fact;
  publication: Fact;
  /** Only an https link; anything else is not rendered as a link. */
  prUrl: string | null;
  prHeadSha: string | null;
  /** CI was observed for a different commit than the PR head, so it says nothing about the head. */
  ciHeadMismatch: boolean;
  observation: Observation;
}

const unknown = (label = 'Unknown'): Fact => ({ label, known: false, tone: 'muted' });

/** PR, CI and merge are reported separately; none implies another. */
export function publicationFacts(
  projection: TaskProjection | undefined,
  now: number = Date.now()
): Publication {
  const p = projection ?? {};
  const prHeadSha = p.pr_head_sha ?? null;
  const ciHeadMismatch = Boolean(p.ci_head_sha && prHeadSha && p.ci_head_sha !== prHeadSha);

  let pr: Fact = unknown();
  const prNumber = p.pr_number != null ? `#${p.pr_number} ` : '';
  if (p.pr_state === 'open') pr = { label: `${prNumber}open`, known: true, tone: 'active' };
  else if (p.pr_state === 'merged') pr = { label: `${prNumber}merged`, known: true, tone: 'ok' };
  else if (p.pr_state === 'closed')
    pr = { label: `${prNumber}closed, not merged`, known: true, tone: 'muted' };

  // Every current CI state requires fresh evidence for the exact PR head.
  let ci: Fact = unknown();
  if (ciHeadMismatch)
    ci = { label: 'Unknown (checked a different commit)', known: false, tone: 'attention' };
  else if (prHeadSha && p.ci_head_sha === prHeadSha && !observationAge(p.observed_at, now).stale) {
    if (p.ci_state === 'success') ci = { label: 'Passing', known: true, tone: 'ok' };
    else if (p.ci_state === 'failure') ci = { label: 'Failing', known: true, tone: 'bad' };
    else if (p.ci_state === 'pending') ci = { label: 'Running', known: true, tone: 'active' };
  }

  const merge: Fact = p.merge_state
    ? {
        label: humanize(p.merge_state),
        known: true,
        tone: p.merge_state === 'merged' ? 'ok' : 'attention'
      }
    : { label: 'Not reported', known: false, tone: 'muted' };
  const publication: Fact = p.publication_state
    ? { label: humanize(p.publication_state), known: true, tone: 'muted' }
    : { label: 'Not reported', known: false, tone: 'muted' };

  return {
    pr,
    ci,
    merge,
    publication,
    prUrl: safeExternalUrl(p.pr_url),
    prHeadSha,
    ciHeadMismatch,
    observation: observationAge(p.observed_at, now)
  };
}

function humanize(value: string): string {
  const text = value.replaceAll('_', ' ');
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/** https links only: a projection must not be able to inject `javascript:` or other schemes. */
export function safeExternalUrl(value: string | null | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.toString() : null;
  } catch {
    return null;
  }
}

export interface Workspace {
  repository: string | null;
  branch: string | null;
  environmentVersionId: string | null;
  workspaceId: string | null;
  sessionId: string | null;
}

/** Where the work is checked out. Observed values win; the task's requested branch is the fallback. */
export function workspaceFacts(view: TaskView): Workspace {
  const attempt = currentAttempt(view);
  return {
    repository: view.projection.repository ?? null,
    branch: view.projection.branch ?? view.task.checkout?.branch ?? null,
    environmentVersionId: view.projection.environment_version_id ?? null,
    workspaceId: attempt?.workspace_id ?? null,
    sessionId: attempt?.session_id ?? null
  };
}

/** The provider's own account of the work. It is a claim, never evidence. */
export function unverifiedSummary(view: TaskView): { label: string; text: string } | null {
  const candidates = view.artifacts.filter(
    (artifact: TaskArtifact) => artifact.kind === 'unverified_provider_summary'
  );
  const latest = candidates.at(-1);
  if (!latest) return null;
  const payload = latest.payload ?? {};
  const raw = [payload.summary, payload.text, payload.note].find((v) => typeof v === 'string');
  if (typeof raw !== 'string' || !raw.trim()) return null;
  return {
    label: UNVERIFIED_SUMMARY_LABEL,
    text: raw.length > SUMMARY_DISPLAY_LIMIT ? `${raw.slice(0, SUMMARY_DISPLAY_LIMIT)}…` : raw
  };
}

const REPORT_OUTCOMES: Record<TaskReportRecord['outcome'], string> = {
  progress: 'Progress report',
  completed: 'Reports completed (unverified claim)',
  failed: 'Reports failed (unverified claim)',
  blocked: 'Reports blocked'
};

export function reportLabel(report: TaskReportRecord): string {
  return REPORT_OUTCOMES[report.outcome] ?? report.outcome;
}

// Operation display ------------------------------------------------------------------------

export const OPERATION_STEPS: TaskOperationState[] = [
  'requested',
  'draining',
  'checkpoint_required',
  'checkpoint_verified',
  'source_fencing',
  'source_fenced',
  'target_creating',
  'target_ready',
  'completed'
];

const STEP_LABELS: Record<TaskOperationState, string> = {
  requested: 'Requested',
  draining: 'Draining the current attempt',
  checkpoint_required: 'Checkpoint required',
  checkpoint_verified: 'Checkpoint verified',
  source_fencing: 'Fencing the old attempt',
  source_fenced: 'Old attempt fenced',
  target_creating: 'Creating the new attempt',
  target_ready: 'New attempt ready',
  completed: 'Completed',
  blocked: 'Blocked',
  uncertain: 'Uncertain'
};

export function stepLabel(state: string): string {
  return STEP_LABELS[state as TaskOperationState] ?? state.replaceAll('_', ' ');
}

export interface OperationProgress {
  operation: TaskOperation;
  kindLabel: string;
  steps: { state: TaskOperationState; label: string; status: 'done' | 'current' | 'pending' }[];
  /** True for blocked and uncertain operations: they hold at the last confirmed step. */
  held: boolean;
  summary: string;
  open: boolean;
}

const KIND_LABELS: Record<TaskOperation['kind'], string> = {
  create: 'Create',
  retry: 'Retry',
  reassign: 'Reassign',
  cancel: 'Cancel'
};

export function operationProgress(operation: TaskOperation): OperationProgress {
  const held = operation.state === 'blocked' || operation.state === 'uncertain';
  const position = OPERATION_STEPS.indexOf(held ? operation.last_confirmed_step : operation.state);
  const steps = OPERATION_STEPS.map((state, index) => ({
    state,
    label: stepLabel(state),
    status:
      operation.state === 'completed' || index < position
        ? ('done' as const)
        : index === position
          ? held
            ? ('done' as const)
            : ('current' as const)
          : ('pending' as const)
  }));
  const reason = reasonLabel(operation.reason);
  const kindLabel = KIND_LABELS[operation.kind] ?? operation.kind;
  const summary = held
    ? `${kindLabel} ${operation.state}: last confirmed step “${stepLabel(operation.last_confirmed_step)}”${reason ? `; ${reason}` : ''}`
    : `${kindLabel}: ${stepLabel(operation.state)}`;
  return {
    operation,
    kindLabel,
    steps,
    held,
    summary,
    open: operation.state !== 'completed'
  };
}

/** Operations still moving or held, newest first; completed ones are history. */
export function openOperations(view: TaskView): OperationProgress[] {
  return view.operations
    .map(operationProgress)
    .filter((progress) => progress.open)
    .sort((a, b) => b.operation.created_at.localeCompare(a.operation.created_at));
}

/** Things the owner must resolve before a provider switch can proceed. Informational only. */
export function switchBlockers(view: TaskView): string[] {
  const blockers: string[] = [];
  const approvals = view.projection.pending_approval_ids ?? [];
  if (approvals.length)
    blockers.push(
      `${approvals.length} pending approval${approvals.length === 1 ? '' : 's'}: answer or abandon in the Inbox before switching providers`
    );
  for (const progress of openOperations(view)) {
    const { operation } = progress;
    if (progress.held && operation.last_confirmed_step === 'checkpoint_required')
      blockers.push('Checkpoint required: the current attempt must commit and push its work');
    else if (operation.state === 'checkpoint_required')
      blockers.push('Waiting for the current attempt to commit and push a checkpoint');
    else blockers.push(progress.summary);
  }
  return blockers;
}

const TONE_CLASSES: Record<Tone, string> = {
  active: 'text-term-cyan border-term-cyan/50',
  attention: 'text-term-yellow border-term-yellow/50',
  ok: 'text-term-green border-term-green/50',
  bad: 'text-term-red border-term-red/50',
  muted: 'text-term-fg-muted border-term-border'
};

/** Text and border colour classes for a badge. Colour is never the only signal: badges carry text. */
export function toneClass(tone: Tone): string {
  return TONE_CLASSES[tone];
}

/** Provider shown for a task: the running attempt's, else the profile it is assigned to. */
export function providerLabel(view: TaskView): string {
  const attempt = currentAttempt(view);
  return attempt ? attempt.native_provider : view.task.assigned_profile_id;
}
