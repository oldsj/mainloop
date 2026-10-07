/**
 * Sanitized task fixtures in the shape of the S0 owner read contract (models/src/models/task.py,
 * docs/specs/tasks.md). IDs and times are invented. Used by the task unit tests only.
 */
import type {
  Task,
  TaskAttempt,
  TaskOperation,
  TaskProjection,
  TaskView,
  ProviderProfile
} from './api';

export const NOW = Date.parse('2026-01-01T12:00:00Z');

export function task(overrides: Partial<Task> = {}): Task {
  return {
    id: 'task-1',
    owner_id: 'owner-1',
    project_id: 'project-1',
    topic_id: null,
    parent_task_id: null,
    root_task_id: overrides.id ?? 'task-1',
    creator_binding_id: null,
    title: 'Add retry button',
    brief: 'Add a retry button to the task page.',
    mode: 'code',
    assigned_profile_id: 'claude-default',
    selection_source: 'explicit',
    provider_constraint: null,
    status: 'running',
    reason: null,
    current_attempt_id: 'attempt-1',
    version: 3,
    checkout: { branch: 'task/retry', ref: 'main', depth: 1 },
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T11:55:00Z',
    ...overrides
  };
}

export function attempt(overrides: Partial<TaskAttempt> = {}): TaskAttempt {
  return {
    id: 'attempt-1',
    task_id: 'task-1',
    number: 1,
    profile_id: 'claude-default',
    native_provider: 'claude',
    configuration_revision: 'rev-1',
    agent_ref: { namespace: 'agents', name: `${overrides.native_provider ?? 'claude'}-child` },
    role: 'child',
    depth: 2,
    writer_generation: 1,
    session_id: 'session-1',
    binding_id: 'binding-1',
    workspace_id: 'workspace-1',
    state: 'active',
    predecessor_id: null,
    successor_id: null,
    checkpoint_ref: null,
    superseded_at: null,
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T11:55:00Z',
    ...overrides
  };
}

export function operation(overrides: Partial<TaskOperation> = {}): TaskOperation {
  return {
    id: 'operation-1',
    owner_id: 'owner-1',
    principal_key: 'owner:owner-1',
    request_digest: 'b'.repeat(64),
    kind: 'reassign',
    request_id: 'ui-request-1',
    task_id: 'task-1',
    attempt_id: 'attempt-1',
    state: 'requested',
    last_confirmed_step: 'requested',
    source_attempt_id: 'attempt-1',
    target_attempt_id: null,
    checkpoint_ref: null,
    reason: null,
    created_at: '2026-01-01T11:56:00Z',
    updated_at: '2026-01-01T11:56:00Z',
    ...overrides
  };
}

export function view(
  taskOverrides: Partial<Task> = {},
  rest: Partial<Omit<TaskView, 'task'>> = {}
): TaskView {
  const t = task(taskOverrides);
  return {
    task: t,
    attempts: [attempt({ task_id: t.id })],
    operations: [],
    artifacts: [],
    reports: [],
    projection: {},
    actions: {
      retry: { available: false, reason: 'handoff_unavailable' },
      reassign: { available: true, reason: null },
      cancel: { available: true, reason: null }
    },
    ...rest
  };
}

/** A Codex-run task, to prove both providers render and act the same way. */
export function codexView(): TaskView {
  const t = task({
    id: 'task-codex',
    assigned_profile_id: 'codex-default',
    current_attempt_id: 'attempt-codex'
  });
  return view(t, {
    attempts: [
      attempt({
        id: 'attempt-codex',
        task_id: t.id,
        profile_id: 'codex-default',
        native_provider: 'codex',
        session_id: 'session-codex',
        workspace_id: 'workspace-codex'
      })
    ]
  });
}

/** Attempt 1 (Claude) was superseded by attempt 2 (Codex) after a reassign. */
export function reassignedView(): TaskView {
  const t = task({
    id: 'task-reassigned',
    assigned_profile_id: 'codex-default',
    current_attempt_id: 'attempt-2',
    version: 9
  });
  return view(t, {
    attempts: [
      attempt({
        id: 'attempt-1',
        task_id: t.id,
        number: 1,
        state: 'superseded',
        successor_id: 'attempt-2',
        superseded_at: '2026-01-01T11:00:00Z',
        checkpoint_ref: 'refs/mainloop/checkpoints/c1',
        session_id: 'session-old',
        workspace_id: 'workspace-old'
      }),
      attempt({
        id: 'attempt-2',
        task_id: t.id,
        number: 2,
        profile_id: 'codex-default',
        native_provider: 'codex',
        predecessor_id: 'attempt-1',
        session_id: 'session-new',
        workspace_id: 'workspace-new'
      })
    ],
    operations: [
      operation({
        id: 'operation-done',
        task_id: t.id,
        state: 'completed',
        last_confirmed_step: 'completed',
        target_attempt_id: 'attempt-2'
      })
    ],
    artifacts: [
      {
        id: 'artifact-1',
        operation_id: 'operation-done',
        kind: 'unverified_provider_summary',
        sha256: 'a'.repeat(64),
        payload: { summary: 'I finished the button and the tests pass.' }
      }
    ]
  });
}

export function projection(overrides: Partial<TaskProjection> = {}): TaskProjection {
  return { observed_at: '2026-01-01T11:59:00Z', ...overrides };
}

export function profile(overrides: Partial<ProviderProfile> = {}): ProviderProfile {
  return {
    id: 'codex-default',
    display_name: 'Codex',
    runtime_adapter: 'kagent',
    native_provider: 'codex',
    configuration_revision: 'rev-1',
    agents: {
      child: { namespace: 'agents', name: 'codex-child' },
      supervisor: { namespace: 'agents', name: 'codex-supervisor' }
    },
    aliases: [],
    enabled: true,
    capabilities: [],
    ...overrides
  };
}

export const CLAUDE_PROFILE = profile({
  id: 'claude-default',
  display_name: 'Claude',
  native_provider: 'claude'
});
export const CODEX_PROFILE = profile();
