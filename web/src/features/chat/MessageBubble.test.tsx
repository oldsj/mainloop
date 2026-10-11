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

  it('labels a child report as the child, not the owner', () => {
    render(
      <MessageBubble
        message={messageFixture({
          content: "[report from child 0a1b2c3d 'Fix the build']\nAll tests pass."
        })}
      />
    );
    const report = screen.getByTestId('child-report');
    expect(report).toHaveTextContent('child · Fix the build');
    expect(report).toHaveTextContent('All tests pass.');
    expect(report).not.toHaveTextContent('report from child');
    expect(report).not.toHaveTextContent('user@main$');
  });

  it('says when a child ended without a report', () => {
    render(
      <MessageBubble
        message={messageFixture({ content: "[report from child 0a1b2c3d 'Fix' fallback]\nended" })}
      />
    );
    expect(screen.getByTestId('child-report')).toHaveTextContent(
      'child · Fix (ended without a report)'
    );
  });

  it('does not make an assistant message a child report', () => {
    render(
      <MessageBubble
        message={messageFixture({
          role: 'assistant',
          content: "[report from child 0a1b2c3d 'x']\nbody"
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
