<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/stores';
  import { api, type HealthView, type HealthEvents, type Project } from '$lib/api';
  import HITLCard from '$lib/components/HITLCard.svelte';
  import NativeIdentityStrip from '$lib/components/NativeIdentityStrip.svelte';
  import { choiceScope } from '$lib/healthFilters';

  let view = $state<HealthView | null>(null);
  let choices = $state<HealthView['rows']>([]);
  let projects = $state<Project[]>([]);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let projectId = $state('');
  let taskId = $state('');
  let sessionId = $state('');
  let attemptId = $state('');
  let provider = $state('');
  let hours = $state(24);
  let observations = $state<Record<string, HealthEvents>>({});
  let observationErrors = $state<Record<string, string>>({});
  let pendingSession = $state<string | null>(null);
  let openRows = $state<Record<string, boolean>>({});
  let refreshVersion = 0;
  let choicesKey = '';
  let choicesVersion = 0;
  let viewportWidth = $state<number>();
  let filtersOpen = $state(false);
  const wide = $derived((viewportWidth ?? 0) >= 640);
  $effect(() => {
    filtersOpen = wide;
  });
  const rows = $derived(
    view?.rows.toSorted((a, b) => (b.totals.failures ?? -1) - (a.totals.failures ?? -1)) ?? []
  );
  let request: AbortController | null = null;
  const sessionChoices = $derived(
    choices.flatMap((row) => row.sessions).filter((session) => session.session_id)
  );
  const count = (value: number | null) => (value === null ? 'Unavailable' : value.toLocaleString());

  async function refresh() {
    refreshVersion++;
    request?.abort();
    const controller = new AbortController();
    request = controller;
    busy = true;
    error = null;
    observations = {};
    try {
      const filters = {
        project_id: projectId,
        task_id: taskId,
        session_id: sessionId,
        attempt_id: attemptId,
        provider,
        hours
      };
      const scope = choiceScope(filters);
      if (scope.narrowed && scope.key !== choicesKey) void loadChoices(scope);
      const result = await api.orchestrationHealth(filters, controller.signal);
      if (controller.signal.aborted) return;
      view = result;
      if (!scope.narrowed) {
        choicesVersion++;
        choices = result.rows;
        choicesKey = scope.key;
      }
    } catch (e) {
      if (!controller.signal.aborted)
        error = e instanceof Error ? e.message : 'Health could not be loaded.';
    } finally {
      if (!controller.signal.aborted) busy = false;
    }
  }

  async function loadChoices(scope: ReturnType<typeof choiceScope>) {
    const version = ++choicesVersion;
    try {
      const result = await api.orchestrationHealth(scope.filters);
      if (version !== choicesVersion) return;
      choices = result.rows;
      choicesKey = scope.key;
    } catch {
      /* The selected filters remain usable without the wider option list. */
    }
  }

  async function loadEvents(id: string) {
    const version = refreshVersion;
    pendingSession = id;
    observationErrors[id] = '';
    try {
      const result = await api.healthEvents(id, hours);
      if (version === refreshVersion) observations[id] = result;
    } catch {
      if (version === refreshVersion)
        observationErrors[id] = 'Observations could not be loaded. Try again.';
    } finally {
      if (version === refreshVersion) pendingSession = null;
    }
  }

  onMount(() => {
    const query = $page.url.searchParams;
    projectId = query.get('project_id') ?? '';
    taskId = query.get('task_id') ?? '';
    sessionId = query.get('session_id') ?? '';
    void refresh();
    void api
      .listProjects()
      .then((rows) => (projects = rows))
      .catch(() => {
        /* The scoped health read remains usable without project names. */
      });
    return () => request?.abort();
  });
</script>

<svelte:window bind:innerWidth={viewportWidth} />

<svelte:head><title>Orchestration health · Mainloop</title></svelte:head>

