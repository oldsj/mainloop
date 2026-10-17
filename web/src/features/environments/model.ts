/**
 * Pure rules for environment selection and registration: which versions a project may use, what a
 * draft can save, how the saved line reads, and when a registration reply is uncertain. Free of
 * React, so each rule has a unit test without a router or a query client.
 */
import { z } from 'zod';
import { ApiError, type components } from '@/api';
import { contractEnum } from '@/lib/contractEnum';

type Schemas = components['schemas'];
export type Selection = Schemas['ProjectEnvironmentSelection'];
export type RegisteredEnvironment = Schemas['RegisteredEnvironment'];
export type Architecture = NonNullable<Schemas['EnvironmentVersion']['architecture']>;

/** The only validator a version may carry to be selected. Older validators are obsolete policy. */
export const VALIDATOR_VERSION = 'oci-static-v2';

/** `registry/repository@sha256:digest`. Used by the input's pattern attribute and by the save check. */
export const IMAGE_PATTERN = '[^\\s]+/[^\\s]+@sha256:[0-9a-f]{64}';
const IMAGE_RE = new RegExp(`^${IMAGE_PATTERN}$`);

const SOURCE_KINDS = contractEnum<Schemas['DevEnvironment']['source_kind']>()([
  'prebuilt_image',
  'definition_repo',
  'operator_default'
]);
const VALIDATION_STATUSES = contractEnum<Schemas['EnvironmentVersion']['validation_status']>()([
  'pending_build',
  'static_validated'
]);
const ARCHITECTURES = contractEnum<Architecture>()(['amd64', 'arm64']);

/** The environment fields the selection and registration forms read. */
export const environmentRowSchema = z.object({
  id: z.string().min(1),
  name: z.string(),
  source_kind: z.enum(SOURCE_KINDS),
  accepted_default_version_id: z.string().nullable()
}) satisfies z.ZodType<
  Pick<Schemas['DevEnvironment'], 'id' | 'name' | 'source_kind' | 'accepted_default_version_id'>
>;
export type EnvironmentRow = z.output<typeof environmentRowSchema>;

/** The version fields the selection form reads to decide whether a version can be chosen. */
export const versionRowSchema = z.object({
  id: z.string().min(1),
  environment_id: z.string().min(1),
  validation_status: z.enum(VALIDATION_STATUSES),
  validator_version: z.string(),
  architecture: z.enum(ARCHITECTURES).nullable()
}) satisfies z.ZodType<
  Pick<
    Schemas['EnvironmentVersion'],
    'id' | 'environment_id' | 'validation_status' | 'validator_version' | 'architecture'
  >
>;
export type VersionRow = z.output<typeof versionRowSchema>;

export const selectionSchema = z.object({
  project_id: z.string().min(1),
  environment_id: z.string().min(1),
  version_id: z.string().nullable(),
  follow_default: z.boolean(),
  revision: z.number().int().nonnegative(),
  access_revoked: z.boolean(),
  resolved_version_id: z.string().nullable()
}) satisfies z.ZodType<
  Pick<
    Selection,
    | 'project_id'
    | 'environment_id'
    | 'version_id'
    | 'follow_default'
    | 'revision'
    | 'access_revoked'
    | 'resolved_version_id'
  >
>;

export function isImageReference(value: string): boolean {
  return IMAGE_RE.test(value.trim());
}

/** A version a project may select: statically validated by the current validator. */
export function eligible(version: VersionRow | undefined): boolean {
  return (
    version?.validation_status === 'static_validated' &&
    version.validator_version === VALIDATOR_VERSION
  );
}

export function versionLabel(version: VersionRow): string {
  const status = eligible(version)
    ? 'Statically validated'
    : 'Unavailable: pending build or obsolete policy';
  return `${version.id} · ${version.architecture ?? 'No platform'} · ${status}`;
}

/** The owner's unsaved choice. It starts from the saved selection and never follows a refetch. */
export interface Draft {
  environmentId: string;
  versionId: string;
  follow: boolean;
}

export function draftFrom(saved: Selection | null): Draft {
  if (!saved) return { environmentId: '', versionId: '', follow: false };
  return {
    environmentId: saved.environment_id,
    versionId: saved.version_id ?? '',
    follow: saved.follow_default
  };
}

