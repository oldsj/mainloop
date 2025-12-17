# mainloop-backend

FastAPI backend for the mainloop AI agent orchestrator.

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

See `.env.example` for required environment variables.

## API Documentation

When running, visit:
- Swagger UI: http://localhost:8000/docs
- ReDoc: http://localhost:8000/redoc
