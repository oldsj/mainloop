import { useEffect, useState } from 'react';
import { Button } from '@/components/ui/button';

/** How many characters of an ID the page shows. Enough to tell two workspaces apart by eye. */
export const SHORT_ID_LENGTH = 8;

/** How long the copy result stays on the button before it goes back to "Copy". */
export const COPY_RESULT_MS = 3000;

type CopyState = 'idle' | 'copied' | 'failed';

const RESULT_TEXT: Record<CopyState, string> = {
  idle: '',
  copied: 'Copied the full ID',
  failed: "Couldn't copy the ID"
};

/**
 * An ID as its first 8 characters, with a copy button for the full value. The full ID is kept out
 * of the visible text, so a UUID does not take a line of the header. The result is announced to
 * screen readers and shown on the button for a few seconds.
 */
export function ShortId({ id, label }: { id: string; label: string }) {
  const [copy, setCopy] = useState<CopyState>('idle');

  // Each result goes back to idle after a few seconds, so the button reads "Copy" again.
  useEffect(() => {
    if (copy === 'idle') return;
    const timer = setTimeout(() => setCopy('idle'), COPY_RESULT_MS);
    return () => clearTimeout(timer);
  }, [copy]);

  async function copyFull() {
    try {
      await navigator.clipboard.writeText(id);
      setCopy('copied');
    } catch {
      setCopy('failed');
    }
  }
  return (
    <span className="gap-xs inline-flex max-w-full flex-wrap items-center font-mono text-xs">
      <code title={id}>{id.slice(0, SHORT_ID_LENGTH)}</code>
      <Button
        variant="outline"
        size="sm"
        className="min-h-11"
        aria-label={`Copy full ${label} ID`}
        onClick={() => void copyFull()}
      >
        {copy === 'copied' ? 'Copied' : copy === 'failed' ? 'Failed' : 'Copy'}
      </Button>
      <span role="status" aria-live="polite" className="sr-only">
        {RESULT_TEXT[copy]}
      </span>
    </span>
  );
}
