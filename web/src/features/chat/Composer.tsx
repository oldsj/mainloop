import { useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { clearDraft, loadDraft, NEW_CONVERSATION, saveDraft } from './drafts';
import { activeSlashToken, matchSlashCommands, type SlashCommandSpec } from './slashCommand';

const MAX_HEIGHT_PX = 160;

export interface ComposerProps {
  /** Which draft this box holds: the conversation id, or `new` before there is one. */
  draftKey: string;
  /** A send on this thread (this box's own, or another's, such as Retry) is pending. */
  busy: boolean;
  /**
   * A turn is in flight on the thread. Shown in the placeholder only: a send while a turn is open
   * is durably queued behind it rather than refused, so this never disables the box.
   */
  turnBusy?: boolean;
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
  /**
   * Slash commands the autosuggest dropdown offers. Omitted where the box takes no local commands,
   * such as a session's own composer: typed text there always goes to the thread as a message.
   */
  commands?: readonly SlashCommandSpec[];
}

function placeholder(
  offline: boolean,
  working: boolean,
  loading: boolean,
  unavailable: boolean,
  closed: string | null
) {
  if (offline) return 'Backend unreachable…';
  if (unavailable) return 'Thread did not load…';
  if (closed) return closed;
  if (loading) return 'Loading…';
  if (working) return 'Working…';
  return 'Enter command...';
}

/**
 * The text box and EXEC button. Enter sends, Shift+Enter adds a line, IME composition never sends.
 * When `commands` names slash commands, a leading `/` opens an autosuggest dropdown: Up/Down moves
 * the highlight, Tab accepts it, Enter accepts it too unless the typed name is already exact (then
 * it sends), and Escape dismisses the dropdown without accepting.
 */
export function Composer({
  draftKey,
  busy,
  turnBusy = false,
  loading = false,
  unavailable = false,
  offline,
  closed = false,
  closedPlaceholder = 'Closed to new messages',
  onSend,
  prefill,
  onPrefillUsed,
  commands
}: ComposerProps) {
  const [text, setText] = useState(() => loadDraft(draftKey));
  const [shownKey, setShownKey] = useState(draftKey);
  const [sending, setSending] = useState(false);
  const input = useRef<HTMLTextAreaElement>(null);
  // The prefill last applied, so one prefill fills the box once even while its caller re-renders.
  const appliedPrefill = useRef<string | undefined>(undefined);
  // Set when a prefill lands, so the caret moves to its end once the box can take focus.
  const caretToEnd = useRef(false);

  const listboxId = useId();
  const [caret, setCaret] = useState(0);
  const [highlight, setHighlight] = useState(0);
  const [dismissed, setDismissed] = useState(false);
  // Set when a command is accepted, so the caret lands just after the inserted completion.
  const pendingCaret = useRef<number | null>(null);
  // The active token's query last seen, so the highlight resets and a dismissal lifts only when it
  // actually changes, not on every render.
  const prevQuery = useRef<string | null>(null);

  const token = commands?.length ? activeSlashToken(text, caret) : null;
  // Adjusted while rendering, not in an effect, so the dropdown never flashes its old selection or a
  // dismissal that no longer applies.
  let dismissedNow = dismissed;
  if (token) {
    if (token.query !== prevQuery.current) {
      prevQuery.current = token.query;
      if (highlight !== 0) setHighlight(0);
      if (dismissed) {
        setDismissed(false);
        dismissedNow = false;
      }
    }
  } else if (prevQuery.current !== null) {
    prevQuery.current = null;
    if (dismissed) {
      setDismissed(false);
      dismissedNow = false;
    }
  }
  const matches = token ? matchSlashCommands(commands ?? [], token.query) : [];
  const menuOpen = token !== null && matches.length > 0 && !dismissedNow;
  const highlightIndex = Math.min(highlight, Math.max(matches.length - 1, 0));

  // A different conversation shows its own draft. Adjusted while rendering, not in an effect, so
  // the old text never flashes under the new key.
  if (shownKey !== draftKey) {
    setShownKey(draftKey);
    const stored = loadDraft(draftKey);
    if (shownKey === NEW_CONVERSATION && text) {
      // The main thread has no conversation id until its first read. Text typed or prefilled before
      // then moves to the conversation's draft, ahead of any draft it already holds.
      const carried = stored ? text + stored : text;
      clearDraft(shownKey);
      saveDraft(draftKey, carried);
      setText(carried);
    } else {
      setText(stored);
    }
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
    // A draft that already starts with this prefill (New task clicked twice) is left as it is.
    const next = existing.startsWith(prefill)
      ? existing
      : existing.trim()
        ? prefill + existing
        : prefill;
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

  // The caret goes just past an accepted command's trailing space, so the owner can type its
  // arguments straight away.
  useEffect(() => {
    const el = input.current;
    if (pendingCaret.current === null || !el) return;
    const pos = pendingCaret.current;
    pendingCaret.current = null;
    el.focus();
    el.setSelectionRange(pos, pos);
    setCaret(pos);
  }, [text]);

  function change(next: string) {
    setText(next);
    saveDraft(draftKey, next);
  }

  function acceptCommand(command: SlashCommandSpec) {
    if (!token) return;
    const insertion = `/${command.name} `;
    pendingCaret.current = insertion.length;
    change(insertion + text.slice(token.end));
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
    if (menuOpen) {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        setHighlight((highlightIndex + 1) % matches.length);
        return;
      }
      if (event.key === 'ArrowUp') {
        event.preventDefault();
        setHighlight((highlightIndex - 1 + matches.length) % matches.length);
        return;
      }
      if (event.key === 'Tab') {
        event.preventDefault();
        acceptCommand(matches[highlightIndex]!);
        return;
      }
      if (event.key === 'Escape') {
        event.preventDefault();
        setDismissed(true);
        return;
      }
      // A name already typed exactly falls through to the ordinary Enter-sends behaviour below,
      // rather than re-accepting a completion that would change nothing.
      const exact = matches.some((command) => command.name === token?.query.toLowerCase());
      if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && !exact) {
        event.preventDefault();
        acceptCommand(matches[highlightIndex]!);
        return;
      }
    }
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    void submit();
  }

  return (
    <div
      className="border-border bg-background px-md py-sm gap-sm relative flex shrink-0 items-end border-t"
      data-testid="input-bar"
      aria-busy={sending || busy || turnBusy}
    >
      {menuOpen ? (
        <ul
          id={listboxId}
          role="listbox"
          aria-label="Slash commands"
          data-testid="slash-command-menu"
          className="border-border bg-popover text-popover-foreground absolute inset-x-0 bottom-full z-10 mb-1 max-h-48 overflow-auto border text-sm shadow-md"
        >
          {matches.map((command, index) => (
            <li
              key={command.name}
              id={`${listboxId}-${command.name}`}
              role="option"
              aria-selected={index === highlightIndex}
              data-testid={`slash-command-option-${command.name}`}
              className={cn(
                'px-md gap-sm flex min-h-11 cursor-pointer items-baseline lg:min-h-7',
                index === highlightIndex && 'bg-accent text-accent-foreground'
              )}
              // A press here must not blur the textarea before the completion lands.
              onMouseDown={(event) => {
                event.preventDefault();
                acceptCommand(command);
              }}
              onMouseEnter={() => setHighlight(index)}
            >
              <span className="text-term-accent shrink-0">/{command.name}</span>
              <span className="text-muted-foreground truncate">{command.summary}</span>
            </li>
          ))}
        </ul>
      ) : null}
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
          busy || turnBusy,
          loading,
          unavailable,
          closed ? closedPlaceholder : null
        )}
        aria-label="Message"
        aria-controls={menuOpen ? listboxId : undefined}
        aria-activedescendant={
          menuOpen ? `${listboxId}-${matches[highlightIndex]!.name}` : undefined
        }
        onChange={(event) => {
          change(event.target.value);
          setCaret(event.target.selectionStart ?? event.target.value.length);
        }}
        onSelect={(event) => setCaret(event.currentTarget.selectionStart ?? 0)}
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
