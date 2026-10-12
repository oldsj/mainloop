import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { deliveryNotices } from '@/lib/delivery';
import { deliveryFixture, messageFixture } from './fixtures';
import { MessageList } from './MessageList';
import { fireEvent } from '@testing-library/react';

const base = {
  notices: new Map(),
  loaded: true,
  retryDisabled: false,
  onRetry: vi.fn()
};

function geometry(el: HTMLElement, scrollTop: number, scrollHeight = 1000, clientHeight = 300) {
  Object.defineProperty(el, 'scrollHeight', { configurable: true, value: scrollHeight });
  Object.defineProperty(el, 'clientHeight', { configurable: true, value: clientHeight });
  el.scrollTop = scrollTop;
}

describe('MessageList', () => {
  it('shows the empty state once loaded, and "connecting" before', () => {
    const { rerender } = render(<MessageList {...base} messages={[]} loaded />);
    expect(screen.getByText('$ mainloop --help')).toBeInTheDocument();
    expect(screen.getByText('Start a conversation to begin')).toBeInTheDocument();
    rerender(<MessageList {...base} messages={[]} loaded={false} />);
    expect(screen.getByText('$ connecting')).toBeInTheDocument();
    expect(screen.getByText('Waiting for the backend…')).toBeInTheDocument();
  });

  it('starts at the newest message', () => {
    render(<MessageList {...base} messages={[messageFixture()]} />);
    // jsdom has no layout, so the pinned position equals the (zero) scroll height.
    expect(screen.getByTestId('message-list').scrollTop).toBe(0);
  });

  it('follows new messages while the reader is near the bottom', () => {
    const messages = [messageFixture({ id: 'a' })];
    const { rerender } = render(<MessageList {...base} messages={messages} />);
    const list = screen.getByTestId('message-list');
    geometry(list, 600); // 100 px from the end
    fireEvent.scroll(list);
    geometry(list, 600, 1200);
    rerender(<MessageList {...base} messages={[...messages, messageFixture({ id: 'b' })]} />);
    expect(list.scrollTop).toBe(1200);
  });

  it('stays put when the reader scrolled up, and offers the button', () => {
    const messages = [messageFixture({ id: 'a' })];
    const { rerender } = render(<MessageList {...base} messages={messages} />);
    const list = screen.getByTestId('message-list');
    expect(screen.queryByTestId('scroll-to-bottom')).toBeNull();
    geometry(list, 100); // 600 px from the end
    fireEvent.scroll(list);
    expect(screen.getByTestId('scroll-to-bottom')).toBeInTheDocument();
    geometry(list, 100, 1200);
    rerender(<MessageList {...base} messages={[...messages, messageFixture({ id: 'b' })]} />);
    expect(list.scrollTop).toBe(100);
  });

  it('re-pins when a delivery notice lands on the last message', () => {
    const messages = [messageFixture({ id: 'a' })];
    const { rerender } = render(<MessageList {...base} messages={messages} />);
    const list = screen.getByTestId('message-list');
    geometry(list, 600); // 100 px from the end
    fireEvent.scroll(list);
    geometry(list, 600, 1200); // the notice makes the last message taller
    rerender(
      <MessageList
        {...base}
        messages={messages}
        notices={deliveryNotices([deliveryFixture({ message_id: 'a', state: 'failed' })])}
      />
    );
    expect(list.scrollTop).toBe(1200);
  });

  it('does not pull a reader who scrolled up back down for a notice', () => {
    const messages = [messageFixture({ id: 'a' })];
    const { rerender } = render(<MessageList {...base} messages={messages} />);
    const list = screen.getByTestId('message-list');
    geometry(list, 100); // 600 px from the end
    fireEvent.scroll(list);
    geometry(list, 100, 1200);
    rerender(
      <MessageList
        {...base}
        messages={messages}
        notices={deliveryNotices([deliveryFixture({ message_id: 'a', state: 'failed' })])}
      />
    );
    expect(list.scrollTop).toBe(100);
  });

  it('labels each message with the thread it was written in', () => {
    render(
      <MessageList
        {...base}
        messages={[messageFixture({ role: 'assistant', content: 'ok' })]}
        agentKind="claude"
        context="Fix sweep"
      />
    );
    expect(screen.getByText('claude@Fix sweep$')).toBeInTheDocument();
  });

  it('hides the button within 100 px of the end', () => {
    render(<MessageList {...base} messages={[messageFixture()]} />);
    const list = screen.getByTestId('message-list');
    geometry(list, 100);
    fireEvent.scroll(list);
    expect(screen.getByTestId('scroll-to-bottom')).toBeInTheDocument();
    geometry(list, 650);
    fireEvent.scroll(list);
    expect(screen.queryByTestId('scroll-to-bottom')).toBeNull();
  });
});
