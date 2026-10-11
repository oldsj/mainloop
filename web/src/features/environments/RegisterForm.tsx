import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { canRegister, IMAGE_PATTERN, type Architecture, type RegisterDraft } from './model';

interface RegisterFormProps {
  busy: boolean;
  uncertain: boolean;
  error: string | null;
  onSubmit: (draft: RegisterDraft) => void;
}

const EMPTY: RegisterDraft = { name: '', image: '', architecture: 'amd64' };

/**
 * Registers a public image. The parent owns the request and the lock, so an uncertain reply keeps
 * the form locked even though this form stays mounted.
 */
export function RegisterForm({ busy, uncertain, error, onSubmit }: RegisterFormProps) {
  const [draft, setDraft] = useState<RegisterDraft>(EMPTY);
  const ready = canRegister(draft, busy, uncertain);

  return (
    <details className="border-border p-md border">
      <summary className="py-sm min-h-11 cursor-pointer text-sm font-medium">
        Register a public image
      </summary>
      <form
        className="gap-md pt-md flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (ready) onSubmit(draft);
        }}
      >
        <label className="gap-xs flex flex-col text-sm">
          Name
          <input
            aria-label="Name"
            className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
            maxLength={200}
            required
            value={draft.name}
            onChange={(event) => setDraft({ ...draft, name: event.target.value })}
          />
        </label>

        <label className="gap-xs flex flex-col text-sm">
          Image
          <input
            aria-label="Image"
            className="border-border bg-background text-foreground px-md min-h-11 border font-mono text-sm"
            required
            pattern={IMAGE_PATTERN}
            placeholder="registry/repository@sha256:digest"
            value={draft.image}
            onChange={(event) => setDraft({ ...draft, image: event.target.value })}
          />
        </label>

        <label className="gap-xs flex flex-col text-sm">
          Architecture
          <select
            aria-label="Architecture"
            className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
            value={draft.architecture}
            onChange={(event) =>
              setDraft({ ...draft, architecture: event.target.value as Architecture })
            }
          >
            <option value="amd64">amd64</option>
            <option value="arm64">arm64</option>
          </select>
        </label>

        {uncertain ? (
          <p role="alert" className="text-term-yellow text-sm">
            Registration reply was not confirmed. Refresh the list and reconcile it before
            registering again.
          </p>
        ) : null}
        {error ? (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        ) : null}

        <Button type="submit" disabled={!ready}>
          {busy ? 'Registering…' : 'Register and validate image'}
        </Button>
      </form>
    </details>
  );
}
