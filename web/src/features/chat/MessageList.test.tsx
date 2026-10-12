import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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

describe('MessageList with a request card', () => {
  // jsdom has no layout: the test drives the observer and says where things are.
  let fire: (targets: Element[]) => void = () => {};
  beforeEach(() => {
    vi.stubGlobal(
      'ResizeObserver',
      class {
        constructor(private callback: ResizeObserverCallback) {
          fire = (targets) =>
            this.callback(
              targets.map((target) => ({ target }) as ResizeObserverEntry),
              this as unknown as ResizeObserver
            );
        }
        observe() {}
        disconnect() {}
      }
    );
  });
  afterEach(() => vi.unstubAllGlobals());

  function at(el: Element, top: number) {
    vi.spyOn(el, 'getBoundingClientRect').mockReturnValue({ top } as DOMRect);
  }

  function setup() {
    const messages = [messageFixture({ id: 'a' })];
    const view = render(
      <MessageList
        {...base}
        messages={messages}
        requests={<button type="button">Show all of the arguments</button>}
      />
    );
    const list = screen.getByTestId('message-list');
    const cards = screen.getByTestId('thread-requests');
    const log = screen.getByRole('log', { name: 'Conversation' });
    // Pinned to the bottom, with the tall card's top 400 px above the view.
    geometry(list, 700);
    fireEvent.scroll(list);
    at(list, 0);
    at(cards, -400);
    return { ...view, messages, list, cards, log };
  }

  it('keeps the cards out of the log, in the same scroll', () => {
    const { list, cards, log } = setup();
    expect(log).not.toContainElement(cards);
    expect(list).toContainElement(cards);
  });

  it('brings a card that fills in into view top first, and the log growing does not undo it', () => {
    const { list, cards, log } = setup();
    fire([cards]);
    expect(list.scrollTop).toBe(292);
    fire([log]);
    expect(list.scrollTop).toBe(292);
  });

  it('leaves a card the owner expanded where it is, until the next message', () => {
    const { list, cards, messages, rerender } = setup();
    fireEvent.pointerDown(screen.getByRole('button', { name: 'Show all of the arguments' }));
    fire([cards]);
    expect(list.scrollTop).toBe(700);
    rerender(
      <MessageList
        {...base}
        messages={[...messages, messageFixture({ id: 'b' })]}
        requests={<button type="button">Show all of the arguments</button>}
      />
    );
    geometry(list, 700);
    fireEvent.scroll(list);
    fire([cards]);
    expect(list.scrollTop).toBe(292);
  });

  it('keeps following new messages to the bottom while the owner is there', () => {
    const { list, log } = setup();
    geometry(list, 700, 1400);
    fire([log]);
    expect(list.scrollTop).toBe(1400);
  });
});
