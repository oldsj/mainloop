"""FastAPI application."""

from fastapi import FastAPI, HTTPException, Header
from fastapi.middleware.cors import CORSMiddleware
from mainloop.config import settings
from mainloop.bigquery import bq_client
from mainloop.claude_agent import claude_agent
from mainloop.models import (
    ConversationListResponse,
    ConversationResponse,
    ChatRequest,
    ChatResponse,
)

app = FastAPI(
    title="Mainloop API",
    description="AI agent orchestrator API",
    version="0.1.0",
)

# CORS middleware for local development
app.add_middleware(
    CORSMiddleware,
    allow_origins=["http://localhost:3000"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


def get_user_id_from_cf_header(cf_access_jwt_assertion: str | None = Header(None)) -> str:
    """Extract user ID from Cloudflare Access JWT header."""
    if not cf_access_jwt_assertion:
        # For local development, return a mock user ID
        return "local-dev-user"

    # TODO: Decode and verify CF Access JWT
    # For now, return mock user ID
    return "user-from-cf-jwt"


@app.get("/")
async def root():
    """Root endpoint."""
    return {"message": "Mainloop API", "version": "0.1.0"}


@app.get("/health")
async def health():
    """Health check endpoint."""
    return {"status": "healthy"}


@app.get("/conversations", response_model=ConversationListResponse)
async def list_conversations(
    user_id: str = Header(alias="X-User-ID", default=None)
):
    """List user's conversations."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    conversations = await bq_client.list_conversations(user_id)
    return ConversationListResponse(
        conversations=conversations,
        total=len(conversations)
    )


@app.get("/conversations/{conversation_id}", response_model=ConversationResponse)
async def get_conversation(conversation_id: str):
    """Get a conversation with its messages."""
    conversation = await bq_client.get_conversation(conversation_id)
    if not conversation:
        raise HTTPException(status_code=404, detail="Conversation not found")

    messages = await bq_client.get_messages(conversation_id)
    return ConversationResponse(
        conversation=conversation,
        messages=messages
    )


@app.post("/chat", response_model=ChatResponse)
async def chat(
    request: ChatRequest,
    user_id: str = Header(alias="X-User-ID", default=None)
):
    """Send a message and get a response from Claude."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    # Get or create conversation
    if request.conversation_id:
        conversation = await bq_client.get_conversation(request.conversation_id)
        if not conversation:
            raise HTTPException(status_code=404, detail="Conversation not found")
    else:
        conversation = await bq_client.create_conversation(user_id)

    # Save user message
    user_message = await bq_client.create_message(
        conversation_id=conversation.id,
        role="user",
        content=request.message
    )

    # TODO: Send to Claude agent and get response
    # For now, return mock response
    assistant_message = await bq_client.create_message(
        conversation_id=conversation.id,
        role="assistant",
        content="This is a placeholder response. Claude agent integration coming soon."
    )

    return ChatResponse(
        conversation_id=conversation.id,
        message=assistant_message
    )


def run():
    """Run the application."""
    import uvicorn
    uvicorn.run(
        "mainloop.api:app",
        host=settings.host,
        port=settings.port,
        reload=True
    )


if __name__ == "__main__":
    run()
