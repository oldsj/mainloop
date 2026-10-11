import { describe, expect, it } from 'vitest';
import { ApiError } from '@/api';
import {
  canAcceptDefault,
  canRegister,
  canSaveSelection,
  conflictText,
  draftFrom,
  eligible,
  isImageReference,
  isUncertain,
  needsAcceptedDefault,
  registerRequest,
  registrationNotice,
  savedLine,
  selectionRequest,
  targetVersionId,
  versionLabel,
  type Draft
} from './model';
import { environment, obsoleteVersion, pendingVersion, selection, version } from './test/fixtures';

const IMAGE = `ghcr.io/example/tools@sha256:${'a'.repeat(64)}`;
const fixed = (versionId: string, extra: Partial<Draft> = {}): Draft => ({
  environmentId: 'env',
  versionId,
  follow: false,
  ...extra
});

describe('eligible and labels', () => {
  it('only a static_validated version from the current validator is selectable', () => {
    expect(eligible(version())).toBe(true);
    expect(eligible(pendingVersion())).toBe(false);
    expect(eligible(obsoleteVersion())).toBe(false);
    expect(eligible(undefined)).toBe(false);
  });

  it('labels show the platform, and an unavailable reason for ineligible versions', () => {
    expect(versionLabel(version({ id: 'v2' }))).toBe('v2 · amd64 · Statically validated');
    expect(versionLabel(pendingVersion({ id: 'v3' }))).toBe(
      'v3 · amd64 · Unavailable: pending build or obsolete policy'
    );
    expect(versionLabel(version({ id: 'v4', architecture: null }))).toBe(
      'v4 · No platform · Statically validated'
    );
  });
});

describe('draft and target', () => {
  it('starts from the saved selection, or empty when none is saved', () => {
    expect(draftFrom(null)).toEqual({ environmentId: '', versionId: '', follow: false });
    expect(
      draftFrom(selection({ environment_id: 'env', version_id: 'v2', follow_default: true }))
    ).toEqual({
      environmentId: 'env',
      versionId: 'v2',
      follow: true
    });
    expect(draftFrom(selection({ version_id: null, follow_default: false }))).toMatchObject({
      versionId: ''
    });
  });

  it('a follow draft targets the environment default; a fixed draft targets its version', () => {
    const env = environment({ accepted_default_version_id: 'v1' });
    expect(targetVersionId({ environmentId: 'env', versionId: 'v9', follow: true }, env)).toBe(
      'v1'
    );
    expect(targetVersionId(fixed('v2'), env)).toBe('v2');
    expect(targetVersionId(fixed(''), env)).toBeNull();
    expect(
      targetVersionId({ environmentId: 'env', versionId: '', follow: true }, undefined)
    ).toBeNull();
  });
});

describe('canSaveSelection', () => {
  const env = environment({ accepted_default_version_id: 'v1' });
  const versions = [version({ id: 'v1' }), version({ id: 'v2' }), pendingVersion({ id: 'v3' })];

  it('allows an eligible fixed version that differs from the saved one', () => {
    expect(
      canSaveSelection({ draft: fixed('v2'), saved: null, environment: env, versions, busy: false })
    ).toBe(true);
  });

  it('refuses a pending or obsolete version, and a follow whose default is not eligible', () => {
    expect(
      canSaveSelection({ draft: fixed('v3'), saved: null, environment: env, versions, busy: false })
    ).toBe(false);
    const badDefault = environment({ accepted_default_version_id: 'v3' });
    expect(
      canSaveSelection({
        draft: { environmentId: 'env', versionId: '', follow: true },
        saved: null,
        environment: badDefault,
        versions,
        busy: false
      })
    ).toBe(false);
  });

  it('refuses an unchanged draft, a missing environment, and a busy save', () => {
    const saved = selection({ environment_id: 'env', version_id: 'v2', follow_default: false });
    expect(
      canSaveSelection({ draft: fixed('v2'), saved, environment: env, versions, busy: false })
    ).toBe(false);
    expect(
      canSaveSelection({
        draft: { environmentId: '', versionId: 'v2', follow: false },
        saved: null,
        environment: env,
        versions,
        busy: false
      })
    ).toBe(false);
    expect(
      canSaveSelection({ draft: fixed('v2'), saved: null, environment: env, versions, busy: true })
    ).toBe(false);
  });
});

describe('selectionRequest', () => {
  it('sends the saved revision, or 0 when nothing is saved', () => {
    expect(selectionRequest(fixed('v2'), null)).toEqual({
      environment_id: 'env',
      version_id: 'v2',
      follow_default: false,
      expected_version: 0
    });
    expect(
      selectionRequest(
        { environmentId: 'env', versionId: 'v2', follow: true },
        selection({ revision: 4 })
      )
    ).toEqual({
      environment_id: 'env',
      version_id: null,
      follow_default: true,
      expected_version: 4
    });
  });
});

