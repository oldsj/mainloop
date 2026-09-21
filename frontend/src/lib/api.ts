/**
 * API client for backend communication
 */

import { API_URL } from '$lib/config';
import { connection } from '$lib/stores/connection';

/** A send the backend did not accept. `status` is 0 when it never got an HTTP response. */
export class SendError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}

/** The backend's explanation for a refused request, or a fallback when it gave none. */
async function errorDetail(response: Response, fallback: string): Promise<string> {
  try {
    const body = await response.json();
    if (typeof body?.detail === 'string') return body.detail;
  } catch {
    // not JSON (e.g. a proxy's error page)
  }
  return fallback;
}

/** fetch that tells the connection store when the backend can't be reached at all. */
async function apiFetch(input: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(input, init);
  } catch (error) {
    // No HTTP response (refused, DNS, offline). Aborts are the caller's own doing.
    if (!(error instanceof DOMException && error.name === 'AbortError')) {
      connection.reportFailure();
    }
    throw error;
  }
}

export interface Message {
  id: string;
  conversation_id: string;
  role: 'user' | 'assistant';
  content: string;
  created_at: string;
}

export interface Conversation {
  id: string;
  user_id: string;
  title: string | null;
  created_at: string;
  updated_at: string;
}

export interface ChatRequest {
  message: string;
  conversation_id?: string;
}

export interface ChatResponse {
  conversation_id: string;
  message: Message | null; // null when session spawned
  spawned_session_id?: string; // Session ID if one was spawned
  pending?: boolean; // native main thread: the reply is mirrored from the journal; poll the conversation
  delivery_message_id?: string | null;
}

export type QueueItemType =
  | 'question'
  | 'notification'
  | 'error'
  | 'review'
  | 'plan_ready'
  | 'plan_review'
  | 'code_ready'
  | 'feedback_addressed'
  | 'routing_suggestion';

export type QueueItemPriority = 'low' | 'normal' | 'high' | 'urgent';

export interface QueueItem {
  id: string;
  main_thread_id: string;
  task_id: string | null;
  user_id: string;
  item_type: QueueItemType;
  priority: QueueItemPriority;
  title: string;
  content: string;
  context: Record<string, unknown>;
  options: string[] | null;
  status: string;
  response: string | null;
  responded_at: string | null;
  read_at: string | null;
  created_at: string;
  expires_at: string | null;
}

export interface Project {
  id: string;
  user_id: string;
  owner: string;
  name: string;
  full_name: string;
  description: string | null;
  default_branch: string;
  avatar_url: string | null;
  html_url: string;
  created_at: string;
  last_used_at: string;
  metadata_updated_at: string | null;
  open_pr_count: number;
  open_issue_count: number;
}

export interface ProjectPRSummary {
  number: number;
  title: string;
  state: string;
  author: string;
  created_at: string;
  updated_at: string;
  url: string;
  is_mainloop: boolean;
}

export interface CommitSummary {
  sha: string;
  message: string;
  author: string;
  date: string;
  url: string;
}

export interface ProjectDetail {
  project: Project;
  open_prs: ProjectPRSummary[];
  recent_commits: CommitSummary[];
  sessions: Session[];
}

// Session types
export type SessionStatus =
  | 'pending'
  | 'active'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'waiting_on_user'
  | 'implementing'
  | 'under_review';

export interface Session {
  id: string;
  user_id: string;
  main_thread_id: string;
  title: string;
  description: string;
  prompt: string;
  conversation_id: string;
  parent_session_id?: string | null; // native child: the delegating session
  topic?: string | null;
  status: SessionStatus;
  worker_pod_name: string | null;
  created_at: string;
  started_at: string | null;
  completed_at: string | null;
  /** Set when the session was cleared from the list; the row is kept for audit. */
  archived_at?: string | null;
  summary: string | null;
  error: string | null;
  // Code work fields (optional)
  repo_url: string | null;
  project_id: string | null;
  branch_name: string | null;
  base_branch: string;
  model: string | null;
  // GitHub issue fields
  issue_url: string | null;
  issue_number: number | null;
  // GitHub PR fields
  pr_url: string | null;
  pr_number: number | null;
  commit_sha: string | null;
  // Inline thread anchoring
  anchor_message_id: string | null;
  color: string | null;
  result: Record<string, unknown> | null;
}

export interface NativeDelivery {
  message_id: string;
  state: string;
  evidence_ref: string | null;
  detail: string | null;
}

