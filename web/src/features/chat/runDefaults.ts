/**
 * Where a session's model and effort come from, so the composer can say it. A turn uses the
 * session's own `/model` or `/effort` (or a delegation's choice), else the owner's default for the
 * session's role and provider (Settings), else the provider's own (docs/specs/chat.md, Session
 * defaults). This mirrors that order; it decides nothing.
 */
import type { components } from '@/api';
import { useSessionDefaults } from '@/features/settings/queries';
import { profileForKind, useProviderProfiles } from './providerProfiles';

type Schemas = components['schemas'];
type SessionDefault = Schemas['SessionDefault'];
export type DefaultsRole = Schemas['SessionDefaultRole'];

/** Who chose a value: this session, the owner's default for its role, or the provider. */
export type DefaultSource = 'session' | 'role' | 'provider';

export interface RunField {
  /** The value that source names, or null when it is the provider's (which Mainloop does not send). */
  value: string | null;
  source: DefaultSource;
  /** Only for a role default that is another role's: a supervisor with none of its own uses the worker's. */
  from?: DefaultsRole;
}

export interface RunDefaults {
  model: RunField;
  effort: RunField;
}

/** What a session's overrides are, as the native session reports them. */
export interface SessionOverrides {
  kind?: string | null;
  model_override?: string | null;
  effort_override?: string | null;
}

function field(
  role: DefaultsRole,
  override: string | null | undefined,
  roleDefaults: readonly (readonly [DefaultsRole, string | null | undefined])[]
): RunField {
  if (override) return { value: override, source: 'session' };
  for (const [from, value] of roleDefaults) {
    if (value) return from === role ? { value, source: 'role' } : { value, source: 'role', from };
  }
  return { value: null, source: 'provider' };
}

/**
 * The source of each field. `efforts` is the provider's own levels when known: an effort the
 * provider stopped offering is left out of the turn, so a session's own falls back to the role
 * default and a role default reads as the provider's.
 */
export function runDefaults(
  session: SessionOverrides | null | undefined,
  defaults: readonly SessionDefault[] | undefined,
  role: DefaultsRole,
  efforts?: readonly string[]
): RunDefaults {
  const rowFor = (r: DefaultsRole) =>
    defaults?.find((d) => d.role === r && d.provider === session?.kind);
  // A supervisor with no supervisor default for a field uses the worker default for it.
  const roles: DefaultsRole[] = role === 'supervisor' ? ['supervisor', 'worker'] : [role];
  const offered = (effort: string | null | undefined) =>
    effort && (!efforts || efforts.includes(effort)) ? effort : null;
  return {
    model: field(
      role,
      session?.model_override,
      roles.map((r) => [r, rowFor(r)?.model] as const)
    ),
    effort: field(
      role,
      offered(session?.effort_override),
      roles.map((r) => [r, offered(rowFor(r)?.effort)] as const)
    )
  };
}

const ROLE_WORDS: Record<DefaultsRole, string> = {
  main: 'main default',
  supervisor: 'supervisor default',
  worker: 'worker default'
};

/** The words for a source: "this session", "main default", "worker default" or "provider default". */
export function sourceWords(source: DefaultSource, role: DefaultsRole): string {
  if (source === 'session') return 'this session';
  return source === 'role' ? ROLE_WORDS[role] : 'provider default';
}

/** The words for a field's source, naming the role the default came from. */
function fieldWords(run: RunField, role: DefaultsRole): string {
  return sourceWords(run.source, run.from ?? role);
}

export interface RunNotes {
  /** One note for the whole chip, when the model and the effort come from the same place. */
  all?: string;
  /** A note per part, when they do not. */
  model?: string;
  effort?: string;
  /** The recorded values for the chip to fall back on; see `runRecorded`. */
  recorded?: { model?: string; effort?: string };
}

/**
 * The notes the session controls chip shows, so "opus-5-5 · high" says whose choice it is:
 * "(worker default)" when both come from one place, or one note per part when they differ. The
 * provider's own is not noted: it is what the chip means when it says nothing, and the row has no
 * room to spare on a phone.
 */
export function runNotes(run: RunDefaults, role: DefaultsRole): RunNotes {
  const note = (f: RunField) => (f.source === 'provider' ? undefined : fieldWords(f, role));
  const model = note(run.model);
  const effort = note(run.effort);
  if (run.model.source === run.effort.source && model === effort) {
    return model ? { all: model } : {};
  }
  return { ...(model ? { model } : {}), ...(effort ? { effort } : {}) };
}

/**
 * The model and effort Mainloop has on record for the session: its own choice, else the owner's
 * default. The session controls chip shows them while the runner has not said what it runs (a
 * session that is asleep or has ended). The provider's own is not known here, so it is left out.
 */
export function runRecorded(run: RunDefaults): { model?: string; effort?: string } {
  return {
    ...(run.model.value ? { model: run.model.value } : {}),
    ...(run.effort.value ? { effort: run.effort.value } : {})
  };
}

/**
 * One field of the chat composer's line. A session's own choice is shown bare, as before; a role
 * default is shown with whose it is; the provider's is "default", since nothing is sent for it.
 */
export function fieldText(run: RunField, role: DefaultsRole): string {
  if (run.source === 'session') return run.value ?? 'default';
  if (run.source === 'role') return `${run.value} (${fieldWords(run, role)})`;
  return 'default';
}

/**
 * The sources for one session, or null until the owner's defaults have been read: the composer
 * then says nothing about them, rather than calling a role default the provider's.
 */
export function useRunDefaults(
  role: DefaultsRole,
  session: SessionOverrides | null | undefined
): RunDefaults | null {
  const defaults = useSessionDefaults();
  const profiles = useProviderProfiles();
  if (!defaults.data || !session) return null;
  const profile = profileForKind(profiles.data, session.kind ?? undefined);
  return runDefaults(session, defaults.data.defaults, role, profile?.effort_levels);
}
