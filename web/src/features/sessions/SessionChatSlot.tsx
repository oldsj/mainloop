/**
 * Where the session chat goes. F2 builds the chat and replaces this body. The page already passes
 * the session ID, so F2 does not need to change the page.
 */
export function SessionChatSlot({ sessionId }: { sessionId: string }) {
  return (
    <section
      data-testid="session-chat-slot"
      data-session-id={sessionId}
      aria-label="Session chat"
      className="gap-xs border-border p-md text-muted-foreground flex min-h-24 flex-col justify-center border border-dashed text-sm"
    >
      Session chat isn&apos;t available yet.
    </section>
  );
}
