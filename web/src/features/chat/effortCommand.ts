import type { EffortLevel, NativeSessionInfo, ProviderProfile } from './types';

function availableLine(profile: ProviderProfile | undefined): string {
  if (!profile || profile.effort_levels.length === 0) {
    return 'This provider offers no reasoning-effort levels.';
  }
  return `Available: ${profile.effort_levels.join(', ')}, or default. Usage: /effort <level>.`;
}

/** `/effort`'s help text for a bare `/effort`: the session's current level and the choices. */
export function describeEffort(
  native: NativeSessionInfo | null | undefined,
  profile: ProviderProfile | undefined
): string {
  const current = native?.effort_override ?? 'default';
  return [`Current effort: ${current}.`, availableLine(profile)].join('\n');
}

export type EffortCommandOutcome = { notice: string } | { effort: EffortLevel | null };

/**
 * `args[0]` names a reasoning-effort level. It must be one the session's current provider profile
 * offers (`effort_levels`); any other word is reported, not sent — the server enforces the same
 * rule, but the composer need not round-trip to say so. `/effort default` clears the override.
 */
export function resolveEffortCommand(
  args: string[],
  native: NativeSessionInfo | null | undefined,
  profile: ProviderProfile | undefined
): EffortCommandOutcome {
  if (args.length === 0) return { notice: describeEffort(native, profile) };
  if (args.length > 1)
    return { notice: `Usage: /effort <level>, or /effort default.\n${availableLine(profile)}` };
  const requested = (args[0] ?? '').toLowerCase();
  if (requested === 'default') {
    if (!native?.effort_override) return { notice: 'Already on the default effort.' };
    return { effort: null };
  }
  const level = profile?.effort_levels.find((candidate) => candidate === requested);
  if (!level) return { notice: `Unknown effort "${requested}".\n${availableLine(profile)}` };
  if (native?.effort_override === level) return { notice: `Already on ${level}.` };
  return { effort: level };
}