/** The version a save would make current: the environment's accepted default when following, else the chosen one. */
export function targetVersionId(
  draft: Draft,
  environment: EnvironmentRow | undefined
): string | null {
  return draft.follow
    ? (environment?.accepted_default_version_id ?? null)
    : draft.versionId || null;
}

export function sameAsSaved(draft: Draft, saved: Selection | null): boolean {
  const base = draftFrom(saved);
  return (
    base.environmentId === draft.environmentId &&
    base.follow === draft.follow &&
    (draft.follow || base.versionId === draft.versionId)
  );
}

/**
 * Whether Save may send the draft. A pending or obsolete version blocks it, and so does following an
 * environment whose accepted default is not eligible. An unchanged draft has nothing to save.
 */
export function canSaveSelection(opts: {
  draft: Draft;
  saved: Selection | null;
  environment: EnvironmentRow | undefined;
  versions: VersionRow[];
  busy: boolean;
}): boolean {
  if (opts.busy || !opts.draft.environmentId || sameAsSaved(opts.draft, opts.saved)) return false;
  const target = targetVersionId(opts.draft, opts.environment);
  return target !== null && eligible(opts.versions.find((version) => version.id === target));
}

/** The body for `PUT /projects/{id}/environment`. The revision is the one the owner last saw; 0 means none saved. */
export function selectionRequest(draft: Draft, saved: Selection | null) {
  return {
    environment_id: draft.environmentId,
    version_id: draft.follow ? null : draft.versionId,
    follow_default: draft.follow,
    expected_version: saved?.revision ?? 0
  };
}

/** Accepting makes a chosen version the environment's default, so it must be eligible and not already the default. */
export function canAcceptDefault(opts: {
  draft: Draft;
  environment: EnvironmentRow | undefined;
  versions: VersionRow[];
  busy: boolean;
}): boolean {
  const { draft, environment, versions, busy } = opts;
  if (busy || draft.follow || !draft.versionId) return false;
  if (draft.versionId === environment?.accepted_default_version_id) return false;
  return eligible(versions.find((version) => version.id === draft.versionId));
}

/** Following needs an eligible accepted default. Without one the owner must choose a version and accept it. */
export function needsAcceptedDefault(
  draft: Draft,
  environment: EnvironmentRow | undefined,
  versions: VersionRow[]
): boolean {
  if (!draft.follow || !draft.environmentId) return false;
  return !eligible(versions.find((v) => v.id === environment?.accepted_default_version_id));
}

export function savedLine(saved: Selection, environment: EnvironmentRow | undefined): string {
  const name = environment?.name ?? saved.environment_id;
  const mode = saved.follow_default
    ? 'Follow accepted default'
    : `Fixed version · ${saved.resolved_version_id ?? 'No accepted default'}`;
  return `Saved: ${name} · ${mode}${saved.access_revoked ? ' · Access revoked' : ''}`;
}

export function conflictText(message: string): string {
  return `${message}. Review the refreshed saved selection before saving again.`;
}

export interface RegisterDraft {
  name: string;
  image: string;
  architecture: Architecture;
}

export function registerRequest(draft: RegisterDraft) {
  return {
    name: draft.name.trim(),
    source_kind: 'prebuilt_image' as const,
    image: draft.image.trim(),
    architecture: draft.architecture,
    follow_watched_tag: false
  };
}

export function canRegister(draft: RegisterDraft, busy: boolean, uncertain: boolean): boolean {
  return !busy && !uncertain && draft.name.trim() !== '' && isImageReference(draft.image);
}

/**
 * A registration reply is uncertain when the server may have created the environment anyway: no
 * reply at all, or a 5xx. Only then does the form stay locked against a second POST.
 */
export function isUncertain(error: unknown): boolean {
  return !(error instanceof ApiError) || error.status >= 500;
}

export function registrationNotice(result: RegisteredEnvironment): string {
  return `Registered ${result.environment.name}: ${result.version.validation_status}, ${result.version.validator_version}. Runtime capabilities have not been tested.`;
}
