export type ProviderProfileId = string;

export interface ProviderProfile {
  id: ProviderProfileId;
  display_name: string;
  runtime_adapter: 'kagent';
  native_provider: 'claude' | 'codex';
  configuration_revision: string;
  agents: Partial<
    Record<'main' | 'supervisor' | 'child' | 'agent', { namespace: string; name: string }>
  >;
  aliases: ProviderProfileId[];
  enabled: boolean;
  capabilities: {
    capability: string;
    state: 'proved' | 'partial' | 'unsupported' | 'unknown';
    scope: 'fixture' | 'live' | 'unverified';
    evidence_ref: string | null;
    detail: string | null;
  }[];
}

/**
 * API client for backend communication
 */

import type {
  HITLMergeDetailSection,
  HITLMergeDetails,
  HITLView,
  HITLResponse,
  MergePolicyView
} from './hitl';
import { API_URL } from '$lib/config';
import { connection } from '$lib/stores/connection';

/** A read request failed with an HTTP response; the backend was reachable. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}

/**
 * A task read or action the backend answered with an HTTP error. The task routes put a typed
 * code in `detail.reason` (e.g. `stale_task_version`); `reason` is null for other bodies.
 */
export class TaskApiError extends ApiError {
  constructor(
    message: string,
    status: number,
    readonly reason: string | null
  ) {
    super(message, status);
  }
}

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
    // FastAPI's own request validation answers with a list of {msg}.
    if (Array.isArray(body?.detail)) {
      const messages = body.detail.map((item: { msg?: unknown }) => item?.msg).filter(Boolean);
      if (messages.length) return messages.join('; ');
    }
  } catch {
    // not JSON (e.g. a proxy's error page)
  }
  return fallback;
}

async function postWorkspace(body: Record<string, unknown>): Promise<WorkspaceLifecycle> {
  const response = await apiFetch(`${API_URL}/workspaces`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (!response.ok) throw new Error(await errorDetail(response, 'Failed to create workspace'));
  return response.json();
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
  pending?: boolean; // native main thread: the reply is mirrored from the kagent task; poll the conversation
  delivery_message_id?: string | null;
}

export type QueueItemType =
  | 'hitl_request'
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
  agent_kind?: 'claude' | 'codex' | null;
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
  task_id?: string | null;
  source?: string;
  evidence_ref: string | null;
  detail: string | null;
}

export type WorkspaceObservedState =
  | 'running'
  | 'suspending'
  | 'suspended'
  | 'resuming'
  | 'failed'
  | 'unknown';

export interface WorkspacePort {
  name: string;
  number: number;
}

export interface WorkspaceDev {
  ports: WorkspacePort[];
  idle_timeout_minutes: number;
}

export interface WorkspaceManifest {
  repo_url: string;
  ref: string;
  branch: string;
  depth: number;
  agent_kind: 'claude' | 'codex';
  dev: WorkspaceDev;
  development_environment?: {
    environment_id: string;
    version_id: string;
    image: string;
    platform: string;
    policy_identity: string;
  } | null;
}

export interface WorkspaceLifecycle {
  publication_mode: 'read_only' | 'branch';
  publication_reason:
    | 'default_branch'
    | 'protected_branch'
    | 'missing_metadata'
    | 'no_grant'
    | 'disabled'
    | null;
  workspace_id: string;
  session_id: string;
  observed_state: WorkspaceObservedState;
  /** Why the state is what it is, when kagent said (a failure, an operation in progress). */
  detail: string | null;
  manifest: WorkspaceManifest;
  last_activity_at: string | null;
  updated_at: string;
}

export interface WorkspacePreviewPort {
  port: number;
  name: string;
  url: string;
}

export interface TopicLine {
  name: string;
  status_line: string;
  pending: number;
}

