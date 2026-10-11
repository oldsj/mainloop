import type { ChatMessage, MainThreadInfo, NativeDeliveryInfo, NativeSessionInfo } from './types';

export const MAIN_SESSION_ID = 'sess-main';
export const CONVERSATION_ID = 'conv-1';

export function messageFixture(overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id: 'm1',
    conversation_id: CONVERSATION_ID,
    role: 'user',
    content: 'hello',
    created_at: '2026-01-01T10:00:00Z',
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
    turns: 1,
    turn_in_flight: false,
    deliveries: [],
    note: null,
    ...overrides
  };
}

/** An empty main thread: no conversation yet, nothing in flight. */
export function mainThreadFixture(overrides: Partial<MainThreadInfo> = {}): MainThreadInfo {
  return {
    mode: 'native',
    session_id: MAIN_SESSION_ID,
    conversation_id: null,
    native: nativeFixture(),
    topics: [],
    ...overrides
  };
}
