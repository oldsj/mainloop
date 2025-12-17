/**
 * Conversation state management
 */

import { writable } from 'svelte/store';
import type { Conversation, Message } from '$lib/api';

interface ConversationState {
  currentConversation: Conversation | null;
  messages: Message[];
  isLoading: boolean;
}

function createConversationStore() {
  const { subscribe, set, update } = writable<ConversationState>({
    currentConversation: null,
    messages: [],
    isLoading: false
  });

  return {
    subscribe,
    setConversation: (conversation: Conversation, messages: Message[]) =>
      set({ currentConversation: conversation, messages, isLoading: false }),
    addMessage: (message: Message) =>
      update((state) => ({
        ...state,
        messages: [...state.messages, message]
      })),
    setLoading: (isLoading: boolean) =>
      update((state) => ({
        ...state,
        isLoading
      })),
    reset: () =>
      set({
        currentConversation: null,
        messages: [],
        isLoading: false
      })
  };
}

export const conversationStore = createConversationStore();
