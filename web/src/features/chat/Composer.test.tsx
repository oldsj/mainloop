import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { loadDraft, saveDraft } from './drafts';
import { Composer } from './Composer';
import type { SlashCommandSpec } from './slashCommand';

afterEach(() => sessionStorage.clear());

function setup(props: Partial<React.ComponentProps<typeof Composer>> = {}) {
  const onSend = vi.fn().mockResolvedValue(true);
  const view = render(
    <Composer draftKey="c1" busy={false} offline={false} onSend={onSend} {...props} />
  );
  return { onSend, ...view };
}

const input = () => screen.getByTestId('command-input') as HTMLTextAreaElement;

describe('Composer', () => {
  it('sends on Enter, trimmed, and clears the box', async () => {
    const { onSend } = setup();
    await userEvent.type(input(), '  hello  {Enter}');
    await waitFor(() => expect(onSend).toHaveBeenCalledWith('hello'));
    expect(input().value).toBe('');
  });

  it('adds a line on Shift+Enter', async () => {
    const { onSend } = setup();
    await userEvent.type(input(), 'a{Shift>}{Enter}{/Shift}b');
    expect(input().value).toBe('a\nb');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('does not send while an IME composition is open', () => {
    const { onSend } = setup();
    fireEvent.change(input(), { target: { value: 'にほん' } });
    fireEvent.keyDown(input(), { key: 'Enter', isComposing: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  it('sends from the EXEC button, which is empty-disabled', async () => {
    const { onSend } = setup();
    expect(screen.getByTestId('exec-button')).toBeDisabled();
    await userEvent.type(input(), 'go');
    await userEvent.click(screen.getByTestId('exec-button'));
    await waitFor(() => expect(onSend).toHaveBeenCalledWith('go'));
  });

  it('puts the text back when the send is not accepted', async () => {
    const { onSend } = setup();
    onSend.mockResolvedValue(false);
    await userEvent.type(input(), 'keep me{Enter}');
    await waitFor(() => expect(input().value).toBe('keep me'));
  });

  it('keeps the stored draft until the caller clears it', async () => {
    const { onSend } = setup();
    await userEvent.type(input(), 'draft');
    expect(loadDraft('c1')).toBe('draft');
    await userEvent.keyboard('{Enter}');
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(loadDraft('c1')).toBe('draft');
  });

  it('restores the draft of its conversation', () => {
    saveDraft('c1', 'earlier');
    setup();
    expect(input().value).toBe('earlier');
  });

  it('shows another conversation its own draft', () => {
    saveDraft('c1', 'one');
    saveDraft('c2', 'two');
    const { rerender, onSend } = setup();
    expect(input().value).toBe('one');
    rerender(<Composer draftKey="c2" busy={false} offline={false} onSend={onSend} />);
    expect(input().value).toBe('two');
  });

  it('locks while busy, with the reason in the placeholder', () => {
    setup({ busy: true });
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Working…');
    expect(screen.getByTestId('exec-button')).toBeDisabled();
  });

  it('keeps the draft and locks while the backend is unreachable', () => {
    saveDraft('c1', 'unsent');
    setup({ offline: true });
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Backend unreachable…');
    expect(input().value).toBe('unsent');
  });

  it('refocuses when it unlocks and focus has nowhere else', () => {
    const { rerender, onSend } = setup({ busy: true });
    rerender(<Composer draftKey="c1" busy={false} offline={false} onSend={onSend} />);
    expect(input()).toHaveFocus();
  });

  it('leaves focus alone when the owner moved it', () => {
    const { rerender, onSend } = setup({ busy: true });
    const other = document.createElement('button');
    document.body.append(other);
    other.focus();
    rerender(<Composer draftKey="c1" busy={false} offline={false} onSend={onSend} />);
    expect(other).toHaveFocus();
    other.remove();
  });

  it('contains its screen-reader status in a positioned box', () => {
    setup();
    // sr-only is position:absolute. Without a positioned ancestor it sizes the page to itself.
    const status = screen.getByRole('status');
    expect(status.closest('[data-testid="input-bar"]')).toHaveClass('relative');
  });
});

// "move" diverges from "model" at its third letter, so a query can match both, only one, or
// neither, without one name ever being a prefix of the other.
const COMMANDS: SlashCommandSpec[] = [
  { name: 'model', summary: 'Switch the main model provider' },
  { name: 'move', summary: 'Move the conversation' }
];

const menu = () => screen.queryByTestId('slash-command-menu');
const option = (name: string) => screen.getByTestId(`slash-command-option-${name}`);

describe('Composer slash command autosuggest', () => {
  it('stays closed with no commands registered, even while typing a slash', async () => {
    setup();
    await userEvent.type(input(), '/model');
    expect(menu()).toBeNull();
  });

  it('opens on a leading slash, listing every command', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/');
    expect(menu()).not.toBeNull();
    expect(option('model')).toBeInTheDocument();
    expect(option('move')).toBeInTheDocument();
  });

  it('narrows to commands whose name starts with what was typed, case-insensitively', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/Mo');
    expect(option('model')).toBeInTheDocument();
    expect(option('move')).toBeInTheDocument();
  });

  it('narrows to a single match once enough of the name is typed', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/Mod');
    expect(option('model')).toBeInTheDocument();
    expect(screen.queryByTestId('slash-command-option-move')).toBeNull();
  });

  it('closes once the caret moves past the command name into its arguments', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/model more text');
    expect(menu()).toBeNull();
  });

  it('closes on ordinary text that does not start with a slash', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), 'hello /model');
    expect(menu()).toBeNull();
  });

  it('highlights the first match by default and moves it with Up/Down, wrapping both ways', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/mo');
    expect(option('model')).toHaveAttribute('aria-selected', 'true');
    expect(option('move')).toHaveAttribute('aria-selected', 'false');

    await userEvent.keyboard('{ArrowDown}');
    expect(option('move')).toHaveAttribute('aria-selected', 'true');
    expect(option('model')).toHaveAttribute('aria-selected', 'false');

    await userEvent.keyboard('{ArrowDown}');
    expect(option('model')).toHaveAttribute('aria-selected', 'true');

    await userEvent.keyboard('{ArrowUp}');
    expect(option('move')).toHaveAttribute('aria-selected', 'true');
  });

  it('Tab accepts the highlighted command, inserting it with a trailing space and closing the menu', async () => {
    const { onSend } = setup({ commands: COMMANDS });
    await userEvent.type(input(), '/mo');
    await userEvent.keyboard('{ArrowDown}'); // highlight "move"
    await userEvent.keyboard('{Tab}');
    expect(input().value).toBe('/move ');
    expect(menu()).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
    // The caret lands right after the inserted space, so typing continues the arguments.
    await userEvent.type(input(), 'x');
    expect(input().value).toBe('/move x');
  });

  it('Enter accepts the highlighted command instead of sending, while the name is not yet exact', async () => {
    const { onSend } = setup({ commands: COMMANDS });
    await userEvent.type(input(), '/mo{Enter}');
    expect(input().value).toBe('/model ');
    expect(menu()).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
  });

  it('Enter sends once the typed name is already an exact command, instead of re-accepting it', async () => {
    const { onSend } = setup({ commands: COMMANDS });
    await userEvent.type(input(), '/model{Enter}');
    await waitFor(() => expect(onSend).toHaveBeenCalledWith('/model'));
    expect(input().value).toBe('');
  });

  it('Escape closes the menu without changing the text, and typing more reopens it', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/mo');
    await userEvent.keyboard('{Escape}');
    expect(menu()).toBeNull();
    expect(input().value).toBe('/mo');

    await userEvent.type(input(), 'd');
    expect(menu()).not.toBeNull();
  });

  it('accepts a command on a mouse click without losing focus', async () => {
    setup({ commands: COMMANDS });
    await userEvent.type(input(), '/mo');
    await userEvent.click(option('move'));
    expect(input().value).toBe('/move ');
    expect(input()).toHaveFocus();
    expect(menu()).toBeNull();
  });

  it('leaves ordinary typing and Enter-to-send unaffected when commands are registered', async () => {
    const { onSend } = setup({ commands: COMMANDS });
    await userEvent.type(input(), 'just a normal message{Enter}');
    await waitFor(() => expect(onSend).toHaveBeenCalledWith('just a normal message'));
    expect(input().value).toBe('');
  });
});

