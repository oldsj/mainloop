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
  message: Message;
}

export type QueueItemType =
  | 'question'
  | 'notification'
  | 'error'
  | 'review'
  | 'plan_ready'
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

export interface WorkerTask {
  id: string;
  main_thread_id: string;
  user_id: string;
  task_type: string;
  description: string;
  prompt: string;
  model: string | null;
  repo_url: string | null;
  branch_name: string | null;
  base_branch: string;
  status: string;
  workflow_run_id: string | null;
  worker_pod_name: string | null;
  created_at: string;
  started_at: string | null;
  completed_at: string | null;
  result: Record<string, unknown> | null;
  error: string | null;
  // Plan phase (issue)
  issue_url: string | null;
  issue_number: number | null;
  // Implementation phase (PR)
  pr_url: string | null;
  pr_number: number | null;
  commit_sha: string | null;
  conversation_id: string | null;
  message_id: string | null;
  keywords: string[];
  skip_plan: boolean;
}

export interface TaskContext {
  task: WorkerTask;
  queue_items: QueueItem[];
}

export interface TaskLogsResponse {
  logs: string;
  source: 'k8s' | 'none';
  task_status: string;
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

  // Task endpoints
  async listTasks(status?: string): Promise<WorkerTask[]> {
    const url = status ? `${API_URL}/tasks?status=${status}` : `${API_URL}/tasks`;
    const response = await fetch(url);
    if (!response.ok) throw new Error('Failed to list tasks');
    return response.json();
  },

  async getTask(taskId: string): Promise<WorkerTask> {
    const response = await fetch(`${API_URL}/tasks/${taskId}`);
    if (!response.ok) throw new Error('Failed to get task');
    return response.json();
  },

  async getTaskContext(taskId: string): Promise<TaskContext> {
    const response = await fetch(`${API_URL}/tasks/${taskId}/context`);
    if (!response.ok) throw new Error('Failed to get task context');
    return response.json();
  },

  async cancelTask(taskId: string): Promise<void> {
    const response = await fetch(`${API_URL}/tasks/${taskId}/cancel`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to cancel task');
  },

  async retryTask(taskId: string): Promise<void> {
    const response = await fetch(`${API_URL}/tasks/${taskId}/retry`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Failed to retry task');
  },

  async getTaskLogs(taskId: string, tail: number = 100): Promise<TaskLogsResponse> {
    const response = await fetch(`${API_URL}/tasks/${taskId}/logs?tail=${tail}`);
    if (!response.ok) throw new Error('Failed to get task logs');
    return response.json();
  }
};
