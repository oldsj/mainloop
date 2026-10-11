// Package events is the outbox and the live-update stream.
//
// # Publishing
//
// A domain change calls [Publish] with the transaction that makes the change. The event row is
// inserted and pg_notify is issued in that transaction, so the event exists exactly when the
// change commits, from any process (API, MCP, git). Publish as the last statement before commit:
// it takes an advisory transaction lock that is held until commit, which makes event ids
// commit-ordered (a reader that has seen id N has seen every earlier id), and a transaction that
// goes on to wait for other locks while holding it can deadlock.
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
// # Resuming
//
// Every stored event id is the SSE `id:`. A client that reconnects with Last-Event-ID gets every
// event of its user after that id, up to the retention window (default 24 hours), then live
// events with no gap and no duplicate. A resume point outside the window, or ahead of the newest
// event (the database was reset), is treated like a fresh connection: nothing is replayed, and
// the `connected` frame is the signal to re-read state. The web client already does that on every
// reconnect.
package events
