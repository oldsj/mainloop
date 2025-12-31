# 001 - The Attention Bottleneck

*Why attention (not compute, not code) is the real constraint in AI-assisted development*

---

## The Problem Nobody Talks About

We've been sold a narrative: AI is the force multiplier. Give Claude or GPT your codebase and watch productivity soar. 10x engineers become 100x engineers.

But here's what I've discovered after months of building with AI assistance: **the bottleneck was never the code.**

It's your attention.

Every time I context-switch to check a Slack notification, review a PR, or answer a question from an AI worker, I lose something precious. The mental model I was holding evaporates. The flow state dissolves. And no amount of AI-generated code can compensate for a fragmented human mind.

## The Context Switching Tax

Researchers have known this for decades. Task switching imposes a cognitive tax of 20-30% on performance. But in the age of AI, this tax has compounded:

- You're now monitoring multiple AI workers, not just your own work
- Each worker might need clarification, approval, or course correction
- PRs are being generated faster than you can meaningfully review them
- The inbox fills faster than the inbox empties

We've amplified output without amplifying attention. The result? A productivity paradox where more gets produced but less gets truly *done*.

## You Are The Main Thread

This realization led to the core mental model behind mainloop: **you are the main thread**.

In programming, the main thread is the primary execution path. Everything else — async tasks, background workers, event handlers — exists in service of the main thread. The main thread is the bottleneck, and that's by design.

Your consciousness works the same way. You are the single-threaded processor that can only truly focus on one thing at a time. Everything else needs to wait in a queue, be delegated, or be summarized.

The mistake most productivity tools make is treating humans like multi-threaded systems. They scatter your attention across tabs, apps, and notifications. They assume you can parallel-process.

You can't. And trying to is the source of the exhaustion.

## The Unified Inbox Solution

Mainloop inverts this. Instead of you chasing information across GitHub, Slack, email, and AI chat windows, everything flows to *you* through a single unified inbox.

The inbox is priority-ordered by what actually needs your attention:

1. **Questions and approvals** — Decisions only you can make
2. **Active tasks** — Work in progress (with live logs if you want to peek)
3. **Recent failures** — Things that broke and need your intervention
4. **Completed work** — Quick review, then it folds away
5. **History** — Everything else, collapsed until you need it

The key insight: **not everything that *could* have your attention *should* have your attention.** Most of the time, the right answer is "I trust the worker, keep going."

## Spawn Workers, Protect Flow

When you're in flow and an idea surfaces — "I should add a logout button" or "that API endpoint needs error handling" — you have two choices:

1. Context switch to handle it now (and lose flow)
2. Write it down for later (and probably forget the nuance)

Mainloop adds a third option: **spawn a worker**.

Tell Claude "spawn a task to add error handling to the /users endpoint" and keep working. The worker picks it up in the background, creates a plan, opens a draft PR, iterates until CI passes, and only then surfaces for your review.

You stay in flow. The work gets done. Your attention is protected.

## What This Means for the Future

I'm building mainloop because I think this is the future of knowledge work:

- **AI workers** handle execution at scale
- **Humans** provide direction, taste, and judgment
- **Attention management** becomes the meta-skill
- **Unified inboxes** replace scattered notifications

The developers who thrive won't be the ones who can type the fastest or remember the most APIs. They'll be the ones who can protect their attention, delegate effectively, and stay in flow while the machines handle the rest.

---

*Next entry: [002 - Memory is the New Context](002-memory-context.md) — how to have conversations that last months, not minutes.*

---

**If this resonates, I'm building mainloop in public. Follow the journey for more on AI-assisted development, attention management, and what it means to be the main thread.**

#buildinpublic #AI #developertools #productivity #attention
