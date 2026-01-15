# Roadmap

Future ideas and features under consideration.

## CI Loop Automation

Agent automatically iterates on CI failures:
- Poll GitHub Actions after each push
- On failure: analyze logs, fix, commit
- Continue until green checkmark

## GitHub Issue Planning

Agent creates/updates issues before coding:
- Problem analysis and proposed approach
- Implementation plan as "thinking out loud" space
- Links PR back to issue for context

## Project Template

Standardized repo setup for mainloop agents:

| Component      | Purpose                              |
| -------------- | ------------------------------------ |
| GitHub Actions | CI pipeline (lint, type-check, test) |
| K8s/Helm       | Preview environments per PR          |
| CNPG operator  | Dynamic test databases               |
| trunk.yaml     | Unified linter config                |
