import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AttentionRows } from './AttentionList';
import { attentionItem, queueItem } from './test/fixtures';

// The panel reads the API and needs a router, so it has no static preview. This previews the
// Needs-you rows it renders, which is where the owner's decisions happen.
const client = new QueryClient();

const question = attentionItem(queueItem({ options: ['main', 'release'] }));
const notice = attentionItem(
  queueItem({
    id: 'n-1',
    item_type: 'hitl_request',
    title: 'Input notice unavailable',
    content: 'The request this notice pointed at no longer exists.'
  })
);

/** Needs you in its states: nothing pending, one question, and a list with a notice. */
export default function InboxPanelPreview() {
  return (
    <QueryClientProvider client={client}>
      <div className="gap-md p-md grid">
        <section aria-label="Empty Needs you">
          <AttentionRows items={[]} />
        </section>
        <section aria-label="One question">
          <AttentionRows items={[question]} />
        </section>
        <section aria-label="Two items">
          <AttentionRows items={[question, notice]} />
        </section>
      </div>
    </QueryClientProvider>
  );
}
