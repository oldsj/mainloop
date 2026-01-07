# Test Specifications

Human-readable test plans for mainloop. The Playwright Generator agent transforms these specs into executable tests.

## How to Use

1. Write test scenarios in markdown format (see examples below)
2. Run the Generator agent: "Generate tests from specs/inbox-management.md"
3. Tests are created in `tests/` directory
4. Run tests: `pnpm test`

## Spec Format

```markdown
# Feature Name Test Plan

## Application Overview

Brief description of what this feature does.

## Test Scenarios

### 1. Scenario Category

#### 1.1 Specific Test Case

**Seed:** tests/seed.spec.ts

**Steps:**

1. Action to perform
2. Another action

**Expected:**

- What should happen
- Another expectation
```

## Available Specs

- [inbox-management.md](./inbox-management.md) - Inbox panel and queue items
- [task-interactions.md](./task-interactions.md) - Question answering and plan review
- [mobile-navigation.md](./mobile-navigation.md) - Mobile tab bar navigation
- [github-tool.md](./github-tool.md) - Chat agent GitHub integration
