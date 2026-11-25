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
  needs: 1,
  working: 1,
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
      needs: 0,
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
    }),
    agent({
      id: 'task:c',
      space: MAINLOOP_SPACE,
      spaceLabel: 'main',
      label: 'Check the logs',
      detail: 'Fix the logs credential',
      tone: 'attention',
      target: { to: 'task', id: 'c' },
      pages: [{ to: 'task', id: 'c' }],
      ask: 'About task Check the logs (c): '
    })
  ],
  main: { needs: 2, working: 1 }
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

const agents = () => within(screen.getByRole('group', { name: 'Agents' }));
const rows = () => agents().getAllByTestId('agent-row');

describe('AgentColumn', () => {
  it('pins the main thread at the top, with what it speaks for, in no group', () => {
    renderColumn();

    const main = screen.getByTestId('main-entry');
    expect(main).toHaveAccessibleName('Mainloop, 2 need you, 1 working');
    expect(within(main).getByTestId('main-state')).toHaveTextContent('2 need you· 1 working');
    // On the main chat it is the current page
    expect(main).toHaveAttribute('aria-current', 'page');
    // It is not listed again among the agents
    expect(agents().queryByText('Mainloop')).toBeNull();
  });

  it('groups the agents by project, then No project, each heading with its counts', () => {
    renderColumn();

    expect(
      agents()
        .getAllByTestId('agent-space')
        .map((heading) => heading.textContent)
    ).toEqual(['widgets11', 'No project']);
    const widgets = agents().getByTestId('space-row');
    expect(widgets).toHaveAccessibleName('widgets, 1 needs you, 1 working');
    expect(widgets).toHaveAttribute('title', 'acme/widgets: open the project');
    expect(
      within(agents().getByRole('list', { name: /^widgets/ })).getAllByTestId('agent-row')
    ).toHaveLength(2);
    expect(
      within(agents().getByRole('list', { name: 'No project' })).getAllByTestId('agent-row')
    ).toHaveLength(1);
  });

  it('gives every row an icon, a title with its age, and one line of state', () => {
    renderColumn();

    const [running, failed, needs] = rows();
    expect(within(running!).getByTestId('agent-dot')).toHaveAttribute('data-tone', 'active');
    expect(within(running!).getByText('Add retry button')).toBeInTheDocument();
    expect(within(running!).getByTestId('agent-age')).toHaveTextContent('3h');
    expect(within(running!).getByTestId('agent-meta')).toHaveTextContent('Running');
    // A failure says so once, in its colour
    expect(within(failed!).getByTestId('agent-dot')).toHaveAttribute('data-tone', 'bad');
    expect(within(failed!).getByTestId('agent-meta')).toHaveTextContent(/^Failed$/);
    // What the owner is asked leads with Needs you
    expect(within(needs!).getByTestId('agent-meta')).toHaveTextContent(
      'Needs you · Fix the logs credential'
    );
  });

  it('marks the agent whose page is showing, and the project it belongs to', () => {
    // Given the owner is on the session of the first task
    renderColumn({ page: { to: 'session', id: 'session-a' }, space: 'project-1' });

    // Then that agent is the current page, and its project's heading is marked as where they are
    expect(screen.getByRole('button', { name: /Add retry button/, current: 'page' })).toBe(
      rows()[0]
    );
    expect(agents().getByRole('button', { current: true })).toHaveTextContent('widgets');
    expect(screen.getAllByRole('button', { current: 'page' })).toHaveLength(1);
  });

  it('marks no agent on a page that belongs to none, and the main thread as its space', () => {
    renderColumn({ page: { to: 'health' } });

    expect(screen.queryByRole('button', { current: 'page' })).toBeNull();
    expect(screen.getByTestId('main-entry')).toHaveAttribute('aria-current', 'true');
  });

  it('opens the main thread, a project or an agent on a click', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderColumn();

    await user.click(screen.getByTestId('main-entry'));
    await user.click(agents().getByTestId('space-row'));
    await user.click(rows()[1]!);

    expect(onOpen.mock.calls).toEqual([
      [{ to: 'chat' }],
      [{ to: 'project', id: 'project-1' }],
      [{ to: 'task', id: 'b' }]
    ]);
  });

  it('is one Tab stop: the arrow keys move through every row, and Enter opens', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderColumn();

    // When the owner tabs in and presses down twice
    await user.tab();
    expect(screen.getByTestId('main-entry')).toHaveFocus();
    await user.keyboard('{ArrowDown}{ArrowDown}');

    // Then focus has passed the project's heading to its first agent
    expect(rows()[0]).toHaveFocus();

    // And End, then Enter, opens the last agent
    await user.keyboard('{End}{Enter}');
    expect(onOpen).toHaveBeenCalledWith({ to: 'task', id: 'c' });

    // And the next Tab leaves the column
    await user.keyboard('{Home}');
    expect(screen.getByTestId('main-entry')).toHaveFocus();
    await user.tab();
    expect(screen.getByTestId('agent-column')).not.toContainElement(
      document.activeElement as HTMLElement
    );
  });

  it('asks Mainloop about an agent from its button and with the a key', async () => {
    const user = userEvent.setup();
    const { onAsk } = renderColumn();

    await user.click(screen.getByRole('button', { name: 'Ask Mainloop about Add retry button' }));
    rows()[1]!.focus();
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

    rows()[0]!.focus();
    await user.keyboard('{Shift>}{F10}{/Shift}');
    await user.click(await screen.findByRole('menuitem', { name: 'Ask Mainloop' }));

    expect(onAsk).toHaveBeenCalledWith('About task Add retry button (a): ');
  });

  it('has its buttons on the Agents heading: new task, and search and commands', async () => {
    const user = userEvent.setup();
    const onMenu = vi.fn();
    // Given the owner is in the widgets project
    const { onOpen, onAsk } = renderColumn({
      onMenu,
      space: 'project-1',
      page: { to: 'project', id: 'project-1' }
    });
    const row = within(screen.getByTestId('column-actions'));

    // When the owner presses each
    await user.click(row.getByRole('button', { name: 'New task in widgets' }));
    await user.click(row.getByRole('button', { name: 'Search and commands' }));

    // Then new fills the main chat for this project, and search opens the palette
    expect(onAsk).toHaveBeenCalledWith('Start a task in acme/widgets: ');
    expect(onOpen).not.toHaveBeenCalled();
    expect(onMenu).toHaveBeenCalledTimes(1);
  });

  it('calls new a word with Mainloop in the main thread’s space', () => {
    renderColumn({ onMenu: vi.fn() });

    expect(screen.getByRole('button', { name: 'New: ask Mainloop' })).toBeInTheDocument();
  });

  it('keeps the buttons while the list loads, so search works from the start', () => {
    renderColumn({ onMenu: vi.fn(), ready: false });

    expect(screen.getByRole('button', { name: 'Search and commands' })).toBeInTheDocument();
    expect(screen.queryByTestId('agent-row')).toBeNull();
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
