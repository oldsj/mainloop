import type { components } from '@/api/schema.gen';
import type { Session } from '@/lib/contract';

type Schemas = components['schemas'];

export type SessionFixture = Session;
export type NativeFixture = Schemas['NativeSessionInfo'];
export type DeliveryFixture = Schemas['NativeDeliveryInfo'];

/** A session in the shape the app reads. IDs and times are invented. */
export function session(overrides: Partial<SessionFixture> = {}): SessionFixture {
  return {
    id: 's1',
    user_id: 'dev-user',
    title: 'Fix login redirect',
    description: 'Investigate the redirect loop after sign-in.',
    prompt: 'Find the cause of the redirect loop.',
    parent_session_id: null,
    topic: null,
    agent_kind: 'claude',
    status: 'active',
    needs_owner: false,
    created_at: '2026-01-01T11:00:00Z',
    started_at: '2026-01-01T11:00:01Z',
    completed_at: null,
    archived_at: null,
    summary: null,
    error: null,
    repo_url: null,
    project_id: null,
    branch_name: null,
    base_branch: 'main',
    model: null,
    commit_sha: null,
    color: null,
    result: null,
    ...overrides
  };
}

/** The identity strip's native read for a session. */
export function native(overrides: Partial<NativeFixture> = {}): NativeFixture {
  return {
    session_id: 's1',
    kind: 'claude',
    role: 'agent',
    parent_session_id: null,
    topic: null,
    agent_name: 'claude-child',
    kagent_session_id: 'kagent-session-123',
    session_state: 'ready',
    model: 'claude-sonnet-5-5',
    model_override: null,
    effort_override: null,
    turns: 2,
    turn_in_flight: false,
    restartable: false,
    deliveries: [],
    note: null,
    ...overrides
  };
}

export function delivery(overrides: Partial<DeliveryFixture> = {}): DeliveryFixture {
  return {
    message_id: 'm1',
    state: 'delivered',
    task_id: null,
    evidence_ref: null,
    detail: null,
    source: 'user',
    ...overrides
  };
}

/** The attention read with the given sessions and nothing else. */
export function attentionSnapshot(sessions: SessionFixture[]): Schemas['AttentionSnapshot'] {
  return { items: [], count: 0, sessions };
}
