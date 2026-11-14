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
  override: string | null | undefined,
  roleDefault: string | null | undefined
): RunField {
  if (override) return { value: override, source: 'session' };
  if (roleDefault) return { value: roleDefault, source: 'role' };
  return { value: null, source: 'provider' };
}

/**
 * The source of each field. `efforts` is the provider's own levels when known: a default effort
 * the provider stopped offering is left out of the turn, so it reads as the provider's.
 */
export function runDefaults(
  session: SessionOverrides | null | undefined,
  defaults: readonly SessionDefault[] | undefined,
  role: DefaultsRole,
  efforts?: readonly string[]
): RunDefaults {
  const row = defaults?.find((d) => d.role === role && d.provider === session?.kind);
  const offered = row?.effort && (!efforts || efforts.includes(row.effort)) ? row.effort : null;
  return {
    model: field(session?.model_override, row?.model),
    effort: field(session?.effort_override, offered)
  };
}

const ROLE_WORDS: Record<DefaultsRole, string> = { main: 'main default', worker: 'worker default' };

/** The words for a source: "this session", "main default", "worker default" or "provider default". */
export function sourceWords(source: DefaultSource, role: DefaultsRole): string {
  if (source === 'session') return 'this session';
  return source === 'role' ? ROLE_WORDS[role] : 'provider default';
}

export interface RunNotes {
  /** One note for the whole chip, when the model and the effort come from the same place. */
  all?: string;
  /** A note per part, when they do not. */
  model?: string;
  effort?: string;
}

/**
 * The notes the session controls chip shows, so "opus-5-5 · high" says whose choice it is:
 * "(worker default)" when both come from one place, or one note per part when they differ. The
 * provider's own is not noted: it is what the chip means when it says nothing, and the row has no
 * room to spare on a phone.
 */
export function runNotes(run: RunDefaults, role: DefaultsRole): RunNotes {
  const note = (source: DefaultSource) =>
    source === 'provider' ? undefined : sourceWords(source, role);
  if (run.model.source === run.effort.source) {
    const all = note(run.model.source);
    return all ? { all } : {};
  }
  const model = note(run.model.source);
  const effort = note(run.effort.source);
  return { ...(model ? { model } : {}), ...(effort ? { effort } : {}) };
}

/**
 * One field of the chat composer's line. A session's own choice is shown bare, as before; a role
 * default is shown with whose it is; the provider's is "default", since nothing is sent for it.
 */
export function fieldText(run: RunField, role: DefaultsRole): string {
  if (run.source === 'session') return run.value ?? 'default';
  if (run.source === 'role') return `${run.value} (${ROLE_WORDS[role]})`;
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
