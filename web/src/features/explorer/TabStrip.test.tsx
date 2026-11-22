import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { MAINLOOP_SPACE, type AgentColumnModel, type AgentRow } from './agents';
import { TabStrip, type TabStripProps } from './TabStrip';

const agent = (overrides: Partial<AgentRow> = {}): AgentRow => ({
  id: 'task:a',
  space: 'project-1',
  spaceLabel: 'widgets',
  label: 'Add retry button',
  role: 'worker',
  standing: false,
  detail: 'Running',
  tone: 'active',
  problem: false,
  target: { to: 'task', id: 'a' },
  pages: [
    { to: 'task', id: 'a' },
    { to: 'session', id: 'session-a' }
  ],
  depth: 0,
  ...overrides
});

const MODEL: AgentColumnModel = {
  spaces: [
    { id: MAINLOOP_SPACE, label: 'main', detail: 'idle', tone: 'muted', target: { to: 'chat' } },
    {
      id: 'project-1',
      label: 'widgets',
      title: 'acme/widgets',
      detail: '1 working',
      tone: 'active',
      target: { to: 'project', id: 'project-1' }
    }
  ],
  agents: [
    agent({
      id: 'main',
      space: MAINLOOP_SPACE,
      spaceLabel: 'main',
      label: 'Mainloop',
      role: 'main',
      tone: 'muted',
      target: { to: 'chat' },
      pages: [{ to: 'chat' }]
    }),
    agent(),
    agent({
      id: 'task:b',
      label: 'Fix CI flake',
      tone: 'bad',
      target: { to: 'task', id: 'b' },
      pages: [{ to: 'task', id: 'b' }]
    })
  ]
};

function renderStrip(overrides: Partial<TabStripProps> = {}) {
  const onOpen = vi.fn();
  const onAsk = vi.fn();
  render(
    <TabStrip
      model={MODEL}
      space="project-1"
      page={{ to: 'task', id: 'a' }}
      pathname="/tasks/a"
      onOpen={onOpen}
      onAsk={onAsk}
      {...overrides}
    />
  );
  return { onOpen, onAsk };
}

describe('TabStrip', () => {
  it('shows the current space’s agents as tabs, the one being shown lit', () => {
    // Given the owner is on a task of the widgets project
    renderStrip();

    // Then the strip is named for the space, and its tabs are the overview and the agents
    const strip = screen.getByRole('navigation', { name: 'Agents in widgets' });
    const tabs = within(strip).getAllByTestId('tab');
    expect(tabs.map((tab) => tab.textContent)).toEqual([
      'overview',
      'Add retry button',
      'Fix CI flake'
    ]);
    expect(within(strip).getByRole('button', { current: 'page' })).toBe(tabs[1]);
    // And an agent's tab carries its dot; the overview is a page, so it has none
    expect(within(tabs[2]!).getByTestId('tab-dot')).toHaveAttribute('data-tone', 'bad');
    expect(within(tabs[0]!).queryByTestId('tab-dot')).toBeNull();
    expect(tabs[1]).toHaveAttribute('title', 'Add retry button');
  });

  it('leads with the standing supervisor in a project that has one, and has no overview tab', () => {
    // Given the widgets project has a standing supervisor, at rest
    const supervised: AgentColumnModel = {
      ...MODEL,
      agents: [
        MODEL.agents[0]!,
        agent({
          id: 'task:sup',
          label: 'supervisor',
          role: 'supervisor',
          standing: true,
          tone: 'muted',
          target: { to: 'session', id: 'session-sup' },
          pages: [
            { to: 'task', id: 'sup' },
            { to: 'session', id: 'session-sup' }
          ]
        }),
        ...MODEL.agents.slice(1)
      ]
    };
    renderStrip({ model: supervised, page: { to: 'session', id: 'session-sup' } });

    const tabs = screen.getAllByTestId('tab');
    expect(tabs.map((tab) => tab.textContent)).toEqual([
      'supervisor',
      'Add retry button',
      'Fix CI flake'
    ]);
    expect(tabs[0]).toHaveAttribute('aria-current', 'page');
    expect(within(tabs[0]!).getByTestId('tab-dot')).toHaveAttribute('data-tone', 'muted');
  });

  it('opens a tab’s page on a click', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderStrip();

    await user.click(screen.getByRole('button', { name: 'overview' }));
    await user.click(screen.getByRole('button', { name: 'Fix CI flake' }));

    expect(onOpen.mock.calls).toEqual([
      [{ to: 'project', id: 'project-1' }],
      [{ to: 'task', id: 'b' }]
    ]);
  });

  it('starts new work in the space from the chat, unsent', async () => {
    const user = userEvent.setup();
    const { onAsk, onOpen } = renderStrip();

    await user.click(screen.getByRole('button', { name: 'New task in widgets' }));

    expect(onAsk).toHaveBeenCalledWith('Start a task in acme/widgets: ');
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('is the main chat’s strip on a page that belongs to no project, with that page as a tab', async () => {
    const user = userEvent.setup();
    const { onOpen } = renderStrip({ space: MAINLOOP_SPACE, page: null, pathname: '/settings' });

    const strip = screen.getByRole('navigation', { name: 'Agents in main' });
    expect(
      within(strip)
        .getAllByTestId('tab')
        .map((tab) => tab.textContent)
    ).toEqual(['main', 'settings']);
    expect(within(strip).getByRole('button', { current: 'page' })).toHaveTextContent('settings');

    // The "+" there is the main chat itself: Mainloop is who starts work.
    await user.click(screen.getByRole('button', { name: 'New: ask Mainloop' }));
    expect(onOpen).toHaveBeenCalledWith({ to: 'chat' });
  });
});
