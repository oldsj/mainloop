package devloop

// FetchArchiveMax is FetchArchive with a caller-chosen archive cap, so a test can exceed it
// without building a repository hundreds of megabytes large.
var FetchArchiveMax = fetchArchive
