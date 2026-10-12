import type { AgentKind } from './model';

const AGENTS: { kind: AgentKind; label: string }[] = [
  { kind: 'claude', label: 'Claude Code' },
  { kind: 'codex', label: 'Codex' }
];

/**
 * The agent choice for a new workspace. `prefix` names the radio group and sets the test ids, so
 * the panel and the project page each keep their own frozen ids (`new-workspace-kind-*`,
 * `project-workspace-kind-*`).
 */
export function AgentRadios({
  prefix,
  value,
  onChange
}: {
  prefix: 'new-workspace-kind' | 'project-workspace-kind';
  value: AgentKind;
  onChange: (kind: AgentKind) => void;
}) {
  return (
    <fieldset className="gap-sm flex flex-wrap">
      <legend className="text-sm font-medium">Agent</legend>
      {AGENTS.map(({ kind, label }) => (
        <label
          key={kind}
          className="gap-xs px-md border-border has-checked:bg-accent has-checked:text-accent-foreground flex min-h-11 cursor-pointer items-center border text-sm"
        >
          <input
            type="radio"
            name={prefix}
            value={kind}
            data-testid={`${prefix}-${kind}`}
            checked={value === kind}
            onChange={() => onChange(kind)}
            className="size-4"
          />
          {label}
        </label>
      ))}
    </fieldset>
  );
}
