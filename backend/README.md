# mainloop-backend

FastAPI control plane for native-agent sessions and kagent workspaces.

## Development

```bash
# Install dependencies
uv sync

# Run development server
uv run uvicorn mainloop.api:app --reload

# Or use make command from root
make backend-dev
```

## Environment Variables

See `.env.example` for kagent Agents, preview routing, owner identity and token-key settings.
Database connections use `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER` and `DB_PASSWORD`.
Provider credentials belong to the kagent installation. Outside development, `AGENT_TOKEN_KEY`
is required at startup for both REST and MCP processes.

## API Documentation

When running, visit:

- Swagger UI: http://localhost:8000/docs
- ReDoc: http://localhost:8000/redoc