export interface TopicLine {
  name: string;
  status_line: string;
  pending: number;
}

export interface MainThreadInfo {
  mode: 'sdk' | 'native';
  session_id: string | null;
  conversation_id: string | null;
  native: NativeSessionInfo | null;
  topics: TopicLine[];
}

export interface TopicRecord {
  id: string;
  kind: 'note' | 'decision' | 'pending' | 'report';
  text: string;
  status: string;
  session_id: string | null;
  created_at: string;
}

export interface TopicWithRecords {
  id: string;
  name: string;
  status_line: string;
  records: TopicRecord[];
}

export interface NativeSessionInfo {
  session_id: string;
  kind: 'claude' | 'codex';
  role?: 'agent' | 'main' | 'child';
  parent_session_id?: string | null;
  topic?: string | null;
  lineage_seq?: number;
  context_tokens?: number | null;
  baseline_tokens?: number | null;
  turns_in_lineage?: number;
  continuations?: number;
  rotating?: boolean;
  agent_name: string;
  native_session_id: string | null;
  model: string | null;
  approval_policy: string;
  herdr_pane_id: string | null;
  herdr_terminal_id: string | null;
  herdr_workspace_id: string | null;
  workspace_pod: string | null;
  workspace_pod_uid: string | null;
  workspace_ready: boolean;
  agent_live: boolean | null;
  generation: number;
  journal_cursor: number;
  journal_ref: string | null;
  turn_in_flight: boolean;
  deliveries: NativeDelivery[];
  note: string | null;
}

export interface SessionCreate {
  title: string;
  description: string;
  prompt: string;
  repo_url?: string;
  anchor_message_id?: string;
  agent_kind?: 'claude' | 'codex';
}

export interface SessionNotification {
  id: string;
  session_id: string;
  user_id: string;
  title: string;
  preview: string;
  read: boolean;
  created_at: string;
}

