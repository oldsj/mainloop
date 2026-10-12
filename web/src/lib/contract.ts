/**
 * Names the UI uses for backend contract types. Every shape comes from the generated
 * `api/schema.gen.ts`, so a backend change that the UI does not handle fails tsc here.
 * Only the action kind is written by hand: the schema has no named type for it.
 */
import type { components, paths } from '@/api/schema.gen';

type Schemas = components['schemas'];

export type Session = Schemas['Session'];
export type SessionStatus = Schemas['SessionStatus'];
export type Message = Schemas['Message'];
export type NativeDelivery = Schemas['NativeDeliveryInfo'];
export type ProviderProfile = Schemas['ProviderProfile'];

export type Task = Schemas['Task'];
export type TaskStatus = Task['status'];
export type TaskReason = NonNullable<Task['reason']>;
export type TaskAttempt = Schemas['TaskAttempt'];
export type TaskOperation = Schemas['TaskOperation'];
export type TaskOperationState = TaskOperation['state'];
export type TaskProjection = Schemas['TaskProjection'];
export type TaskArtifact = Schemas['TaskArtifact'];
export type TaskReportRecord = Schemas['TaskReport'];
export type TaskEligibility = Schemas['TaskEligibility'];
export type TaskView = Schemas['TaskView'];
export type TaskUpdatedEvent = Schemas['TaskUpdated'];

/** The three owner actions. The schema has no enum for them: the route names are the source. */
export type TaskActionKind = 'retry' | 'reassign' | 'cancel';
export type TaskAction = Schemas['TaskAction'];
export type TaskReassign = Schemas['TaskReassign'];

/** The body the owner submits: retry and cancel send `TaskAction`, reassign adds a target. */
export type TaskActionBody = TaskAction | TaskReassign;

/** Human-in-the-loop requests: the payload the server sends, the receipt it keeps, and the answer. */
export type HITLTool = Schemas['HITLTool'];
export type HITLQuestion = Schemas['HITLQuestion'];
export type ToolApprovalRequest = Schemas['ToolApprovalRequest'];
export type AskUserRequest = Schemas['AskUserRequest'];
export type NestedHITLRequest = Schemas['NestedHITLRequest'];
export type HITLProjection = Schemas['HITLProjection'];
export type HITLContext = Schemas['HITLContext'];
export type HITLView = Schemas['HITLView'];
export type DecisionReceipt = Schemas['DecisionReceipt'];
/** What the owner submits. A receipt holds the output form, which always carries `reviewed_context`. */
export type HITLResponse = Schemas['ToolApprovalResponse'] | Schemas['AskUserResponse'];

/** Merge evidence for an approval: the proposal summary, verified per tool call by the server. */
export type MergeSummary = Schemas['MergeSummary'];
export type VerifiedMergeContext = Schemas['VerifiedMergeContext'];
export type MergePolicyView = Schemas['MergePolicyView'];
export type HITLMergeDetails = Schemas['HITLMergeDetails'];
export type HITLMergeDetailSection = HITLMergeDetails['section'];
export type MergeDetailItem = Schemas['MergeDetailItem'];

/** The orchestration health filters. The operation's query parameters are the source. */
export type HealthFilters = NonNullable<
  paths['/orchestration-health']['get']['parameters']['query']
>;
