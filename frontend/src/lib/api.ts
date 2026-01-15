/**
 * API client for backend communication
 */

const API_URL = import.meta.env.VITE_API_URL || 'http://localhost:8000';

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
  status: SessionStatus;
  worker_pod_name: string | null;
  created_at: string;
  started_at: string | null;
  completed_at: string | null;
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

export interface SessionCreate {
  title: string;
  description: string;
  prompt: string;
  repo_url?: string;
  anchor_message_id?: string;
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
    const response = await fetch(`${API_URL}/conversations`);
    if (!response.ok) throw new Error('Failed to list conversations');
    return response.json();
  },

  async getConversation(
    conversationId: string
  ): Promise<{ conversation: Conversation; messages: Message[] }> {
    const response = await fetch(`${API_URL}/conversations/${conversationId}`);
    if (!response.ok) throw new Error('Failed to get conversation');
    return response.json();
  },

  async sendMessage(request: ChatRequest): Promise<ChatResponse> {
    const response = await fetch(`${API_URL}/chat`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(request)
    });
    if (!response.ok) throw new Error('Failed to send message');
    return response.json();
  },

  // Inbox/Queue endpoints
  async getUnreadCount(): Promise<number> {
    const response = await fetch(`${API_URL}/queue/unread/count`);
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
    const response = await fetch(url);
    if (!response.ok) throw new Error('Failed to list queue items');
    return response.json();
  },

  async getQueueItem(itemId: string): Promise<QueueItem> {
    const response = await fetch(`${API_URL}/queue/${itemId}`);
    if (!response.ok) throw new Error('Failed to get queue item');
    return response.json();
  },

  async markQueueItemRead(itemId: string): Promise<void> {
    const response = await fetch(`${API_URL}/queue/${itemId}/read`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to mark queue item read');
  },

  async markAllQueueItemsRead(): Promise<void> {
    const response = await fetch(`${API_URL}/queue/read-all`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to mark all read');
  },

  async respondToQueueItem(itemId: string, responseText: string): Promise<void> {
    const response = await fetch(`${API_URL}/queue/${itemId}/respond`, {
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
    const response = await fetch(`${API_URL}/projects${params}`);
    if (!response.ok) throw new Error('Failed to list projects');
    return response.json();
  },

  async getProject(projectId: string): Promise<Project> {
    const response = await fetch(`${API_URL}/projects/${projectId}`);
    if (!response.ok) throw new Error('Failed to get project');
    return response.json();
  },

  async getProjectDetail(projectId: string): Promise<ProjectDetail> {
    const response = await fetch(`${API_URL}/projects/${projectId}/detail`);
    if (!response.ok) throw new Error('Failed to get project detail');
    return response.json();
  },

  async refreshProject(projectId: string): Promise<void> {
    const response = await fetch(`${API_URL}/projects/${projectId}/refresh`, {
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
    const response = await fetch(url);
    if (!response.ok) throw new Error('Failed to list sessions');
    return response.json();
  },

  async createSession(request: SessionCreate): Promise<Session> {
    const response = await fetch(`${API_URL}/sessions`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(request)
    });
    if (!response.ok) throw new Error('Failed to create session');
    return response.json();
  },

  async getSession(sessionId: string): Promise<Session> {
    const response = await fetch(`${API_URL}/sessions/${sessionId}`);
    if (!response.ok) throw new Error('Failed to get session');
    return response.json();
  },

  async getSessionConversation(
    sessionId: string
  ): Promise<{ session: Session; messages: Message[] }> {
    const response = await fetch(`${API_URL}/sessions/${sessionId}/conversation`);
    if (!response.ok) throw new Error('Failed to get session conversation');
    return response.json();
  },

  async sendSessionMessage(sessionId: string, message: string): Promise<{ message_id: string }> {
    const response = await fetch(`${API_URL}/sessions/${sessionId}/message`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ message })
    });
    if (!response.ok) throw new Error('Failed to send session message');
    return response.json();
  },

  async cancelSession(sessionId: string): Promise<void> {
    const response = await fetch(`${API_URL}/sessions/${sessionId}/cancel`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to cancel session');
  },

  // Notification endpoints
  async listNotifications(unreadOnly: boolean = true): Promise<SessionNotification[]> {
    const params = new URLSearchParams();
    params.set('unread_only', unreadOnly.toString());
    const response = await fetch(`${API_URL}/notifications?${params}`);
    if (!response.ok) throw new Error('Failed to list notifications');
    return response.json();
  },

  async dismissNotification(notificationId: string): Promise<void> {
    const response = await fetch(`${API_URL}/notifications/${notificationId}/dismiss`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to dismiss notification');
  }
};
