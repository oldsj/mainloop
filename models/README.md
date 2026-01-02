# models

Shared Pydantic models for the mainloop project.

This package contains all shared data models used across:

- Backend API
- Claude agent integration
- BigQuery schemas

## Models

- `Conversation`: Conversation metadata and state
- `Message`: Individual messages in a conversation
- `AgentTask`: Claude agent task definitions
- `AgentResponse`: Claude agent responses

## Usage

```python
from models import Conversation, Message
```
