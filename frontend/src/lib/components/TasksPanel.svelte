<script lang="ts">
  import { tasks, tasksList, isTasksOpen, activeTasksCount } from '$lib/stores/tasks';
  import { inbox, inboxItems, unreadCount } from '$lib/stores/inbox';
  import type { QueueItem, WorkerTask, TaskQuestion } from '$lib/api';
  import LogViewer from './LogViewer.svelte';
  import { marked } from 'marked';

  // Configure marked for terminal aesthetic
  marked.setOptions({ breaks: true, gfm: true });

  // Auto-focus action for inputs
  function autofocus(node: HTMLInputElement) {
    // Small delay to ensure DOM is ready
    setTimeout(() => node.focus(), 50);
    return {};
  }

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();

  let expandedTaskIds = $state<Set<string>>(new Set());
  let autoExpandedTaskIds = $state<Set<string>>(new Set());  // Track which tasks we've auto-expanded
  let respondingItemId = $state<string | null>(null);
  let customResponses = $state<Record<string, string>>({});
  let showRecent = $state(false);

  // Auto-expand tasks that need attention (but only once, so user can collapse them)
  $effect(() => {
    const tasksNeedingAttention = $tasksList.filter(t => needsAttention(t));
    for (const task of tasksNeedingAttention) {
      if (!autoExpandedTaskIds.has(task.id)) {
        // First time seeing this task need attention - auto-expand it
        expandedTaskIds = new Set([...expandedTaskIds, task.id]);
        autoExpandedTaskIds = new Set([...autoExpandedTaskIds, task.id]);
      }
    }
  });

  // State for task interactions (questions and plan reviews)
  let selectedAnswers = $state<Record<string, Record<string, string>>>({});  // taskId -> questionId -> answer
  let customQuestionInputs = $state<Record<string, Record<string, string>>>({});  // taskId -> questionId -> custom text
  let editingQuestionId = $state<Record<string, string | null>>({});  // taskId -> currently editing question id
  let planRevisionText = $state<Record<string, string>>({});  // taskId -> revision text
  let submittingTaskId = $state<string | null>(null);

  // Get which question should be shown expanded (editing or first unanswered)
  function getActiveQuestionId(task: WorkerTask): string | null {
    if (!task.pending_questions?.length) return null;

    // If user is editing a specific question, show that
    if (editingQuestionId[task.id]) return editingQuestionId[task.id];

    // Otherwise find first unanswered question
    for (const q of task.pending_questions) {
      if (!getAnswer(task.id, q.id)) return q.id;
    }

    // All answered - show none expanded (ready to submit)
    return null;
  }

  // When user selects an answer, auto-advance to next question
  function selectOptionAndAdvance(taskId: string, questionId: string, option: string) {
    selectOption(taskId, questionId, option);
    // Clear editing state to auto-advance to next unanswered
    editingQuestionId[taskId] = null;
  }

  // Helper to check if task needs attention (questions, plan review, or ready to implement)
  function needsAttention(task: WorkerTask): boolean {
    return task.status === 'waiting_questions' || task.status === 'waiting_plan_review' || task.status === 'ready_to_implement';
  }

  // Get answer for a question
  function getAnswer(taskId: string, questionId: string): string | null {
    return selectedAnswers[taskId]?.[questionId] || customQuestionInputs[taskId]?.[questionId] || null;
  }

  // Select an option for a question
  function selectOption(taskId: string, questionId: string, option: string) {
    if (!selectedAnswers[taskId]) {
      selectedAnswers[taskId] = {};
    }
    selectedAnswers[taskId][questionId] = option;
    // Clear custom input when selecting option
    if (customQuestionInputs[taskId]?.[questionId]) {
      customQuestionInputs[taskId][questionId] = '';
    }
  }

  // Set custom answer for a question
  function setCustomAnswer(taskId: string, questionId: string, text: string) {
    if (!customQuestionInputs[taskId]) {
      customQuestionInputs[taskId] = {};
    }
    customQuestionInputs[taskId][questionId] = text;
    // Clear selected option when typing custom
    if (text && selectedAnswers[taskId]?.[questionId]) {
      delete selectedAnswers[taskId][questionId];
    }
  }

  // Check if all questions have answers
  function allQuestionsAnswered(task: WorkerTask): boolean {
    if (!task.pending_questions) return false;
    return task.pending_questions.every((q) => getAnswer(task.id, q.id));
  }

  // Submit all answers for a task
  async function submitAnswers(taskId: string) {
    submittingTaskId = taskId;
    try {
      const answers: Record<string, string> = {};
      const taskAnswers = selectedAnswers[taskId] || {};
      const taskCustom = customQuestionInputs[taskId] || {};

      // Merge selected options and custom inputs
      for (const [qId, answer] of Object.entries(taskAnswers)) {
        answers[qId] = answer;
      }
      for (const [qId, answer] of Object.entries(taskCustom)) {
        if (answer) answers[qId] = answer;
      }

      await tasks.answerQuestions(taskId, answers);
      // Clear local state
      delete selectedAnswers[taskId];
      delete customQuestionInputs[taskId];
    } catch (e) {
      console.error('Failed to submit answers:', e);
    } finally {
      submittingTaskId = null;
    }
  }

  // Approve plan
  async function handleApprovePlan(taskId: string) {
    submittingTaskId = taskId;
    try {
      await tasks.approvePlan(taskId);
      delete planRevisionText[taskId];
    } catch (e) {
      console.error('Failed to approve plan:', e);
    } finally {
      submittingTaskId = null;
    }
  }

  // Revise plan
  async function handleRevisePlan(taskId: string) {
    const feedback = planRevisionText[taskId]?.trim();
    if (!feedback) return;

    submittingTaskId = taskId;
    try {
      await tasks.revisePlan(taskId, feedback);
      delete planRevisionText[taskId];
    } catch (e) {
      console.error('Failed to revise plan:', e);
    } finally {
      submittingTaskId = null;
    }
  }

  // Start implementation
  async function handleStartImplementation(taskId: string) {
    submittingTaskId = taskId;
    try {
      await tasks.startImplementation(taskId);
    } catch (e) {
      console.error('Failed to start implementation:', e);
    } finally {
      submittingTaskId = null;
    }
  }

  // Split tasks into active vs terminal
  const activeTasks = $derived(
    $tasksList
      .filter((t) => !['completed', 'failed', 'cancelled'].includes(t.status))
      .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
  );

  const terminalTasks = $derived(
    $tasksList
      .filter((t) => ['completed', 'failed', 'cancelled'].includes(t.status))
      .sort((a, b) => {
        const aTime = a.completed_at || a.created_at;
        const bTime = b.completed_at || b.created_at;
        return new Date(bTime).getTime() - new Date(aTime).getTime();
      })
  );

  // Recently completed (within last 30 minutes)
  const recentlyCompleted = $derived(
    terminalTasks.filter((t) => {
      const completedAt = t.completed_at ? new Date(t.completed_at) : new Date(t.created_at);
      const thirtyMinutesAgo = new Date(Date.now() - 30 * 60 * 1000);
      return t.status === 'completed' && completedAt > thirtyMinutesAgo;
    })
  );

  // Most recent failed job (always show if exists)
  const recentlyFailed = $derived(
    terminalTasks.filter((t) => t.status === 'failed').slice(0, 1)
  );

  // Combined for prominent display
  const recentlyFinished = $derived([...recentlyFailed, ...recentlyCompleted]);

  // Older terminal tasks (everything else)
  const olderTerminalTasks = $derived(
    terminalTasks.filter((t) => !recentlyFinished.includes(t))
  );

  // Count for badge - active tasks + inbox items
  const attentionCount = $derived($activeTasksCount + $unreadCount);

  function handleClose() {
    tasks.close();
  }

  function handleClickOutside(e: MouseEvent) {
    if (desktop || mobile) return;
    if (!$isTasksOpen) return;

    const target = e.target as HTMLElement;
    if (target.closest('.tasks-panel') || target.closest('[aria-label="Open tasks"]')) {
      return;
    }
    tasks.close();
  }

  function getStatusStyle(status: string): string {
    switch (status) {
      case 'pending':
        return 'border-term-warning text-term-warning';
      case 'planning':
      case 'implementing':
        return 'border-term-info text-term-info';
      case 'waiting_questions':
        return 'border-term-warning text-term-warning';  // No pulse - waiting on user, not working
      case 'waiting_plan_review':
      case 'under_review':
        return 'border-term-accent text-term-accent';
      case 'ready_to_implement':
        return 'border-term-accent-alt text-term-accent-alt';  // Plan approved, ready to go
      case 'completed':
        return 'border-term-accent-alt text-term-accent-alt';
      case 'failed':
        return 'border-term-error text-term-error';
      case 'cancelled':
        return 'border-term-fg-muted text-term-fg-muted';
      default:
        return 'border-term-fg-muted text-term-fg-muted';
    }
  }

  function getStatusLabel(status: string): string {
    switch (status) {
      case 'pending':
        return 'PENDING';
      case 'planning':
        return 'PLANNING';
      case 'waiting_questions':
        return 'NEEDS INPUT';  // Clear that system is waiting on user
      case 'waiting_plan_review':
        return 'REVIEW PLAN';  // Clear that system is waiting on user
      case 'ready_to_implement':
        return 'READY';  // Plan approved, ready to implement
      case 'implementing':
        return 'IMPLEMENTING';
      case 'under_review':
        return 'REVIEW';
      case 'completed':
        return 'DONE';
      case 'failed':
        return 'FAILED';
      case 'cancelled':
        return 'CANCELLED';
      default:
        return status.toUpperCase();
    }
  }

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (minutes < 1) return 'now';
    if (minutes < 60) return `${minutes}m`;
    if (hours < 24) return `${hours}h`;
    return `${days}d`;
  }

  function handleCancel(taskId: string) {
    if (confirm('Cancel this task?')) {
      tasks.cancelTask(taskId);
    }
  }

  function handleRetry(taskId: string) {
    tasks.retryTask(taskId);
  }

  function toggleExpand(taskId: string, status: string) {
    const terminalStatuses = ['completed', 'failed', 'cancelled'];
    if (terminalStatuses.includes(status)) return;

    const newSet = new Set(expandedTaskIds);
    if (newSet.has(taskId)) {
      newSet.delete(taskId);
    } else {
      newSet.add(taskId);
    }
    expandedTaskIds = newSet;
  }

  function expandAll() {
    const newSet = new Set<string>();
    for (const task of $tasksList) {
      if (isExpandable(task.status)) {
        newSet.add(task.id);
      }
    }
    expandedTaskIds = newSet;
  }

  function collapseAll() {
    expandedTaskIds = new Set();
  }

  function isExpandable(status: string): boolean {
    return !['completed', 'failed', 'cancelled'].includes(status);
  }

  // Inbox item helpers
  const priorityStyles: Record<string, string> = {
    urgent: 'border-l-term-error',
    high: 'border-l-term-warning',
    normal: 'border-l-term-info',
    low: 'border-l-term-fg-muted'
  };

  // Track expanded plan review items
  let expandedPlanId = $state<string | null>(null);

  const typeIcons: Record<string, string> = {
    plan_review:
      'M19.5 14.25v-2.625a3.375 3.375 0 0 0-3.375-3.375h-1.5A1.125 1.125 0 0 1 13.5 7.125v-1.5a3.375 3.375 0 0 0-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 0 0-9-9Z',
    plan_ready:
      'M9 12h3.75M9 15h3.75M9 18h3.75m3 .75H18a2.25 2.25 0 0 0 2.25-2.25V6.108c0-1.135-.845-2.098-1.976-2.192a48.424 48.424 0 0 0-1.123-.08m-5.801 0c-.065.21-.1.433-.1.664 0 .414.336.75.75.75h4.5a.75.75 0 0 0 .75-.75 2.25 2.25 0 0 0-.1-.664m-5.8 0A2.251 2.251 0 0 1 13.5 2.25H15c1.012 0 1.867.668 2.15 1.586m-5.8 0c-.376.023-.75.05-1.124.08C9.095 4.01 8.25 4.973 8.25 6.108V8.25m0 0H4.875c-.621 0-1.125.504-1.125 1.125v11.25c0 .621.504 1.125 1.125 1.125h9.75c.621 0 1.125-.504 1.125-1.125V9.375c0-.621-.504-1.125-1.125-1.125H8.25ZM6.75 12h.008v.008H6.75V12Zm0 3h.008v.008H6.75V15Zm0 3h.008v.008H6.75V18Z',
    code_ready: 'M17.25 6.75 22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3-4.5 16.5',
    question:
      'M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 5.25h.008v.008H12v-.008Z',
    error:
      'M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126ZM12 15.75h.007v.008H12v-.008Z',
    notification:
      'M14.857 17.082a23.848 23.848 0 0 0 5.454-1.31A8.967 8.967 0 0 1 18 9.75V9A6 6 0 0 0 6 9v.75a8.967 8.967 0 0 1-2.312 6.022c1.733.64 3.56 1.085 5.455 1.31m5.714 0a24.255 24.255 0 0 1-5.714 0m5.714 0a3 3 0 1 1-5.714 0',
    routing_suggestion:
      'M7.5 21 3 16.5m0 0L7.5 12M3 16.5h13.5m0-13.5L21 7.5m0 0L16.5 12M21 7.5H7.5',
    feedback_addressed: 'M9 12.75 11.25 15 15 9.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
    review:
      'M2.036 12.322a1.012 1.012 0 0 1 0-.639C3.423 7.51 7.36 4.5 12 4.5c4.638 0 8.573 3.007 9.963 7.178.07.207.07.431 0 .639C20.577 16.49 16.64 19.5 12 19.5c-4.638 0-8.573-3.007-9.963-7.178Z M15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z'
  };

  function getIconPath(itemType: string): string {
    return typeIcons[itemType] || typeIcons.notification;
  }

  function getPrUrl(item: QueueItem): string | undefined {
    return item.context?.pr_url as string | undefined;
  }

  async function handleInboxOption(itemId: string, option: string) {
    respondingItemId = itemId;
    try {
      await inbox.respond(itemId, option);
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      respondingItemId = null;
    }
  }

  async function handleCustomSubmit(itemId: string) {
    const response = customResponses[itemId]?.trim();
    if (!response) return;
    respondingItemId = itemId;
    try {
      await inbox.respond(itemId, response);
      customResponses[itemId] = '';
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      respondingItemId = null;
    }
  }
