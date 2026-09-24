# mainloop-backend

FastAPI control plane for native-agent sessions and Substrate workspaces.

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

See `.env.example` for the Substrate router, actor bindings, lifecycle settings, and database environment variables. Provider credentials are held by the configured actors, not by the backend.

## API Documentation

When running, visit:

- Swagger UI: http://localhost:8000/docs
- ReDoc: http://localhost:8000/redoc
