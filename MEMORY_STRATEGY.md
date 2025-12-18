# Memory Strategy: Pyramid Compression

## Problem

The main thread conversation will accumulate thousands of messages over time. Loading the entire history into context for every request:
- Exceeds context window limits (200K tokens)
- Wastes tokens on irrelevant old details
- Slows down response time
- Increases API costs

## Solution: Temporal Pyramid

Keep detailed memory fresh, compress older memories into summaries at increasing time granularity.

```
┌─────────────────────────────────────────┐
│  Last 24 hours: FULL DETAIL             │  ← All messages
│  - Every message preserved              │
│  - Complete tool uses, files, context   │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Last 7 days: DAILY SUMMARIES           │  ← 1 summary/day
│  - Key decisions made                   │
│  - Features implemented                 │
│  - Problems solved                      │
│  - Files modified                       │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Last 30 days: WEEKLY SUMMARIES         │  ← 1 summary/week
│  - Major milestones                     │
│  - Architecture changes                 │
│  - Project direction shifts             │
└─────────────────────────────────────────┘
           ↓ Compression
┌─────────────────────────────────────────┐
│  Older: MONTHLY SUMMARIES               │  ← 1 summary/month
│  - High-level project evolution         │
│  - Key learnings                        │
│  - Strategic decisions                  │
└─────────────────────────────────────────┘
```

## Context Window Strategy

When loading conversation context, include:

1. **System prompt** (constant)
2. **Project context** (CLAUDE.md, README.md)
3. **Temporal pyramid**:
   - All messages from last 24 hours (~10-50 messages)
   - Daily summaries for last 7 days (~7 summaries × 500 tokens = 3.5K)
   - Weekly summaries for last 30 days (~4 summaries × 1K tokens = 4K)
   - Monthly summaries for older history (~N summaries × 2K tokens)

**Total context budget**: ~20-50K tokens for history, leaving 150K for current work.

## Summary Generation

### Daily Summary (End of Day)
Generated at midnight UTC or when 24 hours have passed since last summary.

**Prompt template**:
```
Summarize today's work in 500 tokens or less:

1. **What was accomplished**
   - Features implemented
   - Bugs fixed
   - Files modified

2. **Decisions made**
   - Technical choices
   - Architecture changes
   - Trade-offs considered

3. **Current state**
   - What's working
   - What's blocked
   - Next steps

4. **Context to preserve**
   - Important variable names
   - API endpoints added
   - Configuration changes
```

### Weekly Summary (End of Week)
Generated every Sunday or after 7 daily summaries exist.

**Prompt template**:
```
Summarize this week's progress in 1000 tokens or less:

Given these 7 daily summaries, extract:

1. **Major accomplishments**
   - What shipped
   - Key features added
   - System improvements

2. **Architecture evolution**
   - New patterns introduced
   - Tech stack changes
   - Infrastructure updates

3. **Open threads**
   - Ongoing work
   - Blocked items
   - Future considerations

4. **Learnings**
   - What worked well
   - What to avoid
   - Insights gained
```

### Monthly Summary (End of Month)
Generated on the 1st of each month or after 4 weekly summaries.

**Prompt template**:
```
Summarize this month's progress in 2000 tokens or less:

Given these 4 weekly summaries, extract:

1. **Project milestones**
   - Major releases
   - System capabilities added
   - Business goals achieved

2. **Strategic direction**
   - Product evolution
   - Architectural maturity
   - Team learnings

3. **Technical debt**
   - What was deferred
   - Why it matters
   - When to revisit
```

## Zoom-In Capability

When the user asks about a specific date or event:
1. Check if it's within the 24-hour window (return full messages)
2. Check if it's within 7 days (load that day's full messages from BigQuery)
3. Check if it's within 30 days (load that week's full messages)
4. For older dates, load the full monthly detail

**Query pattern**:
```sql
-- User asks: "What did we do on Tuesday?"
SELECT * FROM messages
WHERE conversation_id = @conv_id
  AND created_at >= '2025-12-10 00:00:00'
  AND created_at < '2025-12-11 00:00:00'
ORDER BY created_at ASC
```

## Storage Schema

### BigQuery Tables

**messages** (existing)
- Full detail forever
- Never deleted
- Queryable for zoom-in

**memory_summaries** (new)
```sql
CREATE TABLE mainloop.memory_summaries (
  id STRING NOT NULL,
  conversation_id STRING NOT NULL,
  granularity STRING NOT NULL,  -- 'daily', 'weekly', 'monthly'
  start_date TIMESTAMP NOT NULL,
  end_date TIMESTAMP NOT NULL,
  summary STRING NOT NULL,       -- The compressed summary
  message_count INT64 NOT NULL,  -- How many messages it summarizes
  created_at TIMESTAMP NOT NULL
)
```

## Implementation Phases

### Phase 1: Basic Compression (MVP)
- ✅ Store all messages in BigQuery
- ✅ Load last 24 hours for context
- ⬜ Generate daily summaries (cron job at midnight)
- ⬜ Store summaries in `memory_summaries` table
- ⬜ Load daily summaries for days 2-7 in context

### Phase 2: Multi-Level Pyramid
- ⬜ Weekly summary generation (every Sunday)
- ⬜ Monthly summary generation (1st of month)
- ⬜ Smart context assembly (pyramid loading)

### Phase 3: Semantic Search
- ⬜ Embed summaries for semantic search
- ⬜ "What did we do with authentication?" → search summaries
- ⬜ Zoom to relevant time period
- ⬜ Load full detail for that period

### Phase 4: Adaptive Compression
- ⬜ Detect important events (deployments, major decisions)
- ⬜ Keep important days at higher detail
- ⬜ Compress mundane days more aggressively
- ⬜ User can mark days as "important"

## Example Context Load

For a request on 2025-12-17:

```python
context = [
    system_prompt,
    project_files,  # CLAUDE.md, README.md

    # Last 24 hours (full detail)
    *get_messages(since="2025-12-16 00:00:00"),

    # Last 7 days (daily summaries)
    *get_summaries(granularity="daily", since="2025-12-10"),

    # Last 30 days (weekly summaries)
    *get_summaries(granularity="weekly", since="2025-11-17"),

    # Older (monthly summaries)
    *get_summaries(granularity="monthly", since="2025-01-01"),
]
```

## Benefits

1. **Bounded context**: Never exceed limits, no matter how long the conversation
2. **Relevant focus**: Recent work stays detailed, old work compressed
3. **Queryable history**: Can always dive deep when needed
4. **Cost efficient**: Fewer tokens = lower API costs
5. **Fast responses**: Less context to process
6. **Long-term memory**: Conversation can last months/years

## Future Enhancements

- **Smart importance detection**: Keep critical decisions at high detail
- **Cross-reference summaries**: "Similar to what we did in October..."
- **Trend analysis**: "We've refactored auth 3 times this year"
- **Periodic review prompts**: "This week last year you shipped X"
- **Memory consolidation**: Merge similar summaries over time
