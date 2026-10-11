import { useNavigate } from '@tanstack/react-router';
import { useId, useState, type ReactNode } from 'react';
import { Button } from '@/components/ui/button';
import { type Choice, searchFrom, windowLabel } from './model';
import { HOUR_WINDOWS, type HealthSearch } from './search';

export interface HealthFiltersProps {
  id: string;
  /** The applied filters, from the URL. */
  search: HealthSearch;
  projects: Choice[];
  tasks: Choice[];
  sessions: Choice[];
  attempts: Choice[];
}

/**
 * The filter form. Edits stay in a draft until Apply, which writes them to the URL. The parent
 * keys this form on the applied search, so a change from the URL starts the draft again.
 */
export function HealthFilters({
  id,
  search,
  projects,
  tasks,
  sessions,
  attempts
}: HealthFiltersProps) {
  const navigate = useNavigate();
  const [draft, setDraft] = useState<HealthSearch>(search);
  const set = <Key extends keyof HealthSearch>(key: Key, value: HealthSearch[Key]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  return (
    <form
      id={id}
      aria-label="Health filters"
      onSubmit={(event) => {
        event.preventDefault();
        void navigate({ to: '/health', search: searchFrom(draft) });
      }}
      className="gap-md mt-md grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6"
    >
      <SelectField
        label="Window"
        value={String(draft.hours ?? 24)}
        onChange={(value) => set('hours', Number(value))}
      >
        {HOUR_WINDOWS.map((hours) => (
          <option key={hours} value={hours}>
            {windowLabel(hours)}
          </option>
        ))}
      </SelectField>
      <SelectField
        label="Project"
        value={draft.project_id ?? ''}
        onChange={(value) => set('project_id', value)}
      >
        <option value="">All projects</option>
        <Options choices={projects} />
      </SelectField>
      <SelectField
        label="Task"
        value={draft.task_id ?? ''}
        onChange={(value) => set('task_id', value)}
      >
        <option value="">All tasks</option>
        <Options choices={tasks} />
      </SelectField>
      <SelectField
        label="Session"
        value={draft.session_id ?? ''}
        onChange={(value) => set('session_id', value)}
      >
        <option value="">All sessions</option>
        <Options choices={sessions} />
      </SelectField>
      <SelectField
        label="Attempt"
        value={draft.attempt_id ?? ''}
        onChange={(value) => set('attempt_id', value)}
      >
        <option value="">All attempts</option>
        <Options choices={attempts} />
      </SelectField>
      <SelectField
        label="Provider"
        value={draft.provider ?? ''}
        onChange={(value) =>
          set('provider', value === 'claude' || value === 'codex' ? value : undefined)
        }
      >
        <option value="">All providers</option>
        <option value="claude">Claude</option>
        <option value="codex">Codex</option>
      </SelectField>

      <div className="gap-sm col-span-2 flex flex-wrap sm:col-span-3 lg:col-span-6">
        <Button type="submit" variant="outline">
          Apply filters
        </Button>
        <Button
          type="button"
          variant="ghost"
          onClick={() => void navigate({ to: '/health', search: {} })}
        >
          Clear filters
        </Button>
      </div>
    </form>
  );
}

const SELECT_CLASS =
  'bg-background border-border text-foreground p-sm mt-xs min-h-11 w-full border text-sm rounded-none';

/** A label beside its select. The label is not wrapped around the select, so its name is just the label text. */
function SelectField({
  label,
  value,
  onChange,
  children
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  children: ReactNode;
}) {
  const id = useId();
  return (
    <div className="text-muted-foreground text-xs">
      <label htmlFor={id}>{label}</label>
      <select
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className={SELECT_CLASS}
      >
        {children}
      </select>
    </div>
  );
}

function Options({ choices }: { choices: Choice[] }) {
  return (
    <>
      {choices.map((choice) => (
        <option key={choice.value} value={choice.value}>
          {choice.label}
        </option>
      ))}
    </>
  );
}
