import { useState } from 'react';
import { Button } from '@/components/ui/button';

/** How many characters of an ID the page shows. Enough to tell two workspaces apart by eye. */
export const SHORT_ID_LENGTH = 8;

/**
 * An ID as its first 8 characters, with a copy button for the full value. The full ID is kept out
 * of the visible text, so a UUID does not take a line of the header.
 */
export function ShortId({ id, label }: { id: string; label: string }) {
  const [copy, setCopy] = useState<'idle' | 'copied' | 'failed'>('idle');
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
    </span>
  );
}