export interface MainThreadInfo {
  mode: 'native';
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
  agent_name: string;
  kagent_session_id: string | null;
  session_state: string | null;
  model: string | null;
  turns: number;
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

// Durable task contracts (models/src/models/task.py, docs/specs/tasks.md). Fields that later
// backend slices add (PR, CI and merge observations) are optional: absent means unknown.
export type TaskStatus =
  | 'queued'
  | 'running'
  | 'waiting'
  | 'blocked'
  | 'completed'
  | 'failed'
  | 'cancelled';

export type TaskReason =
  | 'awaiting_child'
  | 'approval'
  | 'ci'
  | 'publication'
  | 'handoff'
  | 'reconciliation'
  | 'provisioning_unavailable'
  | 'handoff_unavailable'
  | 'cancel_unavailable';

export type TaskAttemptState =
  | 'creating'
  | 'active'
  | 'draining'
  | 'fenced'
  | 'superseded'
  | 'failed'
  | 'cancelled'
  | 'completed';

export type TaskOperationState =
  | 'requested'
  | 'draining'
  | 'checkpoint_required'
  | 'checkpoint_verified'
  | 'source_fencing'
  | 'source_fenced'
  | 'target_creating'
  | 'target_ready'
  | 'completed'
  | 'blocked'
  | 'uncertain';

export type TaskActionKind = 'retry' | 'reassign' | 'cancel';

export interface TaskCheckout {
  branch: string;
  ref: string;
  depth: number;
}

export interface Task {
  id: string;
  owner_id: string;
  project_id: string | null;
  topic_id: string | null;
  parent_task_id: string | null;
  root_task_id: string;
  creator_binding_id: string | null;
  title: string;
  brief: string;
  mode: 'code' | 'coordination';
  assigned_profile_id: string;
  selection_source: 'explicit' | 'project_default' | 'installation_default' | 'inherited';
  provider_constraint: string | null;
  accepted_environment?: WorkspaceManifest['development_environment'];
  status: TaskStatus;
  reason: TaskReason | null;
  current_attempt_id: string | null;
  version: number;
  checkout: TaskCheckout | null;
  created_at: string;
  updated_at: string;
}

export interface TaskAttempt {
  id: string;
  task_id: string;
  number: number;
  profile_id: string;
  native_provider: 'claude' | 'codex';
  configuration_revision: string;
  agent_ref: { namespace: string; name: string };
  role: 'supervisor' | 'child';
  depth: 1 | 2;
  writer_generation: number | null;
  session_id: string | null;
  binding_id: string | null;
  workspace_id: string | null;
  state: TaskAttemptState;
  brief_delivery_id?: string | null;
  evidence_refs?: string[];
  result_ref?: string | null;
  environment?: WorkspaceManifest['development_environment'];
  initial_ref?: string | null;
  manifest_ref?: string | null;
  archived_at?: string | null;
  native_deleted_at?: string | null;
  retention_hold?: string | null;
  predecessor_id: string | null;
  successor_id: string | null;
  checkpoint_ref: string | null;
  superseded_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface TaskOperation {
  id: string;
  owner_id: string;
  principal_key: string;
  request_digest: string;
  request_payload?: Record<string, unknown>;
  kind: 'create' | 'retry' | 'reassign' | 'cancel';
  request_id: string;
  task_id: string | null;
  attempt_id: string | null;
  state: TaskOperationState;
  last_confirmed_step: TaskOperationState;
  source_attempt_id: string | null;
  target_attempt_id: string | null;
  checkpoint_ref: string | null;
  manifest_ref?: string | null;
  reason: TaskReason | null;
  created_at: string;
  updated_at: string;
}

export type TaskEligibility =
  | { available: true; reason?: TaskReason | null }
  | { available: false; reason: TaskReason };

export interface TaskProjection {
  agent_activity?: string | null;
  delivery_state?: string | null;
  workspace_health?: string | null;
  environment_version_id?: string | null;
  repository?: string | null;
  branch?: string | null;
  pr_url?: string | null;
  pr_number?: number | null;
  pr_head_sha?: string | null;
  pr_state?: 'open' | 'closed' | 'merged' | 'unknown';
  ci_state?: 'pending' | 'success' | 'failure' | 'unknown';
  ci_head_sha?: string | null;
  merge_state?: string | null;
  merge_proposal_id?: string | null;
  publication_state?: string | null;
  pending_approval_ids?: string[];
  observed_at?: string | null;
}

export interface TaskArtifact {
  id: string;
  operation_id: string;
  kind: 'checkpoint' | 'handoff_manifest' | 'unverified_provider_summary';
  sha256: string;
  payload: Record<string, unknown>;
}

export interface TaskReportRecord {
  task_id: string;
  attempt_id: string;
  request_id: string;
  summary: string;
  outcome: 'progress' | 'completed' | 'failed' | 'blocked';
  evidence_refs?: string[];
}

export interface TaskView {
  task: Task;
  attempts: TaskAttempt[];
  operations: TaskOperation[];
  artifacts: TaskArtifact[];
  reports: TaskReportRecord[];
  projection: TaskProjection;
  actions: Partial<Record<TaskActionKind, TaskEligibility>>;
}

/** Body of retry, reassign and cancel. The version and attempt are the ones the owner saw. */
export interface TaskActionBody {
  request_id: string;
  expected_version: number;
  expected_attempt_id: string | null;
  target_profile_id?: string;
}

/** Payload of the `task:updated` SSE event. It names a change; the task itself comes from GET. */
export interface TaskUpdatedEvent {
  type: 'task:updated';
  event_id: string;
  task_id: string;
  version: number;
  attempt_id: string | null;
  root_task_id: string;
  parent_task_id: string | null;
  occurred_at: string;
}

async function taskResponse<T>(response: Response, fallback: string): Promise<T> {
  if (response.ok) return response.json();
  let reason: string | null = null;
  let message = fallback;
  try {
    const body = await response.json();
    if (typeof body?.detail === 'string') message = body.detail;
    else if (typeof body?.detail?.reason === 'string') reason = message = body.detail.reason;
  } catch {
    // not JSON
  }
  throw new TaskApiError(message, response.status, reason);
}

function taskActionRequest(body: TaskActionBody): RequestInit {
  return {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  };
}

function readSignal(signal?: AbortSignal): AbortSignal {
  return AbortSignal.any([AbortSignal.timeout(10000), ...(signal ? [signal] : [])]);
}

export const api = {
  async getHITL(id: string, signal?: AbortSignal): Promise<HITLView> {
    const response = await apiFetch(`${API_URL}/hitl/${encodeURIComponent(id)}`, {
      signal: readSignal(signal)
    });
    if (!response.ok)
      throw new ApiError(await errorDetail(response, 'Could not load request'), response.status);
    return response.json();
  },
  async getHITLMergeDetails(
    requestId: string,
    proposalId: string,
    section: HITLMergeDetailSection,
    cursor = 0,
    signal?: AbortSignal
  ): Promise<HITLMergeDetails> {
    const query = new URLSearchParams({ section, cursor: String(cursor), limit: '50' });
    const response = await apiFetch(
      `${API_URL}/hitl/${encodeURIComponent(requestId)}/merge/${encodeURIComponent(proposalId)}/details?${query}`,
      { signal: readSignal(signal) }
    );
    if (!response.ok)
      throw new ApiError(
        await errorDetail(response, 'Could not load merge details'),
        response.status
      );
    return response.json();
  },
  async listSessionHITL(id: string, signal?: AbortSignal): Promise<string[]> {
    const response = await apiFetch(`${API_URL}/sessions/${encodeURIComponent(id)}/hitl`, {
      signal: readSignal(signal)
    });
    if (!response.ok) throw new ApiError('Could not load session input', response.status);
    return response.json();
  },
  async respondHITL(id: string, action_id: string, responseBody: HITLResponse): Promise<HITLView> {
    const response = await apiFetch(`${API_URL}/hitl/${encodeURIComponent(id)}/respond`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action_id, response: responseBody })
    });
    if (!response.ok)
      throw new ApiError(await errorDetail(response, 'Could not record decision'), response.status);
    return response.json();
  },
  async getMergePolicy(id: string): Promise<MergePolicyView> {
    const response = await apiFetch(`${API_URL}/projects/${encodeURIComponent(id)}/merge-policy`);
    if (!response.ok) throw new ApiError('Could not load merge policy', response.status);
    return response.json();
  },
  async updateMergePolicy(
    id: string,
    merge_policy: 'auto' | 'approval',
    expected_version: number
  ): Promise<MergePolicyView> {
    const response = await apiFetch(`${API_URL}/projects/${encodeURIComponent(id)}/merge-policy`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ merge_policy, expected_version })
    });
    if (!response.ok)
      throw new ApiError(
        await errorDetail(response, 'Could not save merge policy'),
        response.status
      );
    return response.json();
  },
  async listTasks(
    filter: { project_id?: string; parent_task_id?: string } = {},
    signal?: AbortSignal
  ): Promise<TaskView[]> {
    const params = new URLSearchParams();
    if (filter.project_id) params.set('project_id', filter.project_id);
    if (filter.parent_task_id) params.set('parent_task_id', filter.parent_task_id);
    const url = params.size ? `${API_URL}/tasks?${params}` : `${API_URL}/tasks`;
    const response = await apiFetch(url, { signal: readSignal(signal) });
    return taskResponse(response, 'Could not load tasks');
  },
  async getTask(id: string, signal?: AbortSignal): Promise<TaskView> {
    const response = await apiFetch(`${API_URL}/tasks/${encodeURIComponent(id)}`, {
      signal: readSignal(signal)
    });
    return taskResponse(response, 'Could not load task');
  },
  async getTaskOperation(id: string, signal?: AbortSignal): Promise<TaskOperation> {
    const response = await apiFetch(`${API_URL}/task-operations/${encodeURIComponent(id)}`, {
      signal: readSignal(signal)
    });
    return taskResponse(response, 'Could not load task operation');
  },
  /** Retry, reassign and cancel answer 202 with the durable operation; the task changes later. */
  async retryTask(id: string, body: TaskActionBody): Promise<TaskOperation> {
    const response = await apiFetch(
      `${API_URL}/tasks/${encodeURIComponent(id)}/retry`,
      taskActionRequest(body)
    );
    return taskResponse(response, 'Could not retry the task');
  },
  async reassignTask(id: string, body: TaskActionBody): Promise<TaskOperation> {
    const response = await apiFetch(
      `${API_URL}/tasks/${encodeURIComponent(id)}/reassign`,
      taskActionRequest(body)
    );
    return taskResponse(response, 'Could not reassign the task');
  },
  async cancelTask(id: string, body: TaskActionBody): Promise<TaskOperation> {
    const response = await apiFetch(
      `${API_URL}/tasks/${encodeURIComponent(id)}/cancel`,
      taskActionRequest(body)
    );
    return taskResponse(response, 'Could not cancel the task');
  },
  async listProviders(signal?: AbortSignal): Promise<ProviderProfile[]> {
    const response = await apiFetch(`${API_URL}/providers`, { signal: readSignal(signal) });
    if (!response.ok)
      throw new ApiError(await errorDetail(response, 'Could not load providers'), response.status);
    return response.json();
  },

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
      // The native main thread answers 409 with a reason (a turn still in flight).
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

  async listQueueItems(
    options?: {
      status?: string;
      unreadOnly?: boolean;
      taskId?: string;
    },
    signal?: AbortSignal
  ): Promise<QueueItem[]> {
    const params = new URLSearchParams();
    if (options?.status) params.set('status', options.status);
    if (options?.unreadOnly) params.set('unread_only', 'true');
    if (options?.taskId) params.set('task_id', options.taskId);

    const url = params.toString() ? `${API_URL}/queue?${params}` : `${API_URL}/queue`;
    const response = await apiFetch(url, { signal: readSignal(signal) });
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

  async createWorkspace(
    projectId: string,
    branch: string,
    options: { ref?: string; dev?: WorkspaceDev; agent_kind?: WorkspaceManifest['agent_kind'] } = {}
  ): Promise<WorkspaceLifecycle> {
    return postWorkspace({ project_id: projectId, branch, ...options });
  },

  /**
   * Create a workspace from a GitHub repository (`owner/name` or an https://github.com URL).
   * The backend finds or creates the project. An empty branch lets it choose one.
   */
  async createWorkspaceFromRepo(
    repo: string,
    branch = '',
    options: { ref?: string; dev?: WorkspaceDev; agent_kind?: WorkspaceManifest['agent_kind'] } = {}
  ): Promise<WorkspaceLifecycle> {
    return postWorkspace({ repo, branch, ...options });
  },

  async deleteWorkspace(workspaceId: string): Promise<void> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}`, { method: 'DELETE' });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to delete workspace'));
  },

  /**
   * Get the SSE endpoint URL for the global event stream.
   */
  getEventsStreamUrl(): string {
    return `${API_URL}/events`;
  },

  // Session endpoints
  async listWorkspaces(): Promise<WorkspaceLifecycle[]> {
    const response = await apiFetch(`${API_URL}/workspaces`);
    if (!response.ok) throw new Error('Failed to list workspaces');
    return response.json();
  },

  async getWorkspace(workspaceId: string): Promise<WorkspaceLifecycle> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}`);
    if (!response.ok) {
      throw new ApiError(await errorDetail(response, 'Failed to get workspace'), response.status);
    }
    return response.json();
  },

  async suspendWorkspace(workspaceId: string): Promise<WorkspaceLifecycle> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}/suspend`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to suspend workspace'));
    return response.json();
  },

  async resumeWorkspace(workspaceId: string): Promise<WorkspaceLifecycle> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}/resume`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to resume workspace'));
    return response.json();
  },

  async refreshWorkspace(workspaceId: string): Promise<WorkspaceLifecycle> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}/refresh`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to refresh workspace'));
    return response.json();
  },

  async listWorkspacePreviewPorts(workspaceId: string): Promise<WorkspacePreviewPort[]> {
    const response = await apiFetch(`${API_URL}/workspaces/${workspaceId}/ports`);
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to list preview ports'));
    const result = await response.json();
    if (!Array.isArray(result?.ports)) throw new Error('Invalid preview port response');
    return result.ports;
  },

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
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to create session'));
    return response.json();
  },

  async getMainThread(): Promise<MainThreadInfo> {
    const response = await apiFetch(`${API_URL}/main-thread`);
    if (!response.ok) throw new Error('Failed to get main thread');
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
    if (!response.ok) throw new ApiError('Failed to get session', response.status);
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
    if (!response.ok)
      throw new Error(await errorDetail(response, 'Failed to send session message'));
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

  /**
   * Stop the open turn without ending the session. `status`: "stopped", "finished" (the turn
   * ended on its own first) or "no_open_turn". A kagent failure is an error, and the turn stays.
   */
  async stopTurn(sessionId: string): Promise<{ status: string }> {
    const response = await apiFetch(`${API_URL}/sessions/${sessionId}/stop-turn`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error(await errorDetail(response, 'Failed to stop the turn'));
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
