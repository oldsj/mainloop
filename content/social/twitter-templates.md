# Twitter/X Templates

Templates for Twitter/X posts and threads. Optimized for the platform's format: punchy, shareable, threadable.

---

## Template: Hook → Thread

Best for: Deep content adapted from devlogs

```
1/ [Provocative hook - the claim that makes people click]

2/ [Problem statement - relatable pain]

3/ [Key insight - what I learned]

4/ [How I'm solving it]

5/ [CTA - link to full devlog]
```

---

## Template: Single Tweet

Best for: Quick updates, insights, questions

```
[Bold statement or question]

[Optional: brief elaboration]

[Optional: link]
```

---

## Template: Build Update

Best for: Progress updates, milestones

```
Just shipped: [feature/milestone]

[1-2 lines on what it does]

[Screenshot or GIF if possible]

#buildinpublic
```

---

## Example Threads

### For 001 - The Attention Bottleneck

```
1/ AI was supposed to 10x my productivity.

Instead, it 10x'd my context switches.

Here's what I learned about the real bottleneck in AI-assisted development 🧵

2/ The myth: More AI = more output = more productive

The reality: Every AI worker needs monitoring. Every PR needs review. Every clarification needs your attention.

AI multiplies output but doesn't multiply human capacity.

3/ The bottleneck isn't code.

It's your attention.

Context switching has a 20-30% cognitive tax. In the age of AI, that tax compounds with every worker you're managing.

4/ The insight that changed everything:

You are the main thread.

Like a program's main execution path, your consciousness is single-threaded. Everything else needs to queue, delegate, or wait.

5/ So I'm building mainloop:

• One unified inbox for all AI workers
• Priority-ordered by what actually needs you
• Workers run in background, surface when done
• You stay in flow

Full write-up: [link]

#buildinpublic #AI
```

### For 002 - Memory is the New Context

```
1/ Every AI conversation dies with its context.

You build shared understanding for hours.
Then the context window fills.
Tomorrow: blank slate.

I'm building AI memory that actually persists 🧵

2/ The problem with current AI:
• Stateless by default
• Context window = hard limit
• Previous conversations? Gone
• Project history? Re-explain it

It's like working with a brilliant amnesiac.

3/ The solution: Temporal Pyramid compression

Recent = detailed
Old = summarized

Last 24h: every message
Last 7d: daily summaries
Last 30d: weekly summaries
Older: monthly summaries

4/ Fresh context stays fresh.
Old context compresses but survives.

The AI remembers what you did in October without burning tokens on October's messages.

5/ Result: Conversations that last months, not minutes.

Building this into mainloop now.

Full deep-dive on the memory strategy: [link]

#buildinpublic #AI
```

### For 003 - Plan First, Code Second

```
1/ Hot take: Letting AI code immediately is the fastest way to waste time.

Here's the workflow I use instead 🧵

2/ The problem with code-first AI:

Beautiful code that:
• Doesn't fit your architecture
• Uses patterns you avoid
• Solves the wrong problem elegantly

More time fixing than writing yourself.

3/ The fix: Plan first.

My AI workers must:
1. Create a GitHub issue with the plan
2. Wait for approval
3. Only then: code in draft PR
4. Iterate until CI passes
5. Ready for human review

4/ The plan is where understanding happens.

It's not bureaucracy. It's the AI "thinking out loud" before committing.

Changing a plan = cheap.
Changing 500 lines of implementation = expensive.

5/ Keep humans at high-leverage points:
• Approve the plan (direction)
• Review the final PR (quality)

Let AI handle the iteration in between.

Full write-up: [link]

#buildinpublic
```

---

## Single Tweet Examples

### Quick insights
```
The bottleneck in AI-assisted development isn't compute.

It's human attention.

Your context switches cost 20-30% each. AI multiplies the switches.

Building mainloop to fix this.
```

### Build updates
```
Just shipped: Temporal Pyramid memory compression in mainloop

• Last 24h: full detail
• Last week: daily summaries
• Older: progressive compression

Conversations that last months, not minutes.

#buildinpublic
```

### Questions to spark discussion
```
What's the hardest part of working with AI assistants?

For me it's context - every conversation starts from zero.

Building persistent memory into mainloop to fix this.
```

### Observations
```
Things AI is good at:
• Writing code fast
• Explaining concepts
• Finding patterns

Things AI needs humans for:
• Deciding WHAT to build
• Maintaining coherent architecture
• Knowing when to break the rules

The future is orchestration, not replacement.
```

---

## Formatting Guidelines

### What Works on Twitter

- **First line is everything**: Hook or die
- **Short sentences**: One idea per line
- **White space**: Line breaks between thoughts
- **Numbers work**: Lists, stats, quantified claims
- **Threads > long tweets**: Break it up

### Thread Structure

- **Tweet 1**: Hook (provocative, counterintuitive, or promise-based)
- **Tweet 2-3**: Problem setup
- **Tweet 3-4**: Insight or solution
- **Last tweet**: CTA (link, follow, question)

### What to Avoid

- Long paragraphs
- Too many hashtags (1-2 max per tweet)
- Threads longer than 6-7 tweets
- Emoji overuse
- Engagement bait ("RT if you agree")

### Hashtags

Use sparingly:
- `#buildinpublic` - on updates and launches
- `#AI` - on relevant technical content
- Avoid generic tags like `#coding` or `#tech`

---

## Posting Schedule

Optimal times:
- **Weekday mornings**: 9-10 AM ET
- **Weekday evenings**: 4-6 PM ET

Avoid: Late night, early morning, weekends (lower developer engagement)

Frequency:
- Threads: 1-2 per week
- Single tweets: 3-5 per week
- Quote tweets/replies: As natural

---

## Cross-Platform Strategy

1. **Write devlog** (canonical, in repo)
2. **LinkedIn post** (professional, longer)
3. **Twitter thread** (punchy, shareable)
4. **Single tweets** (extract key insights)

All link back to the devlog for depth.