describe('Composer prefill', () => {
  const PREFILL = 'Start a task in acme/widgets: ';

  it('fills the box once, unsent, with the caret at the end, and reports it taken', async () => {
    const onPrefillUsed = vi.fn();
    const { onSend } = setup({ prefill: PREFILL, onPrefillUsed });
    await waitFor(() => expect(input().value).toBe(PREFILL));
    expect(onSend).not.toHaveBeenCalled();
    expect(onPrefillUsed).toHaveBeenCalledTimes(1);
    expect(document.activeElement).toBe(input());
    expect(input().selectionStart).toBe(PREFILL.length);
    expect(loadDraft('c1')).toBe(PREFILL);
  });

  it('keeps the owner’s existing text after the prefill', async () => {
    saveDraft('c1', 'half a thought');
    setup({ prefill: PREFILL });
    await waitFor(() => expect(input().value).toBe(PREFILL + 'half a thought'));
  });

  it('does not stack the prefill on a draft that already starts with it', async () => {
    saveDraft('c1', PREFILL + 'half a thought');
    const onPrefillUsed = vi.fn();
    setup({ prefill: PREFILL, onPrefillUsed });
    await waitFor(() => expect(onPrefillUsed).toHaveBeenCalledTimes(1));
    expect(input().value).toBe(PREFILL + 'half a thought');
  });

  it('does not refill when the caller re-renders with the same prefill', async () => {
    const onPrefillUsed = vi.fn();
    const { rerender } = render(
      <Composer
        draftKey="c1"
        busy={false}
        offline={false}
        onSend={vi.fn()}
        prefill={PREFILL}
        onPrefillUsed={onPrefillUsed}
      />
    );
    await userEvent.type(input(), 'more');
    rerender(
      <Composer
        draftKey="c1"
        busy={false}
        offline={false}
        onSend={vi.fn()}
        prefill={PREFILL}
        onPrefillUsed={() => onPrefillUsed()}
      />
    );
    expect(input().value).toBe(PREFILL + 'more');
    expect(onPrefillUsed).toHaveBeenCalledTimes(1);
  });
});

describe('Composer prefill when the conversation arrives later', () => {
  const PREFILL = 'Start a task in acme/widgets: ';

  it('keeps the prefill when the main thread gets its conversation id after the prefill', async () => {
    const props = { busy: false, offline: false, onSend: vi.fn() };
    const { rerender } = render(<Composer draftKey="new" prefill={PREFILL} {...props} />);
    await waitFor(() => expect(input().value).toBe(PREFILL));

    rerender(<Composer draftKey="conv-1" prefill={PREFILL} {...props} />);

    expect(input().value).toBe(PREFILL);
    expect(loadDraft('conv-1')).toBe(PREFILL);
    expect(loadDraft('new')).toBe('');
    expect(props.onSend).not.toHaveBeenCalled();
  });

  it('keeps text typed before the conversation existed, and does not overwrite a stored draft', async () => {
    saveDraft('conv-1', 'already here');
    const props = { busy: false, offline: false, onSend: vi.fn() };
    const { rerender } = render(<Composer draftKey="new" {...props} />);
    await userEvent.type(input(), 'typed early');

    rerender(<Composer draftKey="conv-1" {...props} />);

    expect(input().value).toBe('typed earlyalready here');
    expect(loadDraft('conv-1')).toBe('typed earlyalready here');
  });
});
