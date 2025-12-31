# 005 - Meta Development

*The recursive journey of building an AI coordination system with AI assistance*

---

## The Recursive Moment

There's something delightfully strange about my current development setup:

I'm using Claude Code to build mainloop — a system designed to coordinate Claude workers.

It's turtles all the way down. The AI is helping build the system that will coordinate AI. The patterns I'm discovering inform the product I'm creating. The product I'm creating shapes how I work with AI.

This is **meta-development**: developing the development process while developing.

## What Actually Works

After months of this recursive experiment, here's what I've learned about AI-assisted development:

**Context is everything.** CLAUDE.md and README.md aren't optional documentation — they're the foundation of every conversation. When I update them, the AI immediately becomes more helpful. When they're stale, every interaction requires more explanation.

**Planning prevents churn.** When I let Claude jump straight to code, I get plausible-looking implementations that miss the point. When I insist on plans first (in GitHub issues), the implementations actually fit the architecture. The planning phase is where understanding happens.

**Incremental beats ambitious.** Small, focused tasks work. "Add error handling to this endpoint" succeeds. "Refactor the entire auth system" spins. The AI is a sprinter, not a marathoner. Design the system for sprints.

**Verification is non-negotiable.** AI-generated code needs to run against tests. Every time. The CI loop isn't overhead — it's the quality gate. Workers that iterate until CI passes produce better code than workers that generate once and stop.

**The main thread matters.** I can have multiple workers running, but I'm still the bottleneck. The inbox helps, but ultimately I need to review plans, approve PRs, and provide direction. Protecting my attention is protecting the whole system.

## What Doesn't Work (Yet)

Some patterns I've tried that haven't panned out:

**Fully autonomous operation.** The vision of "describe it once and walk away" remains elusive. Workers need human checkpoints. Clarification questions arise. Plans need approval. Maybe this improves with better models, but today, human-in-the-loop is essential.

**Long-running context.** Even with the Temporal Pyramid, there's context that gets lost. Subtle architectural decisions. Implicit preferences. The "feel" of the codebase. Summarization helps but doesn't fully solve this.

**Cross-task coordination.** When multiple workers are active on related tasks, they don't know about each other. Worker A might refactor something Worker B depends on. This is a hard distributed systems problem I haven't cracked.

**Taste transfer.** I know what "good code" looks like for this project. Explaining that to an AI is surprisingly hard. It's not just "follow these patterns" — it's knowing when to break patterns. When to add abstraction. When to keep it simple.

## The SaaS Vision

Here's where this is heading:

Mainloop today is a personal tool. My attention management system. My AI workforce.

But the patterns are generalizable. Other developers face the same problems:
- Context switching killing productivity
- AI assistants that forget everything
- Code reviews that can't keep up with AI-generated PRs
- No central place to manage AI-assisted work

The vision is mainloop as a service: **your own AI-coordinated development environment**.

- Persistent conversations that last months
- Workers that understand your codebase deeply
- A unified inbox across all your projects
- CI-verified implementations, not just generated code

Building in public is partly about sharing the journey. It's also about finding the others — the developers who feel these problems and want solutions.

## The Self-Amplifying Effect

There's a compounding effect happening:

1. I build a feature in mainloop (using AI assistance)
2. That feature makes mainloop better at coordinating AI
3. Better coordination means I can build the next feature faster
4. Repeat

Each improvement to the attention management system improves my attention. Each improvement to worker coordination improves how workers help me. The tool and the process are the same loop.

This is what I mean by self-amplifying development. The product is also the process.

## Lessons for Others

If you're building with AI, some takeaways:

**Invest in documentation.** Your CLAUDE.md or equivalent is multiplied across every AI interaction. Time spent here pays dividends for months.

**Design for small tasks.** Structure your work so AI can pick up discrete, well-scoped pieces. This is good engineering anyway, but AI makes it essential.

**Build verification into the workflow.** Don't review AI code manually. Make CI catch the issues. Reserve your attention for what CI can't verify: architecture, design, intent.

**Embrace the bottleneck.** You are the main thread. That's not a bug. Design systems that respect this — unified inboxes, spawn-when-ready patterns, async workers that wait for you.

**Build what you need.** The best development tools come from developers who solve their own problems first. If you're frustrated by something, others probably are too.

## What's Next

I'm continuing to build mainloop in public. Upcoming work:

- Semantic search across conversation history
- Multi-repo worker coordination
- Team features (shared inboxes, collaborative workers)
- The hosted version (early access coming)

If any of this resonates — the attention management problem, the persistent AI collaboration dream, the meta-recursive development journey — follow along.

The future of development isn't humans vs. AI. It's humans orchestrating AI. And learning how to orchestrate well is the skill that will matter most.

---

**This is entry 005 of the mainloop devlog. The full series is available at [/content/devlog](./README.md).**

**Building in public. If you're exploring similar ideas, I'd love to hear from you.**

#buildinpublic #AI #developertools #saas #indiehacker
