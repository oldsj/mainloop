import { Link } from '@tanstack/react-router';
import { useState } from 'react';
import { Button, buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { failureText, projectRowSchema, type ProjectRow } from '@/features/projects/model';
import { useImportProject } from '@/features/projects/queries';

/**
 * Adds a GitHub repository as a project. The server parses `owner/name` or a GitHub URL, and an
 * import of a repository that is already a project returns that project. Import starts no
 * workspace or session; the project page is where its settings are made.
 */
export function ImportProjectSection() {
  const importProject = useImportProject();
  const [repo, setRepo] = useState('');
  const [imported, setImported] = useState<ProjectRow | null>(null);
  const [contractError, setContractError] = useState(false);
  const trimmed = repo.trim();

  const submit = () => {
    setImported(null);
    setContractError(false);
    importProject.mutate(
      { body: { repo: trimmed } },
      {
        onSuccess: (data) => {
          const parsed = projectRowSchema.safeParse(data);
          if (parsed.success) {
            setImported(parsed.data);
            setRepo('');
          } else {
            setContractError(true);
          }
        }
      }
    );
  };

  return (
    <section
      id="import-project"
      aria-labelledby="import-project-heading"
      className="gap-sm flex flex-col"
      data-testid="import-project"
    >
      <h2 id="import-project-heading" className="text-title font-semibold">
        Import a project
      </h2>
      <p className="text-muted-foreground text-body">
        Add a GitHub repository that Mainloop&apos;s GitHub App is installed on. Importing starts no
        workspace or session; set the project up on its page afterwards.
      </p>
      <form
        className="gap-sm flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (trimmed && !importProject.isPending) submit();
        }}
      >
        <label className="gap-xs text-body flex flex-col">
          Repository
          <input
            className="border-border bg-background text-foreground px-md min-h-control text-field border"
            placeholder="owner/name or https://github.com/owner/name"
            value={repo}
            disabled={importProject.isPending}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            onChange={(event) => setRepo(event.target.value)}
          />
        </label>
        {importProject.error ? (
          <p role="alert" className="text-destructive text-body">
            {failureText(importProject.error)}
          </p>
        ) : null}
        {contractError ? (
          <p role="alert" className="text-destructive text-body">
            Mainloop imported the repository but sent a project that does not match the contract.
            Reload to see it.
          </p>
        ) : null}
        <Button type="submit" className="self-start" disabled={!trimmed || importProject.isPending}>
          {importProject.isPending ? 'Importing…' : 'Import'}
        </Button>
      </form>
      {imported ? (
        <div role="status" className="gap-sm text-body flex flex-col">
          <p>Imported {imported.full_name}.</p>
          <Link
            to="/projects/$id"
            params={{ id: imported.id }}
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }), 'self-start')}
          >
            Open {imported.full_name}
          </Link>
        </div>
      ) : null}
    </section>
  );
}
