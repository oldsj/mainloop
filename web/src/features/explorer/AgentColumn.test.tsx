import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { AgentColumn, type AgentColumnProps } from './AgentColumn';
import { MAINLOOP_SPACE, type AgentColumnModel, type AgentRow, type SpaceRow } from './agents';

const NOW = Date.parse('2026-01-01T12:00:00Z');

const space = (overrides: Partial<SpaceRow> = {}): SpaceRow => ({
  id: 'project-1',
  label: 'widgets',
  title: 'acme/widgets',
  detail: '1 working',
  tone: 'active',
  target: { to: 'project', id: 'project-1' },
  ...overrides
});

const agent = (overrides: Partial<AgentRow> = {}): AgentRow => ({
  id: 'task:a',
  space: 'project-1',
  spaceLabel: 'widgets',
  label: 'Add retry button',
  role: 'worker',
  standing: false,
  detail: 'Running',
  at: '2026-01-01T09:00:00Z',
  tone: 'active',
  problem: false,
  target: { to: 'task', id: 'a' },
  pages: [
    { to: 'task', id: 'a' },
    { to: 'session', id: 'session-a' }
  ],
  ask: 'About task Add retry button (a): ',
  depth: 0,
  ...overrides
});

const MAIN = agent({
  id: 'main',
  space: MAINLOOP_SPACE,
  spaceLabel: 'main',
  label: 'Mainloop',
  role: 'main',
  detail: 'Working',
  at: undefined,
  target: { to: 'chat' },
  pages: [{ to: 'chat' }],
  ask: undefined
});

const MODEL: AgentColumnModel = {
  spaces: [
    space({
      id: MAINLOOP_SPACE,
      label: 'main',
      title: undefined,
      detail: 'working',
      target: { to: 'chat' }
    }),
    space()
  ],
  agents: [
    MAIN,
    agent(),
    agent({
      id: 'task:b',
      label: 'Fix CI flake',
      detail: 'Failed',
      tone: 'bad',
      depth: 1,
      target: { to: 'task', id: 'b' },
      pages: [{ to: 'task', id: 'b' }],
      ask: 'About task Fix CI flake (b): '
    })
  ]
};

function renderColumn(overrides: Partial<AgentColumnProps> = {}) {
  const onOpen = vi.fn();
  const onAsk = vi.fn();
  render(
    <AgentColumn
      model={MODEL}
      page={{ to: 'chat' }}
      space={MAINLOOP_SPACE}
      ready
      failed={false}
      onOpen={onOpen}
      onAsk={onAsk}
      now={NOW}
      {...overrides}
    />
  );
  return { onOpen, onAsk };
}

const spaces = () => within(screen.getByRole('list', { name: 'spaces' }));
const agents = () => within(screen.getByRole('group', { name: 'agents' }));

