import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { QueueCard } from './QueueCard';
import { queueItem } from './test/fixtures';

// The card's actions use mutations, so it needs a client. The preview never sends them.
const client = new QueryClient();

/** The queue card in the states an owner meets: options, a reply, a plan, a notice and an answer. */
export default function QueueCardPreview() {
  return (
    <QueryClientProvider client={client}>
      <div className="gap-md p-md grid">
        <QueueCard item={queueItem({ options: ['main', 'release'] })} />
        <QueueCard
          item={queueItem({
            id: 'q-2',
            title: 'Which tests should run?',
            content: 'The branch touches the inbox and the shell. Say which suites to run.'
          })}
        />
        <QueueCard
          item={queueItem({
            id: 'p-1',
            item_type: 'plan_review',
            priority: 'high',
            title: 'Review the migration plan',
            content: 'Step 1: move the inbox. Step 2: mount it in the shell.',
            context: { pr_url: 'https://github.com/owner/repo/pull/7' }
          })}
        />
        <QueueCard
          item={queueItem({
            id: 'n-1',
            item_type: 'hitl_request',
            priority: 'urgent',
            title: 'Input notice unavailable',
            content: 'The request this notice pointed at no longer exists.'
          })}
        />
        <QueueCard
          item={queueItem({
            id: 'q-3',
            title: 'Which branch?',
            content: 'Pick a branch to continue from.',
            status: 'responded',
            response: 'main'
          })}
        />
      </div>
    </QueryClientProvider>
  );
}