describe('canAcceptDefault and needsAcceptedDefault', () => {
  const env = environment({ accepted_default_version_id: 'v1' });
  const versions = [version({ id: 'v1' }), version({ id: 'v2' }), pendingVersion({ id: 'v3' })];

  it('accepts only an eligible, different, fixed version', () => {
    expect(canAcceptDefault({ draft: fixed('v2'), environment: env, versions, busy: false })).toBe(
      true
    );
    expect(canAcceptDefault({ draft: fixed('v1'), environment: env, versions, busy: false })).toBe(
      false
    );
    expect(canAcceptDefault({ draft: fixed('v3'), environment: env, versions, busy: false })).toBe(
      false
    );
    expect(
      canAcceptDefault({
        draft: { environmentId: 'env', versionId: 'v2', follow: true },
        environment: env,
        versions,
        busy: false
      })
    ).toBe(false);
    expect(canAcceptDefault({ draft: fixed('v2'), environment: env, versions, busy: true })).toBe(
      false
    );
  });

  it('following needs an eligible accepted default', () => {
    const follow: Draft = { environmentId: 'env', versionId: '', follow: true };
    expect(needsAcceptedDefault(follow, env, versions)).toBe(false);
    expect(
      needsAcceptedDefault(follow, environment({ accepted_default_version_id: 'v3' }), versions)
    ).toBe(true);
    expect(needsAcceptedDefault(fixed('v2'), env, versions)).toBe(false);
  });
});

describe('savedLine and conflictText', () => {
  it('reads the saved selection in words, and flags revoked access', () => {
    const env = environment({ name: 'Python tools' });
    expect(savedLine(selection({ follow_default: true }), env)).toBe(
      'Saved: Python tools · Follow accepted default'
    );
    expect(savedLine(selection({ follow_default: false, resolved_version_id: 'v2' }), env)).toBe(
      'Saved: Python tools · Fixed version · v2'
    );
    expect(savedLine(selection({ follow_default: false, resolved_version_id: null }), env)).toBe(
      'Saved: Python tools · Fixed version · No accepted default'
    );
    expect(savedLine(selection({ access_revoked: true }), undefined)).toMatch(
      /^Saved: env · .* · Access revoked$/
    );
  });

  it('tells the owner to review the refreshed selection after a conflict', () => {
    expect(conflictText('Stale expected version')).toBe(
      'Stale expected version. Review the refreshed saved selection before saving again.'
    );
  });
});

describe('registration', () => {
  it('recognises a registry reference pinned to a sha256 digest', () => {
    expect(isImageReference(IMAGE)).toBe(true);
    expect(isImageReference(`  ${IMAGE}  `)).toBe(true);
    expect(isImageReference('ghcr.io/example/tools:latest')).toBe(false);
    expect(isImageReference(`ghcr.io/example/tools@sha256:${'a'.repeat(63)}`)).toBe(false);
    expect(isImageReference(`ghcr.io/example/tools@sha256:${'G'.repeat(64)}`)).toBe(false);
    expect(isImageReference('tools@sha256:' + 'a'.repeat(64))).toBe(false);
  });

  it('sends a trimmed name and image with the prebuilt source kind', () => {
    expect(
      registerRequest({ name: '  Public  ', image: `  ${IMAGE} `, architecture: 'arm64' })
    ).toEqual({
      name: 'Public',
      source_kind: 'prebuilt_image',
      image: IMAGE,
      architecture: 'arm64'
    });
  });

  it('allows registering only with a name, a valid image, and no pending or uncertain state', () => {
    const ok = { name: 'Public', image: IMAGE, architecture: 'amd64' as const };
    expect(canRegister(ok, false, false)).toBe(true);
    expect(canRegister({ ...ok, name: '   ' }, false, false)).toBe(false);
    expect(canRegister({ ...ok, image: 'tools:latest' }, false, false)).toBe(false);
    expect(canRegister(ok, true, false)).toBe(false);
    expect(canRegister(ok, false, true)).toBe(false);
  });

  it('treats no reply, a network error and a 5xx as uncertain, and a 4xx as a refusal', () => {
    expect(isUncertain(new TypeError('Failed to fetch'))).toBe(true);
    expect(isUncertain(new ApiError('Registry unavailable', 500))).toBe(true);
    expect(isUncertain(new ApiError('Registry unavailable', 503))).toBe(true);
    expect(isUncertain(new ApiError('Environment use grant required', 403))).toBe(false);
    expect(isUncertain(new ApiError('Image is not pinned', 422))).toBe(false);
  });

  it('reports the validation status and validator, and says runtime is untested', () => {
    expect(
      registrationNotice({
        environment: environment({ name: 'Public image' }),
        version: version({
          validation_status: 'static_validated',
          validator_version: 'oci-static-v2'
        })
      } as Parameters<typeof registrationNotice>[0])
    ).toBe(
      'Registered Public image: static_validated, oci-static-v2. Runtime capabilities have not been tested.'
    );
  });
});
