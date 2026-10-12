import { Button } from '@/components/ui/button';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { useProjects } from '@/features/projects/queries';
import { useAsk } from './useExplorer';

/**
 * Suggestion chips for the empty main chat, shown inside the thread's empty state (the thread
 * decides when it is empty). Each one fills the composer, unsent, so the owner finishes the sentence.
 */
export function ChatStarters() {
  const ask = useAsk();
  const projects = keepValid(useProjects().data, projectRowSchema);

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
    <div
      role="group"
      aria-label="Suggestions"
      className="gap-xs px-md flex flex-wrap justify-center"
    >
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
