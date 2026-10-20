import type { NativeSessionInfo, ProviderProfile } from './types';

const MODEL_PATTERN = /^[A-Za-z0-9._:/-]{1,200}$/;

function modelLabel(id: string, profile: ProviderProfile | undefined): string {
  return profile?.models.find((model) => model.id === id)?.label ?? id;
}

function availableLine(profile: ProviderProfile | undefined): string {
  if (!profile || profile.models.length === 0) {
    return 'This provider lists no models; set one by name, or /model default.';
  }
  const names = profile.models.map((model) => model.id).join(', ');
  return `Available: ${names}, or default. Usage: /model <name>.`;
}

/** `/model`'s help text for a bare `/model`: the session's current model and the choices. */
export function describeModel(
  native: NativeSessionInfo | null | undefined,
  profile: ProviderProfile | undefined
): string {
  const current = native?.model_override
    ? modelLabel(native.model_override, profile)
    : `default${native?.model ? ` (currently ${native.model})` : ''}`;
  return [`Current model: ${current}.`, availableLine(profile)].join('\n');
}

export type ModelCommandOutcome = { notice: string } | { model: string | null };

/**
 * `args[0]` names a model, matched against the session's provider profile when it is one of the
 * profile's listed models, or taken as written otherwise: kagent is the real gate on whether a
 * model exists, and rejects an unknown one when the next turn starts. `/model default` clears the
 * override. A name is checked against the wire's bounded pattern before it is ever sent.
 */
export function resolveModelCommand(
  args: string[],
  native: NativeSessionInfo | null | undefined,
  profile: ProviderProfile | undefined
): ModelCommandOutcome {
  if (args.length === 0) return { notice: describeModel(native, profile) };
  if (args.length > 1)
    return { notice: `Usage: /model <name>, or /model default.\n${availableLine(profile)}` };
  const requested = args[0] ?? '';
  if (requested.toLowerCase() === 'default') {
    if (!native?.model_override) return { notice: 'Already on the default model.' };
    return { model: null };
  }
  const match = profile?.models.find((model) => model.id.toLowerCase() === requested.toLowerCase());
  const modelId = match?.id ?? requested;
  if (!MODEL_PATTERN.test(modelId)) {
    return { notice: `"${requested}" is not a valid model name.\n${availableLine(profile)}` };
  }
  if (native?.model_override === modelId)
    return { notice: `Already on ${modelLabel(modelId, profile)}.` };
  return { model: modelId };
}
