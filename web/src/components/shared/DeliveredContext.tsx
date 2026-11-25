/**
 * The context Mainloop put in front of a message it delivered (see `splitDelivered`): one muted
 * line that opens to the text, so the transcript leads with what was written, not with what was added.
 */
export function DeliveredContext({ text }: { text: string }) {
  return (
    <details className="text-term-fg-muted text-caption" data-testid="delivered-context">
      <summary className="py-xs cursor-pointer">Context Mainloop sent with this message</summary>
      <p className="pb-xs [overflow-wrap:anywhere] whitespace-pre-wrap">{text}</p>
    </details>
  );
}
