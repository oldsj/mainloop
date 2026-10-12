import { $api } from '@/api';
import { Button } from '@/components/ui/button';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { useProjects } from '@/features/projects/queries';
import { useAsk } from './useExplorer';

/**
 * Suggestion chips for an empty main chat. Each one fills the composer, unsent, so the owner
 * finishes the sentence. The chips read the same cache entries as the thread, without a poll of
 * their own, so an empty check costs no extra requests. They render nothing until the thread is
 * known to be empty.
 */
export function ChatStarters() {
  const ask = useAsk();
  const mainThread = $api.useQuery('get', '/main-thread');
  const conversationId = mainThread.data?.conversation_id ?? null;
  const conversation = $api.useQuery(
    'get',
    '/conversations/{conversation_id}',
    { params: { path: { conversation_id: conversationId ?? '' } } },
    { enabled: conversationId !== null }
  );
  const projects = keepValid(useProjects().data, projectRowSchema);

  // No conversation yet means the thread is new. A loaded conversation with no messages is empty too.
  const empty =
    mainThread.data !== undefined &&
    (conversationId === null ||
      (conversation.data !== undefined && conversation.data.messages.length === 0));
  if (!empty) return null;

  const chips = [
    { key: 'needs', label: 'What needs me?', text: 'What needs me?' },
    ...(projects[0]
      ? [
          {
            key: 'start',
            label: `Start a task in ${projects[0].full_name}`,
            text: `Start a task in ${projects[0].full_name}: `
          }
        ]
      : []),
    { key: 'running', label: "What's running?", text: "What's running?" },
    { key: 'pause', label: 'Pause a schedule', text: 'Pause a schedule: ' }
  ];

  return (
    <div role="group" aria-label="Suggestions" className="gap-xs px-md py-sm flex flex-wrap">
      {chips.map((chip) => (
        <Button
          key={chip.key}
          type="button"
          variant="outline"
          size="sm"
          onClick={() => ask(chip.text)}
        >
          {chip.label}
        </Button>
      ))}
    </div>
  );
}
