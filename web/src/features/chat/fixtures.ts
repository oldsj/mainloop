import type { Session } from '@/lib/contract';
import type {
  ChatMessage,
  MainProviderInfo,
  MainThreadInfo,
  NativeDeliveryInfo,
  NativeSessionInfo
} from './types';

export const MAIN_SESSION_ID = 'sess-main';
export const CONVERSATION_ID = 'conv-1';

export function messageFixture(overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id: 'm1',
    conversation_id: CONVERSATION_ID,
    role: 'user',
    content: 'hello',
    created_at: '2026-01-01T10:00:00Z',
    images: undefined,
    ...overrides
  };
}

export function deliveryFixture(overrides: Partial<NativeDeliveryInfo> = {}): NativeDeliveryInfo {
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

export function nativeFixture(overrides: Partial<NativeSessionInfo> = {}): NativeSessionInfo {
  return {
    session_id: MAIN_SESSION_ID,
    kind: 'claude',
    role: 'main',
    parent_session_id: null,
    topic: null,
    agent_name: 'main-agent',
    kagent_session_id: 'kagent-1',
    session_state: 'active',
    model: 'claude-sonnet-5-5',
    model_override: null,
    effort_override: null,
    turns: 1,
    turn_in_flight: false,
    restartable: false,
    deliveries: [],
    note: null,
    ...overrides
  };
}

/** A main provider that is ready and confirmed: the owner chose Claude and Claude is running. */
export function providerFixture(overrides: Partial<MainProviderInfo> = {}): MainProviderInfo {
  return {
    session_id: MAIN_SESSION_ID,
    desired_provider: 'claude',
    effective_provider: 'claude',
    status: 'ready',
    kagent_session_id: 'kagent-1',
    error: null,
    recoverable: false,
    history: [],
    ...overrides
  };
}

/** An empty main thread: no conversation yet, nothing in flight, the provider ready. */
export function mainThreadFixture(overrides: Partial<MainThreadInfo> = {}): MainThreadInfo {
  return {
    mode: 'native',
    session_id: MAIN_SESSION_ID,
    conversation_id: null,
    native: nativeFixture(),
    provider: providerFixture(),
    topics: [],
    ...overrides
  };
}

/** A session row for a conversation read. The chat reads only its id; the rest is filler. */
export function sessionFixture(id: string): Session {
  return {
    id,
    user_id: 'dev-user',
    title: `Session ${id}`,
    description: '',
    prompt: 'Session prompt.',
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
    result: null
  };
}
