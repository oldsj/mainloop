import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { DeliveryNotice } from '@/lib/delivery';
import { messageFixture } from './fixtures';
import { MessageBubble, STOPPED_NOTE } from './MessageBubble';

describe('MessageBubble', () => {
  it('labels the author and renders markdown safely', () => {
    render(
      <MessageBubble
        message={messageFixture({
          role: 'assistant',
          content: 'See [docs](https://example.test) and <b>raw</b>'
        })}
        agentKind="claude"
      />
    );
    expect(screen.getByText('claude@main$')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'docs' })).toHaveAttribute(
      'rel',
      'noopener noreferrer'
    );
    expect(screen.getByText(/<b>raw<\/b>/)).toBeInTheDocument();
  });

  it('shows an assignment collapsed, with its text on demand and no "You" label', async () => {
    render(
      <MessageBubble
        message={messageFixture({ role: 'user', content: 'Build the **parser** brief' })}
        agentKind="claude"
        assignment
      />
    );
    const block = screen.getByTestId('assignment-block');
    expect(block).not.toHaveAttribute('open');
    expect(screen.getByRole('article', { name: /^Assignment from Mainloop/ })).toBeInTheDocument();
    expect(screen.queryByRole('article', { name: /^You/ })).toBeNull();
    await userEvent.click(within(block).getByText(/Assignment from Mainloop/));
    expect(block).toHaveAttribute('open');
    expect(within(block).getByText('parser')).toBeInTheDocument();
  });

  it('keeps a message from the owner labelled "You" when no assignment is given', () => {
    render(<MessageBubble message={messageFixture({ role: 'user', content: 'hi' })} />);
    expect(screen.getByRole('article', { name: /^You/ })).toBeInTheDocument();
    expect(screen.queryByTestId('assignment-block')).toBeNull();
  });

  it('labels a reply by the provider that wrote it, not the session it is bound to now', () => {
    render(
      <MessageBubble
        message={messageFixture({ role: 'assistant', content: 'done', provider: 'claude' })}
        agentKind="codex"
      />
    );
    expect(screen.getByText('claude@main$')).toBeInTheDocument();
    expect(screen.queryByText('codex@main$')).toBeNull();
  });

  it('labels the prompt with the thread it was written in', () => {
    render(
      <MessageBubble
        message={messageFixture({ role: 'assistant', content: 'done' })}
        agentKind="claude"
        context="Fix sweep connection pinning"
      />
    );
    expect(screen.getByText('claude@Fix sweep connection pinning$')).toBeInTheDocument();
    expect(screen.queryByText('claude@main$')).toBeNull();
  });

  it('strips the stopped note, marks the author and shows the note once', () => {
    render(
      <MessageBubble
        message={messageFixture({
          role: 'assistant',
          content: `Partial answer\n\n${STOPPED_NOTE}`
        })}
      />
    );
    expect(screen.getByText(/· stopped/)).toBeInTheDocument();
    expect(screen.getByTestId('turn-stopped-note')).toHaveTextContent(STOPPED_NOTE);
    expect(screen.getAllByText(STOPPED_NOTE)).toHaveLength(1);
    expect(screen.getByText('Partial answer')).toBeInTheDocument();
  });

  it('does not treat a user message as stopped', () => {
    render(<MessageBubble message={messageFixture({ content: STOPPED_NOTE })} />);
    expect(screen.queryByTestId('turn-stopped-note')).toBeNull();
  });

  it('labels a child report as the task, not the owner', () => {
    render(
      <MessageBubble
        message={messageFixture({
          content:
            '[report from child task 0a1b2c3d-1111-4222-8333-444455556666 attempt 1; completed; unverified claim]\nAll tests pass.'
        })}
      />
    );
    const report = screen.getByTestId('child-report');
    expect(report).toHaveTextContent('child task 0a1b2c3d · completed');
    expect(report).toHaveTextContent('All tests pass.');
    expect(report).not.toHaveTextContent('report from child');
    expect(report).not.toHaveTextContent('user@main$');
  });

  it('shows a blocked report with its owner action, expanded and never as an assignment', () => {
    render(
      <MessageBubble
        assignment
        message={messageFixture({
          content:
            '[report from child task 9f8e7d6c-1111-4222-8333-444455556666 attempt 2; blocked; unverified claim]\nThe schema is ambiguous.\nOwner action needed: decide the schema'
        })}
      />
    );
    const report = screen.getByTestId('child-report');
    expect(report).toHaveTextContent('child task 9f8e7d6c · blocked');
    expect(report).toHaveTextContent('Owner action needed: decide the schema');
    expect(screen.queryByTestId('assignment-block')).toBeNull();
    expect(screen.queryByRole('group')).toBeNull();
  });

  it.each([
    [
      'a child task',
      '[report from child task 0a1b2c3d-1111-4222-8333-444455556666 attempt 1; completed; unverified claim]\nok',
      /^child task 0a1b2c3d · completed, /
    ],
    [
      'a top-level task',
      '[report from task 5e4d3c2b-1111-4222-8333-444455556666 attempt 1; failed; unverified claim]\nno',
      /^task 5e4d3c2b · failed, /
    ]
  ])('names the article of a report from %s by its title and outcome', (_name, content, name) => {
    render(<MessageBubble message={messageFixture({ content })} />);
    expect(screen.getByRole('article', { name })).toBe(screen.getByTestId('child-report'));
  });

  it('does not make an assistant message a child report', () => {
    render(
      <MessageBubble
        message={messageFixture({
          role: 'assistant',
          content:
            '[report from child task 0a1b2c3d-1111-4222-8333-444455556666 attempt 1; completed; unverified claim]\nbody'
        })}
      />
    );
    expect(screen.queryByTestId('child-report')).toBeNull();
  });

  describe('task references', () => {
    function renderMessageAt(path: string, content: string) {
      const root = createRootRoute({ component: Outlet });
      const router = createRouter({
        routeTree: root.addChildren([
          createRoute({
            getParentRoute: () => root,
            path: '/',
            component: () => (
              <MessageBubble message={messageFixture({ role: 'assistant', content })} />
            )
          }),
          createRoute({
            getParentRoute: () => root,
            path: '/tasks/$id',
            component: () => <p>Task page</p>
          })
        ]),
        history: createMemoryHistory({ initialEntries: [path] })
      });
      render(<RouterProvider router={router} />);
    }

    it('renders a task reference as a clickable in-app link and navigates on click', async () => {
      renderMessageAt(
        '/',
        'See [Fix sweep connection pinning](/tasks/158f277e-83bc-4584-9b29-88d217542832)'
      );
      const link = await screen.findByRole('link', { name: 'Fix sweep connection pinning' });
      expect(link).toHaveAttribute('href', '/tasks/158f277e-83bc-4584-9b29-88d217542832');
      await userEvent.click(link);
      expect(screen.getByText('Task page')).toBeInTheDocument();
    });
  });

  describe('delivery notice', () => {
    const failed: DeliveryNotice = {
      state: 'failed',
      label: 'Not sent',
      reason: 'not sent: agent offline',
      retryable: true
    };

    it('shows the label and reason, and retries the message', async () => {
      const onRetry = vi.fn();
      const message = messageFixture();
      render(<MessageBubble message={message} notice={failed} onRetry={onRetry} />);
      const notice = screen.getByTestId('delivery-notice');
      expect(within(notice).getByTestId('delivery-notice-label')).toHaveTextContent('Not sent');
      expect(within(notice).getByTestId('delivery-notice-reason')).toHaveTextContent(
        'not sent: agent offline'
      );
      await userEvent.click(screen.getByTestId('delivery-retry'));
      expect(onRetry).toHaveBeenCalledWith(message);
    });

    it('disables Retry while retrying is not allowed', () => {
      render(
        <MessageBubble message={messageFixture()} notice={failed} onRetry={vi.fn()} retryDisabled />
      );
      expect(screen.getByTestId('delivery-retry')).toBeDisabled();
    });

    it('offers no Retry for an unconfirmed delivery and says it was not replayed', () => {
      render(
        <MessageBubble
          message={messageFixture()}
          notice={{
            state: 'uncertain',
            label: 'Delivery unconfirmed',
            reason: 'timeout',
            retryable: false
          }}
          onRetry={vi.fn()}
        />
      );
      expect(screen.queryByTestId('delivery-retry')).toBeNull();
      expect(screen.getByTestId('delivery-notice')).toHaveTextContent(
        'Not replayed. Check for a reply before sending again.'
      );
    });

    it('shows no Retry without a handler', () => {
      render(<MessageBubble message={messageFixture()} notice={failed} />);
      expect(screen.queryByTestId('delivery-retry')).toBeNull();
    });

    it('shows the reason as text, never as markup', () => {
      render(
        <MessageBubble
          message={messageFixture()}
          notice={{ ...failed, reason: '<img src=x onerror=alert(1)>' }}
        />
      );
      expect(screen.getByTestId('delivery-notice-reason')).toHaveTextContent('<img');
      expect(screen.getByTestId('delivery-notice').querySelector('img')).toBeNull();
    });
  });
});
