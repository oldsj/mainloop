import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ExplorerTree } from './ExplorerTree';
import type { ExplorerNode } from './model';

const nodes: ExplorerNode[] = [
  {
    id: 'mainloop',
    label: 'Mainloop',
    detail: 'Idle',
    tone: 'muted',
    target: { to: 'chat' },
    children: []
  },
  {
    id: 'needs',
    label: 'Needs you',
    count: 1,
    tone: 'attention',
    children: [
      {
        id: 'needs/task:t1',
        label: 'Fix login',
        tone: 'attention',
        target: { to: 'task', id: 't1' },
        ask: 'About task Fix login (t1): ',
        children: []
      }
    ]
  },
  {
    id: 'working',
    label: 'Working',
    count: 0,
    children: [{ id: 'working/none', label: 'Nothing running', note: true, children: [] }]
  }
];

function renderTree(expanded: string[] = ['needs']) {
  const onToggle = vi.fn();
  const onOpen = vi.fn();
  const onAsk = vi.fn();
  render(
    <ExplorerTree
      nodes={nodes}
      isExpanded={(id) => expanded.includes(id)}
      onToggle={onToggle}
      onOpen={onOpen}
      onAsk={onAsk}
    />
  );
  return { onToggle, onOpen, onAsk, user: userEvent.setup() };
}

describe('explorer tree roles', () => {
  it('exposes a tree of treeitems with level, position and expanded state', () => {
    renderTree();
    const tree = screen.getByRole('tree', { name: 'Mainloop explorer' });
    const [mainloop, needs] = within(tree)
      .getAllByRole('treeitem')
      .filter((item) => item.getAttribute('aria-level') === '1');
    expect(mainloop).toHaveAttribute('aria-posinset', '1');
    expect(mainloop).toHaveAttribute('aria-setsize', '3');
    expect(mainloop).not.toHaveAttribute('aria-expanded');
    expect(needs).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('treeitem', { name: 'Needs you, 1' })).toBeInTheDocument();
    expect(screen.getByRole('treeitem', { name: 'Fix login' })).toHaveAttribute('aria-level', '2');
  });

  it('shows the folded group as collapsed and leaves its children out', () => {
    renderTree([]);
    expect(screen.getByRole('treeitem', { name: 'Needs you, 1' })).toHaveAttribute(
      'aria-expanded',
      'false'
    );
    expect(screen.queryByRole('treeitem', { name: 'Fix login' })).not.toBeInTheDocument();
  });
});

describe('explorer tree keyboard', () => {
  it('moves down and up with the arrow keys, and stops at either end', async () => {
    const { user } = renderTree();
    const first = screen.getByRole('treeitem', { name: /^Mainloop/ });
    first.focus();
    await user.keyboard('{ArrowUp}');
    expect(first).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('treeitem', { name: 'Needs you, 1' })).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('treeitem', { name: 'Fix login' })).toHaveFocus();
  });

  it('expands a folded group on right', async () => {
    const { user, onToggle } = renderTree([]);
    screen.getByRole('treeitem', { name: 'Needs you, 1' }).focus();
    await user.keyboard('{ArrowRight}');
    expect(onToggle).toHaveBeenCalledWith('needs');
  });

  it('collapses an open group on left, and steps to the parent from a child', async () => {
    const { user, onToggle } = renderTree();
    screen.getByRole('treeitem', { name: 'Needs you, 1' }).focus();
    await user.keyboard('{ArrowLeft}');
    expect(onToggle).toHaveBeenCalledWith('needs');

    screen.getByRole('treeitem', { name: 'Fix login' }).focus();
    await user.keyboard('{ArrowLeft}');
    expect(screen.getByRole('treeitem', { name: 'Needs you, 1' })).toHaveFocus();
  });

  it('opens the row on Enter, and asks Mainloop about it on a', async () => {
    const { user, onOpen, onAsk } = renderTree();
    screen.getByRole('treeitem', { name: 'Fix login' }).focus();
    await user.keyboard('{Enter}');
    expect(onOpen).toHaveBeenCalledWith({ to: 'task', id: 't1' });
    await user.keyboard('a');
    expect(onAsk).toHaveBeenCalledWith('About task Fix login (t1): ');
  });

  it('jumps to the first and last row with Home and End', async () => {
    const { user } = renderTree(['needs', 'working']);
    screen.getByRole('treeitem', { name: 'Needs you, 1' }).focus();
    await user.keyboard('{End}');
    expect(screen.getByRole('treeitem', { name: /^Nothing running/ })).toHaveFocus();
    await user.keyboard('{Home}');
    expect(screen.getByRole('treeitem', { name: /^Mainloop/ })).toHaveFocus();
  });

  it('keeps one row in the tab order', () => {
    renderTree();
    const stops = screen.getAllByRole('treeitem').filter((item) => item.tabIndex === 0);
    expect(stops).toHaveLength(1);
    expect(stops[0]).toHaveAttribute('aria-level', '1');
  });
});

