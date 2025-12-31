# Changelog

All notable changes to mainloop will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Build-in-public content strategy with devlog and social templates
- Temporal Pyramid memory compression strategy (MEMORY_STRATEGY.md)
- spawn_task MCP tool for Claude workers
- Interactive plan review in inbox before worker execution
- Unified inbox with priority-ordered attention management
- Worker CI verification loop (iterate until GitHub Actions pass)
- DBOS durable workflow orchestration
- Draft PR pattern for AI worker implementations
- Mobile-first responsive UI with desktop sidebar

### Architecture
- Coordinator/worker pattern with Haiku for triage, Opus for deep work
- Isolated K8s namespaces per worker for security and reproducibility
- Tailscale Gateway routing (no public ingress)
- PostgreSQL for conversation memory and compaction

### Documentation
- Devlog entries covering the vision and paradigm shift
- LinkedIn and Twitter/X templates for social media presence
- Updated CLAUDE.md with devlog conventions

## Future Milestones

### Planned
- Semantic search across conversation history
- Multi-repo worker coordination
- Team features (shared inboxes, collaborative workers)
- Hosted version (early access)
- Adaptive compression (importance-based detail preservation)

---

*This changelog is maintained as part of mainloop's build-in-public strategy.*
