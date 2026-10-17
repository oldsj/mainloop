import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];
export type EnvironmentFixture = Schemas['DevEnvironment'];
export type VersionFixture = Schemas['EnvironmentVersion'];
export type SelectionFixture = Schemas['ProjectEnvironmentSelection'];

/** A user-owned environment. `accepted` names the version it currently defaults to, or null. */
export function environment(overrides: Partial<EnvironmentFixture> = {}): EnvironmentFixture {
  return {
    id: 'env',
    owner_id: 'dev-user',
    name: 'Python tools',
    source_kind: 'prebuilt_image',
    watched_tag: null,
    follow_watched_tag: false,
    accepted_default_version_id: 'v1',
    definition: null,
    ...overrides
  };
}

/** A version the current validator accepted: eligible for selection. */
export function version(overrides: Partial<VersionFixture> = {}): VersionFixture {
  return {
    id: 'v1',
    environment_id: 'env',
    registry: 'ghcr.io',
    repository: 'example/tools',
    index_digest: null,
    platform_manifest_digest: null,
    config_digest: null,
    declared_user: null,
    architecture: 'amd64',
    capability_profile: {},
    validation_status: 'static_validated',
    validation_result: {},
    validator_version: 'oci-static-v2',
    provenance_kind: 'user_pushed',
    base_provenance: 'unknown',
    parent_version_id: null,
    package_declaration: null,
    approval_reference: null,
    definition: null,
    ...overrides
  };
}

/** A version the build has not validated yet: not selectable. */
export function pendingVersion(overrides: Partial<VersionFixture> = {}): VersionFixture {
  return version({ id: 'v-pending', validation_status: 'pending_build', ...overrides });
}

/** A version validated by an older validator: obsolete policy, not selectable. */
export function obsoleteVersion(overrides: Partial<VersionFixture> = {}): VersionFixture {
  return version({ id: 'v-old', validator_version: 'oci-static-v1', ...overrides });
}

export function selection(overrides: Partial<SelectionFixture> = {}): SelectionFixture {
  return {
    project_id: 'project-a',
    environment_id: 'env',
    version_id: 'v1',
    follow_default: false,
    revision: 1,
    access_revoked: false,
    resolved_version_id: 'v1',
    ...overrides
  };
}
