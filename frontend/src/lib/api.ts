/**
 * API client for backend communication
 */

const API_URL = import.meta.env.VITE_API_URL || 'http://localhost:8000';

export interface Message {
  id: string;
  conversation_id: string;
  role: 'user' | 'assistant';
  content: string;
  created_at: string;
}

export interface Conversation {
  id: string;
  user_id: string;
  title: string | null;
  created_at: string;
  updated_at: string;
}

export interface ChatRequest {
  message: string;
  conversation_id?: string;
}

export interface ChatResponse {
  conversation_id: string;
  message: Message;
}

export const api = {
  async listConversations(): Promise<{ conversations: Conversation[]; total: number }> {
    const response = await fetch(`${API_URL}/conversations`);
    if (!response.ok) throw new Error('Failed to list conversations');
    return response.json();
  },

  async getConversation(
    conversationId: string
  ): Promise<{ conversation: Conversation; messages: Message[] }> {
    const response = await fetch(`${API_URL}/conversations/${conversationId}`);
    if (!response.ok) throw new Error('Failed to get conversation');
    return response.json();
  },

  async sendMessage(request: ChatRequest): Promise<ChatResponse> {
    const response = await fetch(`${API_URL}/chat`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(request)
    });
    if (!response.ok) throw new Error('Failed to send message');
    return response.json();
  }
};
