import type { ProgressFrame } from '@/api';
import { LiveTurn } from './LiveTurn';

const base: ProgressFrame = {
  session_id: 'sess-main',
  message_id: 'm1',
  seq: 1,
  active: true,
  started_at: new Date(Date.now() - 42_000).toISOString().replace('Z', ''),
  status: { kind: 'thinking', label: 'Thinking…' },
  text: '',
  text_truncated: false,
  tools: []
};

/** A turn that has only just started, one writing with tool calls, and one waiting for the owner. */
export default function LiveTurnPreview() {
  return (
    <div className="gap-sm flex flex-col">
      <LiveTurn frame={base} agentKind="claude" />
      <LiveTurn
        agentKind="claude"
        frame={{
          ...base,
          status: { kind: 'tool', label: 'Running Bash: go test -race -count=1 ./...' },
          text: 'The tests need the **scratch database**. Running them now:\n\n```sh\ngo test ./...\n```',
          tools: [
            {
              id: 'a',
              name: 'Read',
              label: 'Reading server/internal/progress/progress.go',
              state: 'done'
            },
            { id: 'b', name: 'Bash', label: 'Running Bash: go vet ./...', state: 'error' },
            {
              id: 'c',
              name: 'Bash',
              label: 'Running Bash: go test -race -count=1 ./...',
              state: 'running'
            }
          ]
        }}
      />
      <LiveTurn
        agentKind="codex"
        frame={{
          ...base,
          status: { kind: 'waiting', label: 'Waiting for you' },
          text: 'Shall I delete the old table?'
        }}
      />
    </div>
  );
}
