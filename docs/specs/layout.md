# Layout

Mainloop is responsive across mobile and desktop viewports.

Project pages load repository details before showing project controls. If that request fails,
the page shows the load error and a **Retry loading project** button instead of remaining on
the loading message. Retrying requests the details for the current project.

## Desktop

- Chat takes main area
- Sessions sidebar always visible on the right; the inbox and projects below it size to their content. The projects section also holds the **New workspace** control (repository, optional branch).
- No tab bar

## Mobile

- Bottom tab bar with Chat, Sessions and Inbox tabs (the Sessions tab includes the "+ agent" link)
- Chat tab active by default on load
- Tab bar hidden on desktop viewports
- Touch targets sized appropriately for mobile interaction
- Tabs switch between the Chat, Sessions and Inbox views
