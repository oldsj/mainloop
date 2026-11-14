/**
 * The rules behind the session defaults form: which rows it shows, what a row currently holds, and
 * what "leave it to the provider" is called. Pure, so the rules test without a QueryClient.
 */
import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];
export type SessionDefault = Schemas['SessionDefault'];
export type SessionDefaultRole = Schemas['SessionDefaultRole'];
export type ProviderProfile = Schemas['ProviderProfile'];

/** The roles, in the order the form lists them, with what each covers. */
export const ROLES: readonly { id: SessionDefaultRole; label: string; covers: string }[] = [
  { id: 'main', label: 'Main thread', covers: 'The Mainloop chat.' },
  { id: 'worker', label: 'Workers', covers: 'Every task session, supervisors included.' }
];

/** The default the owner set for one role and provider, or one with both fields left to the provider. */
export function defaultFor(
  defaults: readonly SessionDefault[] | undefined,
  role: SessionDefaultRole,
  provider: string
): SessionDefault {
  return (
    defaults?.find((row) => row.role === role && row.provider === provider) ?? {
      role,
      provider,
      model: null,
      effort: null
    }
  );
}

/** Profiles a default can be set for: the enabled ones, in the registry's order. */
export function selectableProfiles(profiles: readonly ProviderProfile[] | undefined) {
  return (profiles ?? []).filter((profile) => profile.enabled);
}

/**
 * What the "leave it to the provider" choice says. No value is sent for it, so the turn runs on the
 * provider's own model or effort; the profile's default is named as a hint, since it is only what
 * the profile lists and is never sent.
 */
export function providerDefaultLabel(hint: string | null | undefined): string {
  return hint ? `Provider default (${hint})` : 'Provider default';
}

/** A model's label in the profile, or its id when the profile does not list it. */
export function modelLabel(profile: ProviderProfile, id: string | null): string | null {
  if (id === null) return null;
  return profile.models.find((model) => model.id === id)?.label ?? id;
}

/**
 * The model choices for a row: the profile's models, plus the saved one when the profile does not
 * list it (the API accepts any model name), so a saved value is never dropped from its own select.
 */
export function modelChoices(profile: ProviderProfile, saved: string | null) {
  const listed = profile.models.map((model) => ({ id: model.id, label: model.label }));
  return saved !== null && !listed.some((model) => model.id === saved)
    ? [...listed, { id: saved, label: `${saved} (not in this profile's list)` }]
    : listed;
}

/**
 * The effort choices for a row: the profile's levels, plus the saved one when the profile stopped
 * offering it. Such a default is left out of a turn, and the form says so.
 */
export function effortChoices(profile: ProviderProfile, saved: string | null) {
  const listed = profile.effort_levels.map((level) => ({
    id: level as string,
    label: level as string
  }));
  return saved !== null && !listed.some((level) => level.id === saved)
    ? [...listed, { id: saved, label: `${saved} (no longer offered)` }]
    : listed;
}

/** Whether the saved effort is one the profile no longer offers. */
export function effortStale(profile: ProviderProfile, saved: string | null): boolean {
  return saved !== null && !profile.effort_levels.some((level) => level === saved);
}
