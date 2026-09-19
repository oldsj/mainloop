# Claude Agent Container

This container runs the Claude Code CLI for the mainloop backend.

## Current State

The service accepts requests from the backend, runs the current Claude integration in its workspace, and returns or streams results. Kubernetes jobs can also execute a bounded request and report the result through a callback.

## Usage

The backend communicates with this container via the internal Docker network.

## Configuration

- Claude Code uses the authentication configured for the runtime environment
- Workspace is mounted at `/workspace`
- Configuration from `~/.claude` is mounted read-only
