import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';
import { Button } from '@/components/ui/button';
import { loadDraft, saveDraft } from './drafts';

const MAX_HEIGHT_PX = 160;

export interface ComposerProps {
  /** Which draft this box holds: the conversation id, or `new` before there is one. */
  draftKey: string;
  /** A turn is in flight, or a send is pending. */
  busy: boolean;
  /** The thread has not been read yet, so the draft's conversation is not known. */
  loading?: boolean;
  /** The first read of the thread failed, so there is nothing to send to. */
  unavailable?: boolean;
  /** The backend is unreachable. The draft is kept and sending is off. */
  offline: boolean;
  /** The thread takes no more messages, so the box is locked. The thread says why. */
  closed?: boolean;
  /** What the locked box says in place of its text. */
  closedPlaceholder?: string;
  /**
   * Sends the text. Resolves true once the backend recorded the message; false puts the text
   * back in the box. The caller clears the stored draft on success.
   */
  onSend: (text: string) => Promise<boolean>;
  /**
   * Text to put in the box once, unsent, with the caret at its end. The box keeps any text already
   * there after it. The caller clears the prefill from its own state in `onPrefillUsed`.
   */
  prefill?: string;
  onPrefillUsed?: () => void;
}

function placeholder(
  offline: boolean,
  busy: boolean,
  loading: boolean,
  unavailable: boolean,
  closed: string | null
) {
  if (offline) return 'Backend unreachable…';
  if (unavailable) return 'Thread did not load…';
  if (closed) return closed;
  if (loading) return 'Loading…';
  if (busy) return 'Working…';
  return 'Enter command...';
}

/** The text box and EXEC button. Enter sends, Shift+Enter adds a line, IME composition never sends. */
export function Composer({
  draftKey,
  busy,
  loading = false,
  unavailable = false,
  offline,
  closed = false,
  closedPlaceholder = 'Closed to new messages',
  onSend,
  prefill,
  onPrefillUsed
}: ComposerProps) {
  const [text, setText] = useState(() => loadDraft(draftKey));
  const [shownKey, setShownKey] = useState(draftKey);
  const [sending, setSending] = useState(false);
  const input = useRef<HTMLTextAreaElement>(null);
  // The prefill last applied, so one prefill fills the box once even while its caller re-renders.
  const appliedPrefill = useRef<string | undefined>(undefined);
  // Set when a prefill lands, so the caret moves to its end once the box can take focus.
  const caretToEnd = useRef(false);

  // A different conversation shows its own draft. Adjusted while rendering, not in an effect, so
  // the old text never flashes under the new key.
  if (shownKey !== draftKey) {
    setShownKey(draftKey);
    setText(loadDraft(draftKey));
  }

  const disabled = busy || loading || unavailable || offline || closed || sending;

  // Grow with the text up to a few lines, then scroll.
  useLayoutEffect(() => {
    const el = input.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, MAX_HEIGHT_PX)}px`;
  }, [text]);

  useEffect(() => {
    if (!prefill) appliedPrefill.current = undefined;
    if (!prefill || prefill === appliedPrefill.current) return;
    appliedPrefill.current = prefill;
    const existing = loadDraft(draftKey);
    const next = existing.trim() ? prefill + existing : prefill;
    setText(next);
    saveDraft(draftKey, next);
    caretToEnd.current = true;
    onPrefillUsed?.();
  }, [prefill, draftKey, onPrefillUsed]);

  // The caret goes to the end of a prefill once the box is enabled, so the owner can type on.
  useEffect(() => {
    const el = input.current;
    if (!caretToEnd.current || disabled || !el) return;
    caretToEnd.current = false;
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }, [disabled, text]);

  // Hand focus back when the box unlocks, unless the owner moved it somewhere else.
  const wasDisabled = useRef(disabled);
  useEffect(() => {
    if (wasDisabled.current && !disabled && document.activeElement === document.body) {
      input.current?.focus();
    }
    wasDisabled.current = disabled;
  }, [disabled]);

  function change(next: string) {
    setText(next);
    saveDraft(draftKey, next);
  }

  async function submit() {
    const message = text.trim();
    if (!message || disabled) return;
    setSending(true);
    setText('');
    let accepted = false;
    try {
      accepted = await onSend(message);
    } finally {
      setSending(false);
    }
    // The caller keeps the stored draft until the backend accepts; on refusal the text returns.
    if (!accepted) setText((current) => current || message);
  }

  function keyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    void submit();
  }

  return (
    <div
      className="border-border bg-background px-md py-sm gap-sm relative flex shrink-0 items-end border-t"
      data-testid="input-bar"
      aria-busy={sending || busy}
    >
      <span className="text-term-accent flex min-h-11 items-center" aria-hidden="true">
        $
      </span>
      <textarea
        ref={input}
        rows={1}
        value={text}
        disabled={disabled}
        placeholder={placeholder(
          offline,
          busy,
          loading,
          unavailable,
          closed ? closedPlaceholder : null
        )}
        aria-label="Message"
        onChange={(event) => change(event.target.value)}
        onKeyDown={keyDown}
        data-testid="command-input"
        className="border-input bg-background text-foreground placeholder:text-muted-foreground focus-visible:ring-ring px-sm py-sm min-h-11 min-w-0 flex-1 resize-none border text-sm outline-none focus-visible:ring-1 disabled:opacity-50"
      />
      <Button
        disabled={disabled || !text.trim()}
        onClick={() => void submit()}
        aria-label="Send message"
        data-testid="exec-button"
      >
        EXEC
      </Button>
      {/* sr-only is absolutely positioned. The bar is its positioned ancestor, so it cannot grow the page. */}
      <span role="status" className="sr-only">
        {sending ? 'Sending' : ''}
      </span>
    </div>
  );
}
