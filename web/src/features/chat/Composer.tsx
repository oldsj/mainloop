import { X } from 'lucide-react';
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ClipboardEvent,
  type DragEvent,
  type KeyboardEvent
} from 'react';
import { Button } from '@/components/ui/button';
import { clearDraft, loadDraft, NEW_CONVERSATION, saveDraft } from './drafts';
import {
  fileToPendingImage,
  imageDataUrl,
  isAllowedImageType,
  MAX_IMAGE_BYTES,
  type AllowedImageType,
  type PendingImage
} from './imageLimits';

const MAX_HEIGHT_PX = 160;
/** Anthropic's Messages API accepts at most this many images in one request. */
const MAX_IMAGES = 20;

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
   * Sends the text, and any pasted images. Resolves true once the backend recorded the message;
   * false puts the text back in the box. The caller clears the stored draft on success.
   */
  onSend: (text: string, images?: PendingImage[]) => Promise<boolean>;
  /**
   * Text to put in the box once, unsent, with the caret at its end. The box keeps any text already
   * there after it. The caller clears the prefill from its own state in `onPrefillUsed`.
   */
  prefill?: string;
  onPrefillUsed?: () => void;
  /**
   * Accepts a pasted or dropped image, read into memory and shown as a thumbnail before send. Only
   * the main chat sets this: a worker session's composer stays text-only.
   */
  allowImages?: boolean;
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

/** The text box and EXEC button. Enter sends, Shift+Enter adds a line, IME composition never sends. */
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
  allowImages = false
}: ComposerProps) {
  const [text, setText] = useState(() => loadDraft(draftKey));
  const [shownKey, setShownKey] = useState(draftKey);
  const [sending, setSending] = useState(false);
  const [images, setImages] = useState<PendingImage[]>([]);
  const [imageError, setImageError] = useState<string | null>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  // The prefill last applied, so one prefill fills the box once even while its caller re-renders.
  const appliedPrefill = useRef<string | undefined>(undefined);
  // Set when a prefill lands, so the caret moves to its end once the box can take focus.
  const caretToEnd = useRef(false);

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

  function change(next: string) {
    setText(next);
    saveDraft(draftKey, next);
  }

  /** Reads accepted image files into memory and adds them as pending attachments. */
  async function addFiles(files: File[]) {
    if (!allowImages || files.length === 0) return;
    const valid = files.filter(
      (file) => isAllowedImageType(file.type) && file.size <= MAX_IMAGE_BYTES
    );
    const room = Math.max(0, MAX_IMAGES - images.length);
    const accepted = valid.slice(0, room);
    if (accepted.length < files.length) {
      setImageError(
        valid.length < files.length
          ? 'Images must be PNG, JPEG, WebP or GIF, up to 5MB.'
          : `Up to ${MAX_IMAGES} images per message.`
      );
    } else {
      setImageError(null);
    }
    if (accepted.length === 0) return;
    const read = await Promise.all(
      accepted.map((file) => fileToPendingImage(file, file.type as AllowedImageType))
    );
    setImages((current) => [...current, ...read]);
  }

  function removeImage(index: number) {
    setImages((current) => current.filter((_, i) => i !== index));
  }

  function paste(event: ClipboardEvent<HTMLTextAreaElement>) {
    if (!allowImages) return;
    const files = Array.from(event.clipboardData.items)
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((file): file is File => file !== null);
    if (files.length === 0) return;
    // Only an image paste is intercepted; a plain text paste still inserts normally.
    event.preventDefault();
    void addFiles(files);
  }

  function drop(event: DragEvent<HTMLDivElement>) {
    if (!allowImages) return;
    const files = Array.from(event.dataTransfer.files).filter((file) =>
      file.type.startsWith('image/')
    );
    if (files.length === 0) return;
    event.preventDefault();
    void addFiles(files);
  }

  function dragOver(event: DragEvent<HTMLDivElement>) {
    if (allowImages) event.preventDefault();
  }

  async function submit() {
    const message = text.trim();
    if ((!message && images.length === 0) || disabled) return;
    setSending(true);
    setText('');
    const sentImages = images;
    setImages([]);
    setImageError(null);
    let accepted = false;
    try {
      accepted = sentImages.length > 0 ? await onSend(message, sentImages) : await onSend(message);
    } finally {
      setSending(false);
    }
    // The caller keeps the stored draft until the backend accepts; on refusal the text and the
    // images both return.
    if (!accepted) {
      setText((current) => current || message);
      setImages((current) => (current.length > 0 ? current : sentImages));
    }
  }

  function keyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    void submit();
  }

  return (
    <div className="border-border bg-background border-t">
      {allowImages && images.length > 0 ? (
        <div className="gap-xs px-md pt-sm flex flex-wrap" data-testid="composer-images">
          {images.map((image, index) => (
            <div key={index} className="relative">
              <img
                src={imageDataUrl(image)}
                alt=""
                className="border-border size-14 border object-cover"
                data-testid="composer-image-thumbnail"
              />
              <button
                type="button"
                onClick={() => removeImage(index)}
                aria-label="Remove image"
                data-testid="composer-image-remove"
                className="bg-background border-border text-foreground absolute -top-2 -right-2 flex size-5 items-center justify-center border"
              >
                <X className="size-3" aria-hidden="true" />
              </button>
            </div>
          ))}
        </div>
      ) : null}
      {imageError ? (
        <p
          className="text-destructive px-md pt-xs text-xs"
          role="alert"
          data-testid="composer-image-error"
        >
          {imageError}
        </p>
      ) : null}
      <div
        className="px-md py-sm gap-sm relative flex shrink-0 items-end"
        data-testid="input-bar"
        aria-busy={sending || busy || turnBusy}
        onDrop={drop}
        onDragOver={dragOver}
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
            busy || turnBusy,
            loading,
            unavailable,
            closed ? closedPlaceholder : null
          )}
          aria-label="Message"
          onChange={(event) => change(event.target.value)}
          onKeyDown={keyDown}
          onPaste={paste}
          data-testid="command-input"
          className="border-input bg-background text-foreground placeholder:text-muted-foreground focus-visible:ring-ring px-sm py-sm min-h-11 min-w-0 flex-1 resize-none border text-sm outline-none focus-visible:ring-1 disabled:opacity-50"
        />
        <Button
          disabled={disabled || (!text.trim() && images.length === 0)}
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
    </div>
  );
}