<main class="text-term-fg mx-auto w-full max-w-6xl p-4 sm:p-6">
  <a href="/" class="text-term-fg-muted hover:text-term-accent text-sm underline underline-offset-4"
    >Home</a
  >
  <header class="mt-4 flex flex-wrap items-start justify-between gap-3">
    <div>
      <h1 class="text-2xl font-medium">Orchestration health</h1>
      <p class="text-term-fg-muted mt-2 max-w-prose text-sm">
        Tool failures and reported tokens, alongside blocked claims and approval state.
      </p>
    </div>
    <button
      type="button"
      onclick={refresh}
      disabled={busy}
      class="border-term-border hover:border-term-accent border px-3 py-2 text-sm disabled:opacity-50"
      >{busy ? 'Refreshing…' : 'Refresh'}</button
    >
  </header>

  <details bind:open={filtersOpen} class="border-term-border my-6 border-y py-3">
    <summary
      class="focus-visible:outline-term-accent cursor-pointer text-sm focus-visible:outline focus-visible:outline-2"
      >Filters · {hours === 24
        ? 'last 24 hours'
        : hours === 168
          ? 'last 7 days'
          : hours === 720
            ? 'last 30 days'
            : 'last year'}</summary
    >
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void refresh();
      }}
      class="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6"
    >
      <label class="text-term-fg-muted text-xs"
        >Window
        <select
          bind:value={hours}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value={24}>Last 24 hours</option><option value={168}>Last 7 days</option><option
            value={720}>Last 30 days</option
          ><option value={8760}>Last year</option></select
        >
      </label>
      <label class="text-term-fg-muted text-xs"
        >Project
        <select
          bind:value={projectId}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value="">All projects</option>{#each projects as project}<option
              value={project.id}>{project.name}</option
            >{/each}{#if projectId && !projects.some((project) => project.id === projectId)}<option
              value={projectId}>Selected project</option
            >{/if}</select
        >
      </label>
      <label class="text-term-fg-muted text-xs"
        >Task
        <select
          bind:value={taskId}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value="">All tasks</option
          >{#each choices.filter((row) => row.task_id) as row}<option value={row.task_id ?? ''}
              >{row.title ?? row.task_id}</option
            >{/each}{#if taskId && !choices.some((row) => row.task_id === taskId)}<option
              value={taskId}>Selected task</option
            >{/if}</select
        >
      </label>
      <label class="text-term-fg-muted text-xs"
        >Session
        <select
          bind:value={sessionId}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value="">All sessions</option>{#each sessionChoices as session}<option
              value={session.session_id ?? ''}>{session.title ?? session.session_id}</option
            >{/each}{#if sessionId && !sessionChoices.some((session) => session.session_id === sessionId)}<option
              value={sessionId}>Selected session</option
            >{/if}</select
        >
      </label>
      <label class="text-term-fg-muted text-xs"
        >Attempt
        <select
          bind:value={attemptId}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value="">All attempts</option
          >{#each sessionChoices.filter((session) => session.attempt_id) as session}<option
              value={session.attempt_id ?? ''}
              >{session.title} · {session.attempt_id?.slice(0, 8)}</option
            >{/each}</select
        >
      </label>
      <label class="text-term-fg-muted text-xs"
        >Provider
        <select
          bind:value={provider}
          class="bg-term-bg border-term-border text-term-fg mt-1 w-full border p-2 text-sm"
          ><option value="">All providers</option><option value="claude">Claude</option><option
            value="codex">Codex</option
          ></select
        >
      </label>
      <button
        type="submit"
        disabled={busy}
        class="border-term-border hover:border-term-accent col-span-2 border px-3 py-2 text-sm disabled:opacity-50 sm:col-span-3 lg:col-span-6"
        >Apply filters</button
      >
    </form>
  </details>

  <div aria-live="polite">
    {#if error}<p role="alert" class="text-term-red mb-4">{error} Use Refresh to try again.</p>
    {:else if busy}<p class="text-term-fg-muted mb-4">Loading observations…</p>{/if}
  </div>
  {#if view}
    <p class="text-term-fg-muted mb-4 max-w-prose text-sm">
      Totals use each task’s reported root-session usage. Product child tasks appear separately.
      Native child attribution, cache tokens, Codex usage and exact approval waits are unavailable.
    </p>
    {#if view.partial}<p class="text-term-yellow mb-4" role="status">
        Collection is partial: this window exceeds the recovery or display limit. Filter to a
        session to inspect it.
      </p>{/if}
    {#if !view.rows.length}<p class="text-term-fg-muted py-8">
        No tasks or sessions match these filters. Try another window or clear the filters.
      </p>{/if}
    <ul class="border-term-border divide-term-border divide-y border-y">
      {#each rows as row (row.id)}
        <li class="py-5">
          <details bind:open={openRows[row.id]}>
            <summary
              class="focus-visible:outline-term-accent cursor-pointer list-none focus-visible:outline focus-visible:outline-2"
            >
              <span class="flex flex-wrap items-center justify-between gap-2">
                <span class="font-medium">{row.title ?? row.task_id ?? 'Unassigned'}</span>
                <span class="text-term-fg-muted text-xs"
                  >{row.totals.coverage === 'complete'
                    ? 'Observed coverage'
                    : row.totals.coverage === 'partial'
                      ? 'Partial coverage'
                      : 'Unknown coverage'} · Details</span
                >
              </span>
              <span class="mt-3 grid grid-cols-2 gap-x-4 gap-y-2 text-sm sm:grid-cols-4">
                <span
                  >Failures <strong class="tabular-nums">{count(row.totals.failures)}</strong></span
                >
                <span
                  >Repeated groups <strong class="tabular-nums"
                    >{count(row.totals.repeated_failure_groups)}</strong
                  ></span
                >
                <span
                  >Input <strong class="tabular-nums">{count(row.totals.input_tokens)}</strong
                  ></span
                >
                <span
                  >Output <strong class="tabular-nums">{count(row.totals.output_tokens)}</strong
                  ></span
                >
              </span>
              <span class="text-term-fg-muted mt-2 block text-xs"
                >Usage observed on {row.totals.usage_observed_turns}/{row.totals.turns} observed turns
                · {row.expected_receipts} delivery receipts · {row.reports.length} blocked/failed claims
                · {row.approvals.filter((approval) => approval.state === 'pending').length} pending approvals</span
              >
            </summary>
            {#if openRows[row.id]}
              <div class="mt-4 space-y-4 text-sm">
                {#if row.task_id}<a
                    href="/tasks/{row.task_id}"
                    class="text-term-accent underline underline-offset-4">Open task</a
                  >{/if}
                {#if row.gaps.length}<p class="text-term-yellow">
                    Collection gaps: {row.gaps.map((gap) => gap.replaceAll('_', ' ')).join(', ')}.
                  </p>{/if}
                {#each row.reports as report}<p class="text-term-fg-muted">
                    Reported {report.outcome} · attempt {report.attempt_id} · report {report.id}.
                    Agent claim; runtime failures are counted separately.
                  </p>{/each}
                {#each row.approvals as approval}<div>
                    <p class="text-term-fg-muted mb-2">
                      Approval {approval.state}; exact wait unavailable.
                    </p>
                    <HITLCard requestId={approval.id} />
                  </div>{/each}
                {#each row.sessions.filter((session) => session.session_id) as session}
                  {@const id = session.session_id!}
                  <section
                    aria-label={`Session ${session.title ?? id}`}
                    class="border-term-border border-t pt-4"
                  >
                    <a
                      href="/sessions/{id}"
                      class="text-term-accent break-words underline underline-offset-4"
                      >{session.title ?? id}</a
                    >
                    <p class="text-term-fg-muted my-2">
                      {session.provider} · input {count(session.totals.input_tokens)} · output {count(
                        session.totals.output_tokens
                      )} · failures {count(session.totals.failures)}
                    </p>
                    <NativeIdentityStrip sessionId={id} />
                    <button
                      type="button"
                      onclick={() => loadEvents(id)}
                      disabled={pendingSession === id}
                      class="border-term-border hover:border-term-accent my-3 border px-3 py-2 disabled:opacity-50"
                      >{pendingSession === id ? 'Loading…' : 'Load source observations'}</button
                    >
                    {#if observationErrors[id]}<p role="alert" class="text-term-red">
                        {observationErrors[id]}
                      </p>{/if}
                    {#if observations[id]}
                      {#if observations[id].partial}<p class="text-term-yellow">
                          Observation list is partial.
                        </p>{/if}
                      {#if !observations[id].events.length}<p class="text-term-fg-muted">
                          No retained source events in this window.
                        </p>{/if}
                      <ol class="space-y-2">
                        {#each observations[id].events as event}<li
                            class="text-term-fg-muted text-xs break-words"
                          >
                            {event.payload.kind} · sequence {event.payload.sequence} · {event
                              .payload.source}<br />{event.event_id}{#if event.payload.tool}<br
                              />{event.payload.tool.tool_class} · {event.payload.tool.outcome} · {event
                                .payload.tool.category}{#if event.payload.tool.path}
                                · {event.payload.tool.path}{/if}{/if}
                          </li>{/each}
                      </ol>
                    {/if}
                  </section>
                {/each}
              </div>
            {/if}
          </details>
        </li>
      {/each}
    </ul>
    <p class="text-term-fg-muted mt-5 text-xs">
      Source detail retained {view.retention.events_days} days; frozen, path-free summaries {view
        .retention.summaries_days} days.
    </p>
  {/if}
</main>
