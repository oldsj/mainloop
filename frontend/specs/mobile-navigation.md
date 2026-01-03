# Mobile Navigation Test Plan

## Application Overview

On mobile viewports (<768px), mainloop uses a bottom tab bar (MobileTabBar component) for navigation between:

- **[CHAT]** - Main conversation thread with input bar
- **[INBOX]** - Task management and queue items panel

The tab bar provides terminal-styled navigation with badge counts for items needing attention.

## Test Scenarios

### 1. Tab Bar Display

**Seed:** tests/seed.spec.ts

#### 1.1 Tab Bar Visibility on Mobile

**Steps:**
1. Load application at mobile viewport (375x667)
2. Wait for app to load

**Expected:**
- Bottom tab bar visible at bottom of screen
- Two tabs: [CHAT] and [INBOX]
- Terminal-style bracketed labels
- Fixed position (doesn't scroll with content)

#### 1.2 Tab Bar Hidden on Desktop

**Steps:**
1. Load application at desktop viewport (1280x720)
2. Observe bottom of screen

**Expected:**
- No bottom tab bar visible
- Side-by-side layout used instead
- Inbox panel always visible on desktop

### 2. Tab Navigation

**Seed:** tests/seed.spec.ts

#### 2.1 Default to Chat Tab

**Steps:**
1. Load application fresh at mobile viewport
2. Observe which view is active

**Expected:**
- [CHAT] tab is highlighted/active by default
- Chat view with conversation visible
- Input bar at bottom (above tab bar)
- Inbox view hidden

#### 2.2 Switch to Inbox Tab

**Steps:**
1. Start on [CHAT] tab (default)
2. Tap the [INBOX] tab

**Expected:**
- [INBOX] tab becomes highlighted (text-term-accent color)
- [CHAT] tab becomes muted
- Inbox panel fills main content area
- Chat view hidden completely
- Input bar hidden (inbox has its own interactions)

#### 2.3 Return to Chat Tab

**Steps:**
1. Navigate to [INBOX] tab
2. Tap the [CHAT] tab

**Expected:**
- [CHAT] tab becomes highlighted
- [INBOX] tab becomes muted
- Chat view with messages restored
- Conversation scroll position preserved
- Input bar visible again

### 3. Badge Count Display

**Seed:** tests/seed.spec.ts

#### 3.1 Inbox Badge Shows Attention Count

**Steps:**
1. When there are items needing attention (active tasks + unread queue items)
2. Observe [INBOX] tab

**Expected:**
- Badge appears on or near [INBOX] tab
- Badge shows count of items needing action
- Uses attention-grabbing color (term-accent)
- Count updates in real-time as items change

#### 3.2 Badge Hidden When Empty

**Steps:**
1. When no items need attention
2. Observe [INBOX] tab

**Expected:**
- No badge shown
- Tab appears normal without count
- Clean appearance

#### 3.3 Large Count Display

**Steps:**
1. When many items need attention (10+)
2. Observe badge

**Expected:**
- Count shows actual number or "99+" for very large counts
- Badge doesn't overflow or break layout
- Remains readable

### 4. State Persistence

**Seed:** tests/seed.spec.ts

#### 4.1 Tab State Survives Interaction

**Steps:**
1. Switch to [INBOX] tab
2. Interact with an inbox item (expand task, answer question)
3. Observe current tab

**Expected:**
- Remains on [INBOX] tab during interaction
- No unexpected tab switches
- State maintained throughout interaction

#### 4.2 Chat Input Preserved

**Steps:**
1. Start typing a message in chat
2. Switch to [INBOX] tab
3. Switch back to [CHAT] tab

**Expected:**
- Typed text preserved in input field
- Input cursor may or may not be focused
- No loss of draft message

### 5. Viewport Transitions

**Seed:** tests/seed.spec.ts

#### 5.1 Resize from Mobile to Desktop

**Steps:**
1. Load app at mobile viewport (375px wide)
2. Navigate to [INBOX] tab
3. Resize browser to desktop width (1280px)

**Expected:**
- Tab bar disappears
- Transitions to side-by-side layout
- Both chat and inbox now visible
- No jarring transition

#### 5.2 Resize from Desktop to Mobile

**Steps:**
1. Load app at desktop viewport (1280px)
2. Resize browser to mobile width (375px)

**Expected:**
- Tab bar appears
- Layout switches to tabbed view
- Default to [CHAT] tab active
- Inbox accessible via tab

### 6. Touch Interactions

**Seed:** tests/seed.spec.ts

#### 6.1 Tab Touch Target Size

**Steps:**
1. On mobile viewport with touch device
2. Attempt to tap each tab

**Expected:**
- Tabs have adequate touch target size (minimum 44px)
- Easy to tap without precision
- Clear feedback on tap (visual state change)

#### 6.2 No Accidental Swipe Navigation

**Steps:**
1. On mobile viewport
2. Swipe horizontally in content area

**Expected:**
- Swiping doesn't accidentally change tabs
- Tab changes only on explicit tap
- Content scrolls normally if applicable
