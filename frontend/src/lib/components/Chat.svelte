<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { projects } from '$lib/stores/projects';
  import { tasks, tasksList } from '$lib/stores/tasks';
  import { api, type WorkerTask } from '$lib/api';
  import MessageBubble from './MessageBubble.svelte';
  import ThreadPreview from './ThreadPreview.svelte';
  import ThreadExpanded from './ThreadExpanded.svelte';
  import InputBar from './InputBar.svelte';

  let { messages, isLoading } = $derived($conversationStore);

  let messagesContainer: HTMLDivElement;
  let showScrollButton = $state(false);
  let expandedTaskId = $state<string | null>(null);

  // Map message_id -> task for inline thread rendering
  let tasksByMessageId = $derived(
    new Map($tasksList.filter((t) => t.message_id).map((t) => [t.message_id!, t]))
  );

  // Get task linked to a message
  function getTaskForMessage(messageId: string): WorkerTask | undefined {
    return tasksByMessageId.get(messageId);
  }

  // Convert WorkerTask to Thread-like shape for ThreadPreview/ThreadExpanded
  function taskToThread(task: WorkerTask) {
    // Determine thread status from task status
    let status: 'active' | 'waiting' | 'completed' | 'failed' = 'active';
    if (['completed'].includes(task.status)) status = 'completed';
    else if (['failed', 'cancelled'].includes(task.status)) status = 'failed';
    else if (['waiting_questions', 'waiting_plan_review', 'ready_to_implement'].includes(task.status))
      status = 'waiting';

    // Build thread messages from task state
    const threadMessages: Array<{ id: string; role: 'worker' | 'user'; content: string; timestamp: string }> = [];

    // Add status message
    const statusMessages: Record<string, string> = {
      pending: 'Starting...',
      planning: 'Creating implementation plan...',
      waiting_questions: 'I have some questions before proceeding.',
      waiting_plan_review: 'Plan ready for your review.',
      ready_to_implement: 'Plan approved. Ready to implement.',
      implementing: 'Implementing the changes...',
      under_review: 'PR created. Waiting for review.',
      completed: 'Done!',
      failed: task.error || 'Something went wrong.',
      cancelled: 'Cancelled.'
    };

    threadMessages.push({
      id: `${task.id}-status`,
      role: 'worker',
      content: statusMessages[task.status] || task.status,
      timestamp: task.created_at
    });

    // Add questions if pending
    if (task.pending_questions?.length) {
      for (const q of task.pending_questions) {
        threadMessages.push({
          id: q.id,
          role: 'worker',
          content: `**${q.header}:** ${q.question}`,
          timestamp: task.created_at
        });
      }
    }

    // Add plan if ready for review
    if (task.plan_text && ['waiting_plan_review', 'ready_to_implement'].includes(task.status)) {
      threadMessages.push({
        id: `${task.id}-plan`,
        role: 'worker',
        content: task.plan_text,
        timestamp: task.created_at
      });
    }

    return {
      id: task.id,
      parentMessageId: task.message_id || '',
      title: task.description.slice(0, 50) + (task.description.length > 50 ? '...' : ''),
      status,
      messages: threadMessages,
      unreadCount: status === 'waiting' ? 1 : 0,
      result: {
        prUrl: task.pr_url || undefined,
        prNumber: task.pr_number || undefined,
        planText: task.plan_text || undefined
      },
      // Keep reference to original task for actions
      _task: task
    };
  }

  // Check if scrolled to bottom
  function checkScrollPosition() {
    if (!messagesContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = messagesContainer;
    const distanceFromBottom = scrollHeight - scrollTop - clientHeight;
    showScrollButton = distanceFromBottom > 100;
  }

  function scrollToBottom() {
    if (messagesContainer) {
      messagesContainer.scrollTo({
        top: messagesContainer.scrollHeight,
        behavior: 'smooth'
      });
    }
  }

  // Auto-scroll to bottom when messages change or loading state changes
  $effect(() => {
    messages;
    isLoading;
    $tasksList;

    tick().then(() => {
      if (messagesContainer) {
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
        showScrollButton = false;
      }
    });
  });

  onMount(() => {
    // Start listening for task updates
    tasks.startListening();

    // Load the most recent conversation on startup
    (async () => {
      try {
        const { conversations } = await api.listConversations();
        if (conversations.length > 0) {
          const latest = conversations[0];
          const { conversation, messages } = await api.getConversation(latest.id);
          conversationStore.setConversation(conversation, messages);
        }
      } catch (error) {
        console.error('Failed to load conversation:', error);
      }
    })();

    return () => {
      tasks.stopListening();
    };
  });

  async function handleSendMessage(detail: { message: string }) {
    const userMessage = detail.message;
    const currentConversationId = $conversationStore.currentConversation?.id;

    // Optimistic: Add user message immediately
    conversationStore.addMessage({
      id: `temp-${Date.now()}`,
      conversation_id: currentConversationId || 'pending',
      role: 'user',
      content: userMessage,
      created_at: new Date().toISOString()
    });

    conversationStore.setLoading(true);

    try {
      const response = await api.sendMessage({
        message: userMessage,
        conversation_id: currentConversationId
      });

      if (!currentConversationId) {
        conversationStore.setCurrentConversation({
          id: response.conversation_id,
          user_id: '',
          title: null,
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString()
        });
      }

      conversationStore.addMessage(response.message);
      projects.fetchProjects();
      tasks.fetchTasks(); // Refresh tasks in case one was spawned
    } catch (error) {
      console.error('Failed to send message:', error);
    } finally {
      conversationStore.setLoading(false);
    }
  }

  function toggleThread(taskId: string) {
    expandedTaskId = expandedTaskId === taskId ? null : taskId;
  }

  // Handle thread reply - this could answer questions or provide feedback
  async function handleThreadReply(taskId: string, message: string) {
    const task = $tasksList.find((t) => t.id === taskId);
    if (!task) return;

    // If task has pending questions, treat reply as answering the first one
    if (task.pending_questions?.length) {
      const answers: Record<string, string> = {};
      answers[task.pending_questions[0].id] = message;
      await tasks.answerQuestions(taskId, answers);
    }
  }

  // Get the expanded task's thread
  let expandedThread = $derived(
    expandedTaskId ? taskToThread($tasksList.find((t) => t.id === expandedTaskId)!) : null
  );

  // Find any waiting thread for the input hint
  let waitingTask = $derived($tasksList.find((t) =>
    ['waiting_questions', 'waiting_plan_review'].includes(t.status)
  ));
</script>

<div class="relative flex h-full flex-col bg-term-bg">
  <!-- Messages -->
  <div
    bind:this={messagesContainer}
    onscroll={checkScrollPosition}
    class="flex-1 space-y-2 overflow-y-auto p-4"
  >
    {#if messages.length === 0}
      <div class="flex h-full flex-col items-center justify-center text-term-fg-muted">
        <p class="text-term-accent">$ mainloop --help</p>
        <p class="mt-2">Start a conversation to begin</p>
        <p class="animate-cursor text-term-accent">_</p>
      </div>
    {:else}
      {#each messages as message (message.id)}
        <!-- Render the message -->
        <MessageBubble {message} />

        <!-- Check if there's a task linked to this message -->
        {@const task = getTaskForMessage(message.id)}
        {#if task}
          {@const thread = taskToThread(task)}
          {#if expandedTaskId === task.id}
            <ThreadExpanded
              {thread}
              onClose={() => toggleThread(task.id)}
              onReply={(msg) => handleThreadReply(task.id, msg)}
            />
          {:else}
            <ThreadPreview
              {thread}
              onToggle={() => toggleThread(task.id)}
            />
          {/if}
        {/if}
      {/each}
    {/if}

    {#if isLoading}
      <div
        class="flex w-full flex-col gap-1 border-l-2 border-term-accent bg-term-bg-secondary px-3 py-2 md:flex-row md:items-center md:gap-3 md:px-4"
      >
        <span class="text-xs text-term-accent md:text-sm">
          >
          <span class="hidden md:inline">claude@mainloop</span>
        </span>
        <div class="flex items-center gap-2">
          <span class="text-sm text-term-fg-muted">processing</span>
          <span class="animate-cursor text-term-accent">_</span>
        </div>
      </div>
    {/if}
  </div>

  <!-- Scroll to bottom button -->
  {#if showScrollButton}
    <button
      type="button"
      onclick={scrollToBottom}
      class="absolute bottom-24 right-4 flex h-10 w-10 items-center justify-center border border-term-border bg-term-bg-secondary text-term-fg-muted transition-colors hover:border-term-accent hover:text-term-accent"
      aria-label="Scroll to bottom"
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
        viewBox="0 0 24 24"
        stroke-width="2"
        stroke="currentColor"
        class="h-5 w-5"
      >
        <path stroke-linecap="square" stroke-linejoin="miter" d="M19 14l-7 7m0 0l-7-7m7 7V3" />
      </svg>
    </button>
  {/if}

  <!-- Input -->
  <div class="border-t border-term-border p-4">
    {#if expandedTaskId && expandedThread}
      <div class="mb-2 flex items-center gap-2 text-xs text-term-info">
        <span>🧵</span>
        <span>Replying in thread: {expandedThread.title}</span>
        <button
          type="button"
          onclick={() => (expandedTaskId = null)}
          class="text-term-fg-muted hover:text-term-fg"
        >
          [exit thread]
        </button>
      </div>
    {:else if waitingTask}
      <div class="mb-2 flex items-center gap-2 text-xs text-term-warning">
        <span>?</span>
        <span>Task waiting: {waitingTask.description.slice(0, 40)}...</span>
        <button
          type="button"
          onclick={() => toggleThread(waitingTask!.id)}
          class="text-term-warning hover:underline"
        >
          [open]
        </button>
      </div>
    {/if}
    <InputBar onsend={handleSendMessage} disabled={isLoading} />
  </div>
</div>
