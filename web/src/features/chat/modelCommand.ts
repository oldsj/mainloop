import { MAIN_PROVIDERS, providerLabel, providerReady, selectionIsNoop } from './mainProvider';
import type { MainProvider, MainThreadInfo } from './types';

/**
 * Mainloop has no model registry and no reasoning-effort field anywhere in its contract or in
 * kagent's session API. The only axis it can actually switch at runtime is the main session's
 * provider (Claude Code or Codex) — the closest real analogue to "model" this repo controls. The
 * LLM a provider's native session actually runs is fixed by kagent's Agent/AgentTemplate/
 * ModelConfig chain, set by operators outside this repo, which Mainloop neither stores nor
 * injects. So `/model` exposes the provider switch and refuses a reasoning-effort argument rather
 * than silently accepting and discarding it.
 */
export const EFFORT_UNSUPPORTED_NOTICE =
  'Reasoning effort is not supported: Mainloop has no reasoning-effort parameter in its session ' +
  "contract, and kagent's session API has none to carry it either.";

function providerStatusLine(info: MainThreadInfo | null | undefined): string {
  const current = info?.provider;
  if (!current) return 'Current model: unknown (main thread not read yet).';
  const label = providerLabel(current.desired_provider);
  return providerReady(current)
    ? `Current model: ${label} (ready).`
    : `Current model: ${label} (${current.status}).`;
}

function availableLine(): string {
  const names = MAIN_PROVIDERS.map((option) => option.value).join(', ');
  return `Available: ${names}. Usage: /model <name>.`;
}

/** `/model`'s help text for a bare `/model`: the current provider, the choices, and the effort gap. */
export function describeModel(info: MainThreadInfo | null | undefined): string {
  return [providerStatusLine(info), availableLine(), EFFORT_UNSUPPORTED_NOTICE].join('\n');
}

export type ModelCommandOutcome = { notice: string } | { provider: MainProvider };

/**
 * `args[0]` names a provider; a second argument would be a reasoning effort, which is refused
 * outright. An unknown or already-selected provider is reported, not switched.
 */
export function resolveModelCommand(
  args: string[],
  info: MainThreadInfo | null | undefined
): ModelCommandOutcome {
  if (args.length === 0) return { notice: describeModel(info) };
  if (args.length > 1) return { notice: EFFORT_UNSUPPORTED_NOTICE };
  const requested = args[0] ?? '';
  const match = MAIN_PROVIDERS.find((option) => option.value === requested.toLowerCase());
  if (!match) return { notice: `Unknown model "${requested}". ${availableLine()}` };
  if (selectionIsNoop(match.value, info?.provider)) {
    return { notice: `Already on ${match.label}.` };
  }
  return { provider: match.value };
}
