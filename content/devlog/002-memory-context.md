# 002 - Memory is the New Context

*How the Temporal Pyramid solves the "conversation dies with context" problem*

---

## The Ephemeral Conversation Problem

Here's a frustrating truth about working with AI: every conversation is a fresh start.

You spend hours building shared context with Claude. You explain your architecture, your naming conventions, why you chose PostgreSQL over MongoDB, why that particular function is named that particular way. Claude becomes genuinely helpful, understanding your project deeply.

Then the context window fills. The conversation ends. And tomorrow, you start from zero.

It's like having a brilliant colleague with amnesia. Every morning, they forget who you are.

## Why This Matters More Than You Think

The cost isn't just the time spent re-explaining. It's the *loss of nuance*.

When you work with someone over months, they understand not just what you've built, but *why*. They remember the dead ends you tried. They know which patterns emerged from pain and which from preference. They've absorbed the implicit style guide that no document captures.

AI assistants today can't do this. They're stateless. And the workaround — pasting project context into every conversation — creates its own problems:

- Consumes precious context window tokens
- Loses temporal ordering (what came first vs. what you decided recently)
- Flattens everything to the same importance
- Doesn't scale beyond small projects

## The Temporal Pyramid

Mainloop's answer is what I call the **Temporal Pyramid**: a memory compression strategy that keeps recent details fresh while compressing older context into increasingly dense summaries.

```
┌─────────────────────────────────────────┐
│  Last 24 hours: FULL DETAIL             │  ← Every message preserved
│  - Complete tool uses, files, context   │
│  - All the nuance of the current work   │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Last 7 days: DAILY SUMMARIES           │  ← 1 summary per day
│  - Key decisions made                   │
│  - Features implemented                 │
│  - Problems solved                      │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Last 30 days: WEEKLY SUMMARIES         │  ← 1 summary per week
│  - Major milestones                     │
│  - Architecture changes                 │
│  - Project direction shifts             │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Older: MONTHLY SUMMARIES               │  ← 1 summary per month
│  - High-level project evolution         │
│  - Key learnings and strategic decisions│
└─────────────────────────────────────────┘
```

The insight: **importance decays with time, but context compounds**.

You don't need to remember every keystroke from six months ago. But you absolutely need to know that six months ago you decided to use DBOS for durable workflows because of X, and that decision shaped everything since.

## How It Works in Practice

Every conversation with mainloop loads context intelligently:

1. **System prompt** — The base personality and capabilities
2. **Project files** — CLAUDE.md, README.md, and key documentation
3. **Temporal pyramid** — Recent full detail, older summaries

The result: Claude understands both what you're doing *right now* and what you've done *over months*. It can say "this is similar to what we did with authentication in October" without burning tokens on every authentication-related message from October.

And when you need to zoom in? Ask "what did we do on Tuesday?" and mainloop can pull those full messages from storage. The summaries are indexes, not deletions.

## Conversations That Last Months

This changes the relationship with AI fundamentally.

Instead of episodic, transactional interactions, you get a persistent collaborator. The conversation doesn't die when the context window fills — it compresses and continues. The AI remembers your project history not as a static document, but as a living, temporally-aware narrative.

Some implications:

- **Onboarding compounds**: Early explanations pay dividends for months
- **Decisions have memory**: "Why did we do it this way?" has an answer
- **Style emerges naturally**: The AI learns your preferences from exposure
- **Context costs drop**: Older work is compressed, recent work is detailed

## The Technical Reality

Building this isn't trivial. The summarization needs to be:

- **Accurate**: Can't lose important decisions
- **Temporally-aware**: Must preserve when things happened, not just what
- **Semantic-friendly**: Future search needs to find relevant summaries
- **Incrementally buildable**: Can't re-summarize everything daily

But the payoff is enormous. A project that's been active for a year with mainloop will have Claude that understands the full arc: the initial architecture decisions, the pivots, the lessons learned, and all the context that makes suggestions genuinely helpful.

## What This Means for You

If you're working with AI today, you're probably experiencing the stateless problem. Every new context window is a fresh start.

The solution isn't just "dump more context at the start." It's building systems that understand temporal relevance — what matters now vs. what shaped now.

Memory is the new context. And managing that memory intelligently is what turns an AI assistant into an AI collaborator.

---

*Next entry: [003 - Plan First, Code Second](003-plan-first.md) — why AI workers should plan in issues, not jump straight to code.*

---

**Building mainloop in public. If persistent AI memory sounds useful, follow along.**

#buildinpublic #AI #developertools #memory #context