describe('AgentColumn', () => {
  it('shows the spaces, then the agents, each with a dot and one muted line', () => {
    // Given two spaces and three agents
    renderColumn();

    // Then the spaces list names each with what its agents are doing
    const spaceRows = spaces().getAllByTestId('space-row');
    expect(spaceRows.map((row) => row.textContent)).toEqual(['mainworking', 'widgets1 working']);
    expect(spaceRows[1]).toHaveAttribute('title', 'acme/widgets');

    // And the agents are listed under their space's name, each its title, then its state and age
    expect(
      agents()
        .getAllByTestId('agent-space')
        .map((label) => label.textContent)
    ).toEqual(['main', 'widgets']);
    const widgets = within(agents().getByRole('list', { name: 'widgets' }));
    expect(widgets.getAllByTestId('agent-row')).toHaveLength(2);
    const agentRows = agents().getAllByTestId('agent-row');
    expect(agentRows).toHaveLength(3);
    expect(within(agentRows[1]!).getByText('Add retry button')).toBeInTheDocument();
    expect(within(agentRows[1]!).getByTestId('agent-meta')).toHaveTextContent('Running · 3h');
    expect(within(agentRows[2]!).getByTestId('agent-dot')).toHaveAttribute('data-tone', 'bad');
  });

  it('marks the agent whose page is showing, and the space it belongs to', () => {
    // Given the owner is on the session of the first task
    renderColumn({ page: { to: 'session', id: 'session-a' }, space: 'project-1' });

    // Then that agent is the current page, and its space is marked as where the owner is
    expect(screen.getByRole('button', { name: /Add retry button/, current: 'page' })).toBe(
      agents().getAllByTestId('agent-row')[1]
    );
    expect(spaces().getByRole('button', { current: true })).toHaveTextContent('widgets');
    expect(screen.getAllByRole('button', { current: 'page' })).toHaveLength(1);
  });

  it('marks no agent on a page that belongs to none', () => {
    renderColumn({ page: { to: 'health' } });

    expect(screen.queryByRole('button', { current: 'page' })).toBeNull();
    expect(spaces().getByRole('button', { current: true })).toHaveTextContent('main');
  });

  it('opens a space or an agent on a click', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderColumn();

    await user.click(spaces().getAllByTestId('space-row')[1]!);
    await user.click(agents().getAllByTestId('agent-row')[2]!);

    expect(onOpen.mock.calls).toEqual([
      [{ to: 'project', id: 'project-1' }],
      [{ to: 'task', id: 'b' }]
    ]);
  });

  it('is one Tab stop: the arrow keys move through both lists, and Enter opens', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderColumn();

    // When the owner tabs in and presses down twice
    await user.tab();
    expect(spaces().getAllByTestId('space-row')[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}{ArrowDown}');

    // Then focus has left the spaces for the first agent
    expect(agents().getAllByTestId('agent-row')[0]).toHaveFocus();

    // And End, then Enter, opens the last agent
    await user.keyboard('{End}{Enter}');
    expect(onOpen).toHaveBeenCalledWith({ to: 'task', id: 'b' });

    // And the next Tab leaves the column
    await user.keyboard('{Home}');
    expect(spaces().getAllByTestId('space-row')[0]).toHaveFocus();
    await user.tab();
    expect(screen.getByTestId('agent-column')).not.toContainElement(
      document.activeElement as HTMLElement
    );
  });

  it('asks Mainloop about an agent from its button and with the a key', async () => {
    const user = userEvent.setup();
    const { onAsk } = renderColumn();

    await user.click(screen.getByRole('button', { name: 'Ask Mainloop about Add retry button' }));
    agents().getAllByTestId('agent-row')[2]!.focus();
    await user.keyboard('a');

    expect(onAsk.mock.calls).toEqual([
      ['About task Add retry button (a): '],
      ['About task Fix CI flake (b): ']
    ]);
    // The main thread has nothing to ask itself about
    expect(screen.queryByRole('button', { name: 'Ask Mainloop about Mainloop' })).toBeNull();
  });

  it('offers a row’s actions in a menu from the keyboard', async () => {
    const user = userEvent.setup();
    const { onAsk } = renderColumn();

    agents().getAllByTestId('agent-row')[1]!.focus();
    await user.keyboard('{Shift>}{F10}{/Shift}');
    await user.click(await screen.findByRole('menuitem', { name: 'Ask Mainloop' }));

    expect(onAsk).toHaveBeenCalledWith('About task Add retry button (a): ');
  });

  it('holds its place with placeholder rows while the first read is out', () => {
    renderColumn({ ready: false });

    expect(screen.getByRole('status')).toHaveTextContent('Loading Mainloop…');
    expect(screen.getByTestId('explorer-loading')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-row')).toBeNull();
  });

  it('says so when the agents could not be read', () => {
    renderColumn({ failed: true });

    expect(screen.getByRole('alert')).toHaveTextContent('Couldn’t load the agents.');
  });
});