</script>

<svelte:window on:click={handleClickOutside} />

{#if desktop || mobile || $isTasksOpen}
  <div
    class="tasks-panel flex h-full flex-col bg-term-bg"
    class:fixed={!desktop && !mobile}
    class:right-0={!desktop && !mobile}
    class:top-0={!desktop && !mobile}
    class:z-50={!desktop && !mobile}
    class:w-full={!desktop && !mobile}
    class:max-w-md={!desktop && !mobile}
    class:border-l={!desktop && !mobile}
    class:border-term-border={!desktop && !mobile}
    class:shadow-xl={!desktop && !mobile}
  >
    <header class="flex items-center justify-between border-b border-term-border px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-term-fg">[INBOX]</h2>
        {#if attentionCount > 0}
          <span class="border border-term-info px-2 py-0.5 text-xs text-term-info">
            {attentionCount}
          </span>
        {/if}
      </div>
      <div class="flex items-center gap-1">
        <!-- Expand/Collapse all buttons (VSCode style) -->
        {#if activeTasks.length > 0}
          <button
            onclick={expandAll}
            class="p-1 text-term-fg-muted transition-colors hover:text-term-fg"
            aria-label="Expand all"
            title="Expand all"
          >
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor" class="h-4 w-4">
              <path stroke-linecap="round" stroke-linejoin="round" d="M3.75 3.75v4.5m0-4.5h4.5m-4.5 0L9 9M3.75 20.25v-4.5m0 4.5h4.5m-4.5 0L9 15M20.25 3.75h-4.5m4.5 0v4.5m0-4.5L15 9m5.25 11.25h-4.5m4.5 0v-4.5m0 4.5L15 15" />
            </svg>
          </button>
          <button
            onclick={collapseAll}
            class="p-1 text-term-fg-muted transition-colors hover:text-term-fg"
            aria-label="Collapse all"
            title="Collapse all"
          >
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor" class="h-4 w-4">
              <path stroke-linecap="round" stroke-linejoin="round" d="M9 9V4.5M9 9H4.5M9 9 3.75 3.75M9 15v4.5M9 15H4.5M9 15l-5.25 5.25M15 9h4.5M15 9V4.5M15 9l5.25-5.25M15 15h4.5M15 15v4.5m0-4.5 5.25 5.25" />
            </svg>
          </button>
        {/if}
        {#if !desktop && !mobile}
          <button
            onclick={handleClose}
            class="p-1 text-term-fg-muted transition-colors hover:text-term-fg"
            aria-label="Close panel"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              stroke-width="1.5"
              stroke="currentColor"
              class="h-6 w-6"
            >
              <path stroke-linecap="square" stroke-linejoin="miter" d="M6 18 18 6M6 6l12 12" />
            </svg>
          </button>
        {/if}
      </div>
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $inboxItems.length === 0 && $tasksList.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <p class="text-term-fg-muted">$ ls inbox/</p>
          <p class="mt-2 text-term-fg-muted">All caught up</p>
        </div>
      {:else}
        <div>
          <!-- Inbox items (needs response) -->
          {#each $inboxItems as item (item.id)}
            <div
              class="border-b border-l-2 border-term-border p-4 transition-colors {priorityStyles[item.priority]} {item.read_at
                ? 'opacity-75'
                : ''}"
            >
              <div class="flex items-start gap-3">
                <svg
                  xmlns="http://www.w3.org/2000/svg"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke-width="1.5"
                  stroke="currentColor"
                  class="mt-0.5 h-5 w-5 shrink-0 text-term-fg-muted"
                >
                  <path stroke-linecap="square" stroke-linejoin="miter" d={getIconPath(item.item_type)} />
                </svg>

                <div class="min-w-0 flex-1">
                  <div class="flex items-center justify-between gap-2">
                    <h3 class="text-term-fg">{item.title}</h3>
                    <span class="shrink-0 text-xs text-term-fg-muted">{formatTime(item.created_at)}</span>
                  </div>

                  <!-- Plan review items get special treatment -->
                  {#if item.item_type === 'plan_review'}
                    <!-- Expandable plan content -->
                    <button
                      onclick={() => (expandedPlanId = expandedPlanId === item.id ? null : item.id)}
                      class="mt-2 flex w-full items-center gap-2 text-left text-sm text-term-info hover:underline"
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4 transition-transform {expandedPlanId === item.id ? 'rotate-180' : ''}"
                      >
                        <path
                          stroke-linecap="square"
                          stroke-linejoin="miter"
                          d="m19.5 8.25-7.5 7.5-7.5-7.5"
                        />
                      </svg>
                      {expandedPlanId === item.id ? 'Hide plan' : 'View plan'}
                    </button>

                    {#if expandedPlanId === item.id}
                      <div class="mt-3 max-h-96 overflow-y-auto rounded border border-term-border bg-term-bg-secondary p-3">
                        <pre class="whitespace-pre-wrap text-xs text-term-fg">{item.content}</pre>
                      </div>
                    {/if}
                  {:else}
                    <p class="mt-1 text-sm text-term-fg-muted">{item.content}</p>
                  {/if}

                  {#if getPrUrl(item)}
                    <a
                      href={getPrUrl(item)}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="mt-2 inline-flex items-center gap-1 text-sm text-term-info hover:underline"
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4"
                      >
                        <path
                          stroke-linecap="square"
                          stroke-linejoin="miter"
                          d="M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25"
                        />
                      </svg>
                      View PR
                    </a>
                  {/if}

                  {#if item.options && item.options.length > 0 && item.status === 'pending'}
                    <div class="mt-3 flex flex-wrap gap-2">
                      {#each item.options as option}
                        <button
                          onclick={() => handleInboxOption(item.id, option)}
                          disabled={respondingItemId === item.id}
                          class="border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50 {option === 'Approve' ? 'border-term-accent-alt text-term-accent-alt hover:bg-term-accent-alt/10' : ''}"
                        >
                          {option}
                        </button>
                      {/each}
                    </div>
                  {/if}

                  <!-- Text input for questions AND plan_review items -->
                  {#if (item.item_type === 'question' || item.item_type === 'plan_review') && item.status === 'pending'}
                    <form
                      onsubmit={(e) => {
                        e.preventDefault();
                        handleCustomSubmit(item.id);
                      }}
                      class="mt-3"
                    >
                      <div class="flex gap-2">
                        <input
                          type="text"
                          bind:value={customResponses[item.id]}
                          placeholder={item.item_type === 'plan_review' ? 'Request changes...' : 'Type your response...'}
                          disabled={respondingItemId === item.id}
                          class="flex-1 border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none disabled:opacity-50"
                        />
                        <button
                          type="submit"
                          disabled={respondingItemId === item.id || !customResponses[item.id]?.trim()}
                          class="border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50"
                        >
                          {item.item_type === 'plan_review' ? 'REVISE' : 'SEND'}
                        </button>
                      </div>
                    </form>
                  {/if}

                  {#if item.status === 'responded' && item.response}
                    <div class="mt-2 border border-term-border bg-term-bg px-2 py-1 text-sm text-term-fg-muted">
                      > {item.response}
                    </div>
                  {/if}
                </div>
              </div>
            </div>
          {/each}

          <!-- Active tasks (in progress) -->
          {#each activeTasks as task (task.id)}
            {@const isActive = ['planning', 'implementing'].includes(task.status)}
            {@const isWaitingOnUser = needsAttention(task)}
            <div class="border-b border-term-border">
              <!-- svelte-ignore a11y_no_static_element_interactions a11y_no_noninteractive_tabindex -->
              <div
                onclick={() => toggleExpand(task.id, task.status)}
                onkeydown={(e) => !isWaitingOnUser && e.key === 'Enter' && toggleExpand(task.id, task.status)}
                role="button"
                tabindex={isWaitingOnUser ? -1 : 0}
                class="w-full cursor-pointer p-4 text-left transition-colors hover:bg-term-selection"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm text-term-fg">
                      {task.description.length > 60
                        ? task.description.slice(0, 60) + '...'
                        : task.description}
                    </p>
                    <div class="mt-1 flex flex-wrap items-center gap-2 text-xs">
                      <span class="flex items-center gap-1 border px-2 py-0.5 {getStatusStyle(task.status)} {isActive ? 'animate-pulse' : ''}">
                        {getStatusLabel(task.status)}
                      </span>
                      <span class="text-term-fg-muted">{formatTime(task.created_at)}</span>
                      {#if task.repo_url}
                        <span class="max-w-32 truncate text-term-fg-muted" title={task.repo_url}>
                          {task.repo_url.replace('https://github.com/', '')}
                        </span>
                      {/if}
                    </div>
                    {#if task.issue_url}
                      <a
                        href={task.issue_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        onclick={(e) => e.stopPropagation()}
                        class="mt-2 inline-flex items-center gap-1 text-xs text-term-accent hover:underline"
                      >
                        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                          <path d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z" />
                          <path
                            d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z"
                          />
                        </svg>
                        Issue #{task.issue_number}
                      </a>
                    {/if}
                    {#if task.pr_url}
                      <a
                        href={task.pr_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        onclick={(e) => e.stopPropagation()}
                        class="mt-2 inline-flex items-center gap-1 text-xs text-term-info hover:underline"
                      >
                        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                          <path
                            d="M7.177 3.073L9.573.677A.25.25 0 0110 .854v4.792a.25.25 0 01-.427.177L7.177 3.427a.25.25 0 010-.354zM3.75 2.5a.75.75 0 100 1.5.75.75 0 000-1.5zm-2.25.75a2.25 2.25 0 113 2.122v5.256a2.251 2.251 0 11-1.5 0V5.372A2.25 2.25 0 011.5 3.25zM11 2.5h-1V4h1a1 1 0 011 1v5.628a2.251 2.251 0 101.5 0V5A2.5 2.5 0 0011 2.5zm1 10.25a.75.75 0 111.5 0 .75.75 0 01-1.5 0zM3.75 12a.75.75 0 100 1.5.75.75 0 000-1.5z"
                          />
                        </svg>
                        PR #{task.pr_number}
                      </a>
                    {/if}
                  </div>

                  <div class="flex items-center gap-1">
                    <button
                      onclick={(e) => {
                        e.stopPropagation();
                        handleCancel(task.id);
                      }}
                      class="p-1 text-term-fg-muted transition-colors hover:text-term-error"
                      aria-label="Cancel task"
                      title="Cancel task"
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4"
                      >
                        <path
                          stroke-linecap="square"
                          stroke-linejoin="miter"
                          d="M6 18 18 6M6 6l12 12"
                        />
                      </svg>
                    </button>

                    <svg
                      xmlns="http://www.w3.org/2000/svg"
                      fill="none"
                      viewBox="0 0 24 24"
                      stroke-width="1.5"
                      stroke="currentColor"
                      class="h-4 w-4 text-term-fg-muted transition-transform {expandedTaskIds.has(task.id)
                        ? 'rotate-180'
                        : ''}"
                    >
                      <path
                        stroke-linecap="square"
                        stroke-linejoin="miter"
                        d="m19.5 8.25-7.5 7.5-7.5-7.5"
                      />
                    </svg>
                  </div>
                </div>
              </div>

              {#if expandedTaskIds.has(task.id)}
                <div class="border-t border-term-border bg-term-bg-secondary">
                  <!-- Questions UI -->
                  {#if task.status === 'waiting_questions' && task.pending_questions}
                    {@const activeQuestionId = getActiveQuestionId(task)}
                    <div class="p-4 space-y-2">
                      {#each task.pending_questions as question, qIndex (question.id)}
                        {@const answer = getAnswer(task.id, question.id)}
                        {@const isActive = question.id === activeQuestionId}
                        {@const isAnswered = !!answer}

                        {#if isAnswered && !isActive}
                          <!-- Answered question - collapsed view, clickable to edit -->
                          <button
                            onclick={() => editingQuestionId[task.id] = question.id}
                            class="w-full flex items-center gap-2 p-2 text-left border border-term-border/50 bg-term-bg hover:border-term-accent/50 transition-colors group"
                          >
                            <span class="text-term-accent-alt text-xs">✓</span>
                            <span class="px-1.5 py-0.5 text-xs border border-term-fg-muted/50 text-term-fg-muted">{question.header}</span>
                            <span class="text-sm text-term-fg truncate flex-1">{answer}</span>
                            <span class="text-xs text-term-fg-muted opacity-0 group-hover:opacity-100">edit</span>
                          </button>
                        {:else if isActive}
                          <!-- Active question - expanded view -->
                          <div class="space-y-3 p-3 border border-term-accent/30 bg-term-bg">
                            <div class="flex items-center gap-2">
                              <span class="w-5 h-5 flex items-center justify-center text-xs border border-term-accent text-term-accent">{qIndex + 1}</span>
                              <span class="px-2 py-0.5 text-xs border border-term-accent text-term-accent">{question.header}</span>
                            </div>
                            <p class="text-sm text-term-fg">{question.question}</p>
                            <div class="flex flex-wrap gap-2">
                              {#each question.options as opt}
                                <button
                                  type="button"
                                  tabindex={-1}
                                  onclick={() => selectOptionAndAdvance(task.id, question.id, opt.label)}
                                  disabled={submittingTaskId === task.id}
                                  class="border px-3 py-1.5 text-sm transition-colors disabled:opacity-50
                                    {selectedAnswers[task.id]?.[question.id] === opt.label
                                      ? 'border-term-accent bg-term-accent/10 text-term-accent'
                                      : 'border-term-border text-term-fg hover:border-term-accent hover:text-term-accent'}"
                                  title={opt.description || ''}
                                >
                                  {opt.label}
                                </button>
                              {/each}
                            </div>
                            <!-- Custom input with inline confirm -->
                            <div class="flex gap-2" onclick={(e) => e.stopPropagation()} onkeydown={(e) => e.stopPropagation()}>
                              <input
                                use:autofocus
                                type="text"
                                placeholder="Or type a custom answer..."
                                value={customQuestionInputs[task.id]?.[question.id] || ''}
                                onfocus={() => editingQuestionId[task.id] = question.id}
                                oninput={(e) => setCustomAnswer(task.id, question.id, (e.target as HTMLInputElement).value)}
                                onkeydown={(e) => {
                                  e.stopPropagation();  // Prevent bubbling to parent handlers
                                  if (e.key === 'Enter' && customQuestionInputs[task.id]?.[question.id]?.trim()) {
                                    editingQuestionId[task.id] = null;  // Advance to next
                                  }
                                }}
                                disabled={submittingTaskId === task.id}
                                class="flex-1 border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none disabled:opacity-50"
                              />
                              {#if customQuestionInputs[task.id]?.[question.id]?.trim()}
                                <button
                                  onclick={() => editingQuestionId[task.id] = null}
                                  class="border border-term-accent px-3 py-1.5 text-sm text-term-accent hover:bg-term-accent/10"
                                >
                                  OK
                                </button>
                              {/if}
                            </div>
                          </div>
                        {:else}
                          <!-- Future question - dimmed -->
                          <div class="flex items-center gap-2 p-2 opacity-40">
                            <span class="w-5 h-5 flex items-center justify-center text-xs border border-term-fg-muted text-term-fg-muted">{qIndex + 1}</span>
                            <span class="px-1.5 py-0.5 text-xs border border-term-fg-muted/50 text-term-fg-muted">{question.header}</span>
                          </div>
                        {/if}
                      {/each}

                      <!-- Submit button - shows when all answered -->
                      {#if allQuestionsAnswered(task)}
                        <div class="flex gap-2 pt-3 mt-2 border-t border-term-border">
                          <button
                            onclick={() => submitAnswers(task.id)}
                            disabled={submittingTaskId === task.id}
                            class="border border-term-accent-alt bg-term-accent-alt/10 px-4 py-2 text-sm text-term-accent-alt transition-colors hover:bg-term-accent-alt/20 disabled:opacity-50"
                          >
                            {submittingTaskId === task.id ? 'Submitting...' : 'Continue →'}
                          </button>
                          <button
                            onclick={() => tasks.cancelQuestions(task.id)}
                            disabled={submittingTaskId === task.id}
                            class="border border-term-border px-4 py-2 text-sm text-term-fg-muted transition-colors hover:border-term-error hover:text-term-error disabled:opacity-50"
                          >
                            Cancel
                          </button>
                        </div>
                      {/if}
                    </div>
                  {:else if task.status === 'waiting_plan_review' && task.plan_text}
                    <!-- Plan Review UI - no separate collapse, just shows in task -->
                    <div class="p-4 space-y-3">
                      <!-- Plan content with markdown rendering -->
                      <div class="max-h-[60vh] overflow-y-auto rounded border border-term-border bg-term-bg p-4 prose-terminal text-sm">
                        {@html marked.parse(task.plan_text)}
                      </div>

                      <!-- Approve/Revise buttons -->
                      <div class="flex gap-2 pt-2">
                        <button
                          onclick={() => handleApprovePlan(task.id)}
                          disabled={submittingTaskId === task.id}
                          class="border border-term-accent-alt bg-term-accent-alt/10 px-4 py-2 text-sm text-term-accent-alt transition-colors hover:bg-term-accent-alt/20 disabled:opacity-50"
                        >
                          {submittingTaskId === task.id ? 'Approving...' : 'Approve Plan'}
                        </button>
                        <button
                          onclick={() => tasks.cancelTask(task.id)}
                          disabled={submittingTaskId === task.id}
                          class="border border-term-border px-4 py-2 text-sm text-term-fg-muted transition-colors hover:border-term-error hover:text-term-error disabled:opacity-50"
                        >
                          Cancel
                        </button>
                      </div>
                      <!-- Revision input -->
                      <div class="flex gap-2">
                        <input
                          type="text"
                          placeholder="Request changes..."
                          bind:value={planRevisionText[task.id]}
                          disabled={submittingTaskId === task.id}
                          class="flex-1 border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none disabled:opacity-50"
                        />
                        <button
                          onclick={() => handleRevisePlan(task.id)}
                          disabled={submittingTaskId === task.id || !planRevisionText[task.id]?.trim()}
                          class="border border-term-border px-4 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50"
                        >
                          Revise
                        </button>
                      </div>
                    </div>
                  {:else if task.status === 'ready_to_implement' && task.plan_text}
                    <!-- Ready to Implement UI - plan approved, waiting to start -->
                    <div class="p-4 space-y-3">
                      <!-- Plan content with markdown rendering -->
                      <div class="max-h-[60vh] overflow-y-auto rounded border border-term-accent-alt/30 bg-term-bg p-4 prose-terminal text-sm">
                        {@html marked.parse(task.plan_text)}
                      </div>

                      <!-- Status message -->
                      <div class="flex items-center gap-2 text-sm text-term-accent-alt">
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor" class="h-5 w-5">
                          <path stroke-linecap="round" stroke-linejoin="round" d="M9 12.75 11.25 15 15 9.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z" />
                        </svg>
                        Plan approved! Ready to start implementation.
                      </div>

                      <!-- Start Implementation button -->
                      <div class="flex gap-2 pt-2">
                        <button
                          onclick={() => handleStartImplementation(task.id)}
                          disabled={submittingTaskId === task.id}
                          class="border border-term-accent-alt bg-term-accent-alt/10 px-4 py-2 text-sm text-term-accent-alt transition-colors hover:bg-term-accent-alt/20 disabled:opacity-50"
                        >
                          {submittingTaskId === task.id ? 'Starting...' : 'Start Implementation →'}
                        </button>
                        <button
                          onclick={() => tasks.cancelTask(task.id)}
                          disabled={submittingTaskId === task.id}
                          class="border border-term-border px-4 py-2 text-sm text-term-fg-muted transition-colors hover:border-term-error hover:text-term-error disabled:opacity-50"
                        >
                          Cancel
                        </button>
                      </div>

                      <!-- Link to GitHub issue if available -->
                      {#if task.issue_url}
                        <a
                          href={task.issue_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          class="inline-flex items-center gap-1 text-xs text-term-info hover:underline"
                        >
                          <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                            <path d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z" />
                            <path d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z" />
                          </svg>
                          View on GitHub
                        </a>
                      {/if}
                    </div>
                  {:else}
                    <LogViewer taskId={task.id} taskStatus={task.status} />
                  {/if}
                </div>
              {/if}
            </div>
          {/each}

          <!-- Recently finished (completed or failed) - shown prominently -->
          {#each recentlyFinished as task (task.id)}
            <div class="border-b border-term-border bg-term-bg-secondary/50">
              <div class="p-4">
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm text-term-fg">
                      {task.description.length > 60
                        ? task.description.slice(0, 60) + '...'
                        : task.description}
                    </p>
                    <div class="mt-1 flex flex-wrap items-center gap-2 text-xs">
                      <span class="border px-2 py-0.5 {getStatusStyle(task.status)}">
                        {getStatusLabel(task.status)}
                      </span>
                      <span class="text-term-fg-muted">{formatTime(task.completed_at || task.created_at)}</span>
                    </div>
                    {#if task.error}
                      <p class="mt-2 text-xs text-term-error">{task.error}</p>
                    {/if}
                    {#if task.pr_url}
                      <a
                        href={task.pr_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        class="mt-2 inline-flex items-center gap-1 text-xs text-term-info hover:underline"
                      >
                        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                          <path
                            d="M7.177 3.073L9.573.677A.25.25 0 0110 .854v4.792a.25.25 0 01-.427.177L7.177 3.427a.25.25 0 010-.354zM3.75 2.5a.75.75 0 100 1.5.75.75 0 000-1.5zm-2.25.75a2.25 2.25 0 113 2.122v5.256a2.251 2.251 0 11-1.5 0V5.372A2.25 2.25 0 011.5 3.25zM11 2.5h-1V4h1a1 1 0 011 1v5.628a2.251 2.251 0 101.5 0V5A2.5 2.5 0 0011 2.5zm1 10.25a.75.75 0 111.5 0 .75.75 0 01-1.5 0zM3.75 12a.75.75 0 100 1.5.75.75 0 000-1.5z"
                          />
                        </svg>
                        PR #{task.pr_number}
                      </a>
                    {/if}
                  </div>
                  {#if task.status === 'failed'}
                    <button
                      onclick={() => handleRetry(task.id)}
                      class="p-1 text-term-fg-muted transition-colors hover:text-term-info"
                      aria-label="Retry task"
                      title="Retry task"
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4"
                      >
                        <path
                          stroke-linecap="square"
                          stroke-linejoin="miter"
                          d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99"
                        />
                      </svg>
                    </button>
                  {/if}
                </div>
              </div>
            </div>
          {/each}

          <!-- Older completed/failed/cancelled - collapsible -->
          {#if olderTerminalTasks.length > 0}
            <div class="border-b border-term-border">
              <button
                onclick={() => (showRecent = !showRecent)}
                class="flex w-full items-center justify-between px-4 py-3 text-left text-sm text-term-fg-muted hover:bg-term-selection"
              >
                <span>History ({olderTerminalTasks.length})</span>
                <svg
                  xmlns="http://www.w3.org/2000/svg"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke-width="1.5"
                  stroke="currentColor"
                  class="h-4 w-4 transition-transform {showRecent ? 'rotate-180' : ''}"
                >
                  <path
                    stroke-linecap="square"
                    stroke-linejoin="miter"
                    d="m19.5 8.25-7.5 7.5-7.5-7.5"
                  />
                </svg>
              </button>

              {#if showRecent}
                <div class="border-t border-term-border">
                  {#each olderTerminalTasks as task (task.id)}
                    <div class="border-b border-term-border p-4 opacity-60 last:border-b-0">
                      <div class="flex items-start justify-between gap-2">
                        <div class="min-w-0 flex-1">
                          <p class="truncate text-sm text-term-fg">
                            {task.description.length > 50
                              ? task.description.slice(0, 50) + '...'
                              : task.description}
                          </p>
                          <div class="mt-1 flex flex-wrap items-center gap-2 text-xs">
                            <span class="border px-2 py-0.5 {getStatusStyle(task.status)}">
                              {getStatusLabel(task.status)}
                            </span>
                            <span class="text-term-fg-muted">{formatTime(task.completed_at || task.created_at)}</span>
                          </div>
                          {#if task.error}
                            <p class="mt-1 text-xs text-term-error">{task.error}</p>
                          {/if}
                          {#if task.pr_url}
                            <a
                              href={task.pr_url}
                              target="_blank"
                              rel="noopener noreferrer"
                              class="mt-1 inline-flex items-center gap-1 text-xs text-term-info hover:underline"
                            >
                              PR #{task.pr_number}
                            </a>
                          {/if}
                        </div>
                        {#if task.status === 'failed'}
                          <button
                            onclick={() => handleRetry(task.id)}
                            class="p-1 text-term-fg-muted transition-colors hover:text-term-info"
                            aria-label="Retry task"
                            title="Retry task"
                          >
                            <svg
                              xmlns="http://www.w3.org/2000/svg"
                              fill="none"
                              viewBox="0 0 24 24"
                              stroke-width="1.5"
                              stroke="currentColor"
                              class="h-4 w-4"
                            >
                              <path
                                stroke-linecap="square"
                                stroke-linejoin="miter"
                                d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99"
                              />
                            </svg>
                          </button>
                        {/if}
                      </div>
                    </div>
                  {/each}
                </div>
              {/if}
            </div>
          {/if}
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  /* Markdown styles for plan content */
  .prose-terminal :global(p) {
    margin-bottom: 0.75rem;
  }
  .prose-terminal :global(p:last-child) {
    margin-bottom: 0;
  }
  .prose-terminal :global(code) {
    background-color: var(--term-bg-secondary);
    padding: 0.125rem 0.25rem;
    border-radius: 0.125rem;
    font-size: 0.85em;
    color: var(--term-accent);
  }
  .prose-terminal :global(pre) {
    background-color: var(--term-bg-secondary);
    padding: 0.75rem;
    border-radius: 0.25rem;
    overflow-x: auto;
    margin: 0.75rem 0;
    border: 1px solid var(--term-border);
  }
  .prose-terminal :global(pre code) {
    background: none;
    padding: 0;
    color: var(--term-fg);
  }
  .prose-terminal :global(ul),
  .prose-terminal :global(ol) {
    margin: 0.5rem 0;
    padding-left: 1.5rem;
  }
  .prose-terminal :global(li) {
    margin: 0.25rem 0;
  }
  .prose-terminal :global(ul) {
    list-style-type: disc;
  }
  .prose-terminal :global(ol) {
    list-style-type: decimal;
  }
  .prose-terminal :global(strong) {
    font-weight: 600;
    color: var(--term-fg);
  }
  .prose-terminal :global(h1),
  .prose-terminal :global(h2),
  .prose-terminal :global(h3),
  .prose-terminal :global(h4) {
    font-weight: 600;
    color: var(--term-fg);
    margin: 1rem 0 0.5rem 0;
  }
  .prose-terminal :global(h1) {
    font-size: 1.25rem;
  }
  .prose-terminal :global(h2) {
    font-size: 1.1rem;
  }
  .prose-terminal :global(h3),
  .prose-terminal :global(h4) {
    font-size: 1rem;
  }
  .prose-terminal :global(blockquote) {
    border-left: 2px solid var(--term-border);
    padding-left: 0.75rem;
    margin: 0.5rem 0;
    color: var(--term-fg-muted);
  }
  .prose-terminal :global(hr) {
    border: none;
    border-top: 1px solid var(--term-border);
    margin: 1rem 0;
  }
</style>
