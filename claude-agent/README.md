# Claude Agent Container

This container runs the Claude Code CLI for the mainloop backend.

## Current State

The container currently just keeps running. In the future, this will:
1. Accept HTTP requests from the backend
2. Execute Claude Code CLI commands
3. Stream responses back to the backend

## Usage

The backend communicates with this container via the internal Docker network.

## Configuration

- Claude Code CLI uses the Max subscription
- Workspace is mounted at `/workspace`
- Configuration from `~/.claude` is mounted read-only
