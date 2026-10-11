import { render, screen, within } from '@testing-library/react';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { describe, expect, it } from 'vitest';
import { SessionRow } from './SessionRow';
import { session } from './test/fixtures';

// The open row is the one the router marks `aria-current`, and the fill and bold keyed on that
// attribute are CSS, which jsdom does not compute. So these tests check the attribute and the
// `▌` marker that sits in the open row: the non-colour cue for selection.
function renderRowAt(path: string) {
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/sessions/$id',
        component: () => (
          <SessionRow session={session({ id: 's1', title: 'Open work' })} providers={[]} />
        )
      })
    ]),
    history: createMemoryHistory({ initialEntries: [path] })
  });
  render(<RouterProvider router={router} />);
}

describe('session row selection', () => {
  it('marks the open row with aria-current and a marker that is not colour', async () => {
    renderRowAt('/sessions/s1');
    const row = await screen.findByRole('link', { name: /Open work/ });
    expect(row).toHaveAttribute('aria-current', 'page');
    expect(within(row).getByText('▌')).toHaveAttribute('aria-hidden', 'true');
  });

  it('leaves a row that is not open without aria-current', async () => {
    renderRowAt('/sessions/s2');
    const row = await screen.findByRole('link', { name: /Open work/ });
    expect(row).not.toHaveAttribute('aria-current');
  });
});
