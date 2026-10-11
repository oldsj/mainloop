// Package events is the outbox and the live-update stream.
//
// # Publishing
//
// A domain change publishes with the transaction that makes the change. The event row is
// inserted and pg_notify is issued in that transaction, so the event exists exactly when the
// change commits, from any process (API, MCP, git). Domain slices call [InTxPublish]: the domain
// function does its writes and returns the events they imply, and the helper publishes them after
// it returns, as the last statements before commit. That order matters. The insert takes an
// advisory transaction lock (inside the INSERT statement itself, before the id is drawn) that is
// held until commit, which makes event ids commit-ordered (a reader that has seen id N has seen
// every earlier id); a transaction that goes on to wait for other locks while holding it can
// deadlock. Code that already has its own transaction may call [Publish] directly, last.
//
// # Fan-out
//
// serve-api runs a [Source], which reads new rows from the outbox whenever the notify listener
// wakes it (and on a poll, as the backstop) and hands them to the [Hub]. The hub delivers each
// event to the subscribers of its user through bounded buffers. A publisher never waits for a
// subscriber: when a subscriber's buffer is full the hub drops that subscriber, which ends its
// stream, and the browser reconnects with Last-Event-ID and is replayed from the outbox. That is
// the drop-and-resync rule; it makes a slow client cost itself a reconnect and nobody else
// anything.
//
// The hub refuses subscribers until the source has read its starting cursor ([Hub.MarkReady]):
// a stream that connected earlier would miss events committed before the source started (never
// dispatched, and after the stream's own replay point). The endpoint answers 503 and the browser
// retries.
//
// # Resuming
//
// Every stored event id is the SSE `id:`. A client that reconnects with Last-Event-ID gets every
// event of its user after that id, up to the retention window (default 24 hours), then live
// events with no gap and no duplicate. A resume point outside the window, or ahead of the newest
// event (the database was reset), is treated like a fresh connection: nothing is replayed, and
// the `connected` frame is the signal to re-read state. The web client already does that on every
// reconnect. If retention prunes past a replay that is still running, the stream ends; the
// client's resume point is then below the floor, so its next attempt starts fresh.
package events
