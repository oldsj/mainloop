import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { loadDraft, saveDraft } from './drafts';
import { Composer } from './Composer';

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
