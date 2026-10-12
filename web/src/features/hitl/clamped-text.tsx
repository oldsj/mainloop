import { useState } from 'react';
import { Button } from '@/components/ui/button';

/** Lines shown before "Show all". */
export const CLAMP_LINES = 12;
/** Characters shown before "Show all", for text with few but long lines. */
export const CLAMP_CHARS = 1200;

/**
 * Preformatted text that grows with the page instead of scrolling inside it. Long text shows its
 * start and a toggle for the rest, so a card never holds a scroller of its own.
 */
export function ClampedText({ text, what }: { text: string; what: string }) {
  const [open, setOpen] = useState(false);
  const lines = text.split('\n');
  const long = lines.length > CLAMP_LINES || text.length > CLAMP_CHARS;
  const shown =
    long && !open ? `${lines.slice(0, CLAMP_LINES).join('\n').slice(0, CLAMP_CHARS)}…` : text;
  return (
    <div className="gap-xs grid min-w-0">
      <pre className="bg-muted p-sm [overflow-wrap:anywhere] break-words whitespace-pre-wrap">
        {shown}
      </pre>
      {long ? (
        <Button
          variant="ghost"
          size="sm"
          className="justify-self-start"
          aria-expanded={open}
          onClick={() => setOpen((value) => !value)}
        >
          {open ? `Show less of the ${what}` : `Show all of the ${what}`}
        </Button>
      ) : null}
    </div>
  );
}
