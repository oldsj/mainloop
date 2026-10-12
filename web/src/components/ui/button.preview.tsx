import { Button } from './button';

/** Enabled and disabled buttons side by side. A disabled primary is an outline with muted text, not an accent bar. */
export default function ButtonPreview() {
  return (
    <div className="gap-md p-md flex flex-wrap items-center">
      <Button>Save policy</Button>
      <Button disabled>Save policy</Button>
      <Button variant="outline">Retry</Button>
      <Button variant="outline" disabled>
        Retry
      </Button>
      <Button variant="destructive">Cancel task</Button>
      <Button variant="destructive" disabled>
        Cancel task
      </Button>
    </div>
  );
}
