/** Use native identity, never infer the runtime from a model name. */
export function agentLabel(kind?: 'claude' | 'codex' | null): string {
  return kind ?? 'agent';
}
