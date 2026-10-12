# Native-agent boundary inventory

Superseded. This inventory mapped the retired Python backend (`backend/`, `models/`) and its
Claude Agent SDK and Job paths to proposed native-runtime boundaries. It is not current, and it
is not live-cluster proof.

The full table is kept in git history: `git show e776537:docs/architecture/native-agent-inventory.md`.

For the current native-session boundary, read:

- [Sessions spec](../specs/sessions.md): the Go session routes and their behavior.
- `server/internal/sessions` and `server/internal/kagent`: the Go session service and the kagent client.
- [Architecture](../architecture.md): the current component layout and known gaps.
