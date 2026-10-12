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