describe('explorer tree mouse and ask', () => {
  it('opens a row when it is clicked', async () => {
    const { user, onOpen } = renderTree();
    await user.click(screen.getByText('Mainloop'));
    expect(onOpen).toHaveBeenCalledWith({ to: 'chat' });
  });

  it('toggles a group from its chevron without opening it', async () => {
    const { user, onOpen, onToggle } = renderTree();
    await user.click(
      screen
        .getByRole('treeitem', { name: 'Needs you, 1' })
        .querySelector('span[aria-hidden="true"]') as HTMLElement
    );
    expect(onToggle).toHaveBeenCalledWith('needs');
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('asks Mainloop from the row’s button, and does not open the row', async () => {
    const { user, onAsk, onOpen } = renderTree();
    await user.click(screen.getByRole('button', { name: 'Ask Mainloop about Fix login' }));
    expect(onAsk).toHaveBeenCalledWith('About task Fix login (t1): ');
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('reads a placeholder row without opening anything', async () => {
    const { user, onOpen } = renderTree(['needs', 'working']);
    await user.click(screen.getByText('Nothing running'));
    expect(onOpen).not.toHaveBeenCalled();
  });
});

describe('explorer tree focus state', () => {
  const deep: ExplorerNode[] = [
    {
      id: 'working',
      label: 'Working',
      count: 1,
      children: [
        {
          id: 'working/task:t2',
          label: 'Pick queue backend',
          detail: 'Running',
          tone: 'active',
          target: { to: 'task', id: 't2' },
          ask: 'About task Pick queue backend (t2): ',
          children: [
            {
              id: 'working/task:t2/session:s1',
              label: 'Claude session',
              detail: 'running',
              target: { to: 'session', id: 's1' },
              children: []
            }
          ]
        }
      ]
    }
  ];

  function renderDeep() {
    render(
      <ExplorerTree
        nodes={deep}
        isExpanded={() => true}
        onToggle={vi.fn()}
        onOpen={vi.fn()}
        onAsk={vi.fn()}
      />
    );
    return userEvent.setup();
  }

  const row = (name: string) => screen.getByRole('treeitem', { name });

  it('puts the tab stop and selection on the focused level-2 and level-3 rows, not their ancestors', async () => {
    const user = renderDeep();
    row('Working, 1').focus();
    await user.keyboard('{ArrowDown}');
    const task = row('Pick queue backend, Running');
    expect(task).toHaveFocus();
    expect(task).toHaveAttribute('aria-level', '2');
    expect(task).toHaveAttribute('tabindex', '0');
    expect(task).toHaveAttribute('aria-selected', 'true');
    expect(row('Working, 1')).toHaveAttribute('tabindex', '-1');
    expect(row('Working, 1')).toHaveAttribute('aria-selected', 'false');

    await user.keyboard('{ArrowDown}');
    const session = row('Claude session, running');
    expect(session).toHaveFocus();
    expect(session).toHaveAttribute('aria-level', '3');
    expect(session).toHaveAttribute('tabindex', '0');
    expect(session).toHaveAttribute('aria-selected', 'true');
    for (const name of ['Working, 1', 'Pick queue backend, Running']) {
      expect(row(name)).toHaveAttribute('tabindex', '-1');
      expect(row(name)).toHaveAttribute('aria-selected', 'false');
    }
  });

  it('draws the ring from keyboard focus on the row itself, so a tree never focused shows none', () => {
    renderDeep();
    for (const item of screen.getAllByRole('treeitem')) {
      // The ring rule targets only the treeitem's own row, while the browser shows focus on it.
      expect(item.className).toContain('[&:focus-visible>div]:ring-1');
      expect(item.firstElementChild?.className ?? '').not.toMatch(/(^|\s)ring-1(\s|$)/);
    }
    expect(document.activeElement).toBe(document.body);
  });

  it('names each row from its label and status, not its buttons', () => {
    renderDeep();
    expect(row('Pick queue backend, Running')).toBeInTheDocument();
    expect(screen.queryByRole('treeitem', { name: /Ask Mainloop/ })).toBeNull();
  });

  it('draws one indent guide per ancestor', () => {
    renderDeep();
    const guides = (name: string) =>
      within(row(name).firstElementChild as HTMLElement).queryAllByTestId('indent-guide');
    expect(guides('Working, 1')).toHaveLength(0);
    expect(guides('Pick queue backend, Running')).toHaveLength(1);
    expect(guides('Claude session, running')).toHaveLength(2);
  });
});
