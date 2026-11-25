import { AgentColumn } from './AgentColumn';
import { MAINLOOP_SPACE, type AgentColumnModel, type AgentRow, type SpaceRow } from './agents';

const NOW = Date.parse('2026-01-01T12:00:00Z');
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();

const space = (row: Partial<SpaceRow> & Pick<SpaceRow, 'id' | 'label'>): SpaceRow => ({
  detail: 'idle',
  tone: 'muted',
  needs: 0,
  working: 0,
  target: { to: 'project', id: row.id },
  ...row
});

const agent = (row: Partial<AgentRow> & Pick<AgentRow, 'id' | 'label'>): AgentRow => ({
  space: 'widgets',
  spaceLabel: 'widgets',
  role: 'worker',
  standing: false,
  tone: 'active',
  problem: false,
  target: { to: 'task', id: row.id },
  pages: [{ to: 'task', id: row.id }],
  ask: `About task ${row.label}: `,
  depth: 0,
  ...row
});

/** Fictional work across two projects and the main thread's own. */
const MODEL: AgentColumnModel = {
  spaces: [
    space({ id: MAINLOOP_SPACE, label: 'main', target: { to: 'chat' } }),
    space({ id: 'widgets', label: 'widgets', title: 'acme/widgets', needs: 2, working: 2 }),
    space({ id: 'infra', label: 'infra', title: 'acme/infra', working: 1 }),
    space({ id: 'docs', label: 'a-project-with-a-rather-long-name', title: 'acme/docs' })
  ],
  agents: [
    agent({
      id: 'main',
      space: MAINLOOP_SPACE,
      spaceLabel: 'main',
      label: 'Mainloop',
      role: 'main',
      tone: 'attention',
      detail: 'Needs you',
      target: { to: 'chat' },
      pages: [{ to: 'chat' }],
      ask: undefined
    }),
    agent({ id: 'a', label: 'Pick a queue backend', detail: 'Running tests', at: ago(2) }),
    agent({
      id: 'b',
      label: 'Pick a region for the cache',
      detail: 'Choose eu or us',
      tone: 'attention',
      at: ago(4)
    }),
    agent({
      id: 'c',
      label: 'Fix the flaky upload test',
      detail: 'Failed',
      tone: 'bad',
      at: ago(18)
    }),
    agent({
      id: 'd',
      label: 'Add a retry button to the task page and the session page',
      detail: 'PR #41 · CI passing',
      tone: 'unseen',
      at: ago(60)
    }),
    agent({
      id: 'e',
      space: 'infra',
      spaceLabel: 'infra',
      label: 'Supervisor',
      role: 'supervisor',
      standing: true,
      tone: 'muted',
      detail: '1/2 done · 1 running',
      at: ago(2880)
    }),
    agent({
      id: 'f',
      space: 'infra',
      spaceLabel: 'infra',
      label: 'Rotate the registry token',
      detail: 'Waiting for the rollout',
      at: ago(5),
      depth: 1
    }),
    agent({
      id: 'g',
      space: MAINLOOP_SPACE,
      spaceLabel: 'main',
      label: 'Check the queue worker logs',
      tone: 'attention',
      detail: 'Fix the credential for the logs endpoint in ops/logging.yaml',
      at: ago(60)
    }),
    agent({
      id: 's',
      space: MAINLOOP_SPACE,
      spaceLabel: 'main',
      label: 'Scratch session',
      role: 'session',
      tone: 'muted',
      detail: 'idle',
      target: { to: 'session', id: 's' },
      pages: [{ to: 'session', id: 's' }]
    })
  ],
  main: { needs: 3, working: 3 }
};

function Column({
  page,
  space
}: {
  page: Parameters<typeof AgentColumn>[0]['page'];
  space: string;
}) {
  return (
    <div className="bg-background border-border-subtle flex h-[720px] max-w-80 flex-col border">
      <AgentColumn
        model={MODEL}
        page={page}
        space={space}
        ready
        failed={false}
        onOpen={() => {}}
        onAsk={() => {}}
        onMenu={() => {}}
        now={NOW}
      />
    </div>
  );
}

/** On the main chat: the pinned main thread is the current row. */
export default function AgentColumnPreview() {
  return <Column page={{ to: 'chat' }} space={MAINLOOP_SPACE} />;
}

/** On a task's page: its row is current, and its project's heading is marked. */
export function AgentColumnTaskPreview() {
  return <Column page={{ to: 'task', id: 'b' }} space="widgets" />;
}