export const api = {
  async listConversations(): Promise<{ conversations: Conversation[]; total: number }> {
    const response = await apiFetch(`${API_URL}/conversations`);
    if (!response.ok) throw new Error('Failed to list conversations');
    return response.json();
  },

  async getConversation(
    conversationId: string
  ): Promise<{ conversation: Conversation; messages: Message[] }> {
    const response = await apiFetch(`${API_URL}/conversations/${conversationId}`);
    if (!response.ok) throw new Error('Failed to get conversation');
    return response.json();
  },

  async sendMessage(request: ChatRequest): Promise<ChatResponse> {
    let response: Response;
    try {
      response = await apiFetch(`${API_URL}/chat`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json'
        },
        body: JSON.stringify(request)
      });
    } catch {
      throw new SendError("Can't reach the Mainloop backend.", 0);
    }
    if (!response.ok) {
      // The native main thread answers 409 with a reason (rotating, or a turn still in flight).
      let detail = 'Failed to send message';
      try {
        const body = await response.json();
        if (typeof body?.detail === 'string') detail = body.detail;
      } catch {
        // keep the generic message
      }
      throw new SendError(detail, response.status);
    }
    return response.json();
  },

  // Inbox/Queue endpoints
  async getUnreadCount(): Promise<number> {
    const response = await apiFetch(`${API_URL}/queue/unread/count`);
    if (!response.ok) throw new Error('Failed to get unread count');
    const data = await response.json();
    return data.count;
  },

  async listQueueItems(options?: {
    status?: string;
    unreadOnly?: boolean;
    taskId?: string;
  }): Promise<QueueItem[]> {
    const params = new URLSearchParams();
    if (options?.status) params.set('status', options.status);
    if (options?.unreadOnly) params.set('unread_only', 'true');
    if (options?.taskId) params.set('task_id', options.taskId);

    const url = params.toString() ? `${API_URL}/queue?${params}` : `${API_URL}/queue`;
    const response = await apiFetch(url);
    if (!response.ok) throw new Error('Failed to list queue items');
    return response.json();
  },

  async getQueueItem(itemId: string): Promise<QueueItem> {
    const response = await apiFetch(`${API_URL}/queue/${itemId}`);
    if (!response.ok) throw new Error('Failed to get queue item');
    return response.json();
  },

  async markQueueItemRead(itemId: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/queue/${itemId}/read`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to mark queue item read');
  },

  async markAllQueueItemsRead(): Promise<void> {
    const response = await apiFetch(`${API_URL}/queue/read-all`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to mark all read');
  },

  async respondToQueueItem(itemId: string, responseText: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/queue/${itemId}/respond`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ response: responseText })
    });
    if (!response.ok) throw new Error('Failed to respond to queue item');
  },

  // Project endpoints
  async listProjects(limit?: number): Promise<Project[]> {
    const params = limit ? `?limit=${limit}` : '';
    const response = await apiFetch(`${API_URL}/projects${params}`);
    if (!response.ok) throw new Error('Failed to list projects');
    return response.json();
  },

  async getProject(projectId: string): Promise<Project> {
    const response = await apiFetch(`${API_URL}/projects/${projectId}`);
    if (!response.ok) throw new Error('Failed to get project');
    return response.json();
  },

  async getProjectDetail(projectId: string): Promise<ProjectDetail> {
    const response = await apiFetch(`${API_URL}/projects/${projectId}/detail`);
    if (!response.ok) throw new Error('Failed to get project detail');
    return response.json();
  },

  async refreshProject(projectId: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/projects/${projectId}/refresh`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to refresh project');
  },

  /**
   * Get the SSE endpoint URL for the global event stream.
   */
  getEventsStreamUrl(): string {
    return `${API_URL}/events`;
  },

  // Session endpoints
  async listSessions(options?: { status?: string }): Promise<Session[]> {
    const params = new URLSearchParams();
    if (options?.status) params.set('status', options.status);
    const url = params.toString() ? `${API_URL}/sessions?${params}` : `${API_URL}/sessions`;
    const response = await apiFetch(url);
    if (!response.ok) throw new Error('Failed to list sessions');
    return response.json();
  },

  async createSession(request: SessionCreate): Promise<Session> {
    const response = await apiFetch(`${API_URL}/sessions`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(request)
    });
    if (!response.ok) throw new Error('Failed to create session');
    return response.json();
  },

  async getMainThread(): Promise<MainThreadInfo> {
    const response = await apiFetch(`${API_URL}/main-thread`);
    if (!response.ok) throw new Error('Failed to get main thread');
    return response.json();
  },

  async rotateMainThread(): Promise<Record<string, unknown>> {
    const response = await apiFetch(`${API_URL}/main-thread/rotate`, { method: 'POST' });
    if (!response.ok) throw new Error('Failed to rotate main thread');
    return response.json();
  },

  async listTopics(): Promise<TopicWithRecords[]> {
    const response = await apiFetch(`${API_URL}/topics`);
    if (!response.ok) throw new Error('Failed to list topics');
    return response.json();
  },

  async getSessionNative(sessionId: string): Promise<NativeSessionInfo | null> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/native`);
    if (response.status === 404) return null;
    if (!response.ok) throw new Error('Failed to get native session info');
    return response.json();
  },

  async getSession(sessionId: string): Promise<Session> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}`);
    if (!response.ok) throw new Error('Failed to get session');
    return response.json();
  },

  async getSessionConversation(
    sessionId: string
  ): Promise<{ session: Session; messages: Message[] }> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/conversation`);
    if (!response.ok) throw new Error('Failed to get session conversation');
    return response.json();
  },

  async sendSessionMessage(sessionId: string, message: string): Promise<{ message_id: string }> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/message`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ message })
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to send session message'));
    return response.json();
  },

  /** `agent` says whether the agent's process was confirmed stopped ("unknown": it may still run). */
  async cancelSession(sessionId: string): Promise<{ status: string; agent?: string }> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/cancel`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to cancel session'));
    return response.json();
  },

  /** Clear one finished session from the list (kept for audit). A live one is refused (409). */
  async archiveSession(sessionId: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/archive`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to clear session'));
  },

  /** Clear every finished session (done, failed, cancelled); returns the ids cleared. */
  async archiveFinishedSessions(): Promise<string[]> {
    const response = await apiFetch(`${API_URL}/sessions/archive-finished`, { method: 'POST' });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to clear sessions'));
    return (await response.json()).archived;
  },

  // Notification endpoints
  async listNotifications(unreadOnly: boolean = true): Promise<SessionNotification[]> {
    const params = new URLSearchParams();
    params.set('unread_only', unreadOnly.toString());
    const response = await apiFetch(`${API_URL}/notifications?${params}`);
    if (!response.ok) throw new Error('Failed to list notifications');
    return response.json();
  },

  async dismissNotification(notificationId: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/notifications/${notificationId}/dismiss`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to dismiss notification');
  }
};
