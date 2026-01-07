# GitHub Tool Test Plan

## Application Overview

The chat handler provides a `github` tool that allows the main thread agent to read repository information and manage GitHub issues directly, without spawning a worker task. This was added because users asking about GitHub issues/PRs were told "I don't have access to GitHub" when the chat agent only had the `spawn_task` tool.

**Tool Actions:**

- `list_issues` - List open issues for a repository
- `list_prs` - List open PRs for a repository
- `get_repo` - Get repository metadata
- `get_issue` - Get details of a specific issue
- `create_issue` - Create a new issue
- `update_issue` - Update an existing issue
- `add_comment` - Add a comment to an issue

## Test Scenarios

### 1. Info Requests Use GitHub Tool

**Seed:** tests/seed.spec.ts

#### 1.1 List Issues Without Spawn Offer

**Steps:**

1. Send message: "List open issues on https://github.com/oldsj/mainloop"
2. Wait for response

**Expected:**

- Response contains issue information
- Response does NOT say "don't have access" or "cannot access"
- Response does NOT offer to spawn a task or worker
