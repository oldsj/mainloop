# Roadmap

Future ideas and features under consideration.

## Agent Workflow Automation

Structured workflow for code sessions: **plan in issue → implement in draft PR → iterate until CI green → ready for human review**.

```text
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Planning  │────►│    Draft    │────►│  Iteration  │────►│   Review    │
│  (GH Issue) │     │    (PR)     │     │  (CI Loop)  │     │   (Human)   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
```

### Phases

1. **Planning (GitHub Issue)** - Agent creates/updates an issue with problem analysis, proposed approach, and implementation plan. The issue is the "thinking out loud" space before code.

2. **Draft PR** - Agent creates a draft PR linked to the issue. Implements in small, logical commits. Uses PR comments to narrate progress and decisions.

3. **Iteration (CI Loop)** - Agent polls GitHub Actions after each push. On failure: analyzes logs, fixes, commits. Continues until green checkmark.

4. **Ready for Review** - Agent marks PR ready and adds summary comment. Human reviewer steps in for final approval.

### Verification Tools

- **LSP server integration** - Real-time type/lint errors
- **`trunk` CLI** - Unified super-linter
- **Project test suites** - Via GitHub Actions

## Project Template

Standardized setup for repositories that work well with mainloop agents.

| Component      | Purpose                              |
| -------------- | ------------------------------------ |
| GitHub Actions | CI pipeline (lint, type-check, test) |
| K8s/Helm       | Preview environments per PR          |
| CNPG operator  | Dynamic test databases               |
| trunk.yaml     | Unified linter config                |
