/**
 * Names the UI uses for backend contract types. Every shape comes from the generated
 * `api/schema.gen.ts`, so a backend change that the UI does not handle fails tsc here.
 * Only the action kind and the health filters are written by hand: the schema has no named
 * type for either.
 */
import type { components } from '@/api/schema.gen';

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

export interface HealthFilters {
  project_id?: string;
  task_id?: string;
  attempt_id?: string;
  session_id?: string;
  provider?: string;
  hours?: number;
}
