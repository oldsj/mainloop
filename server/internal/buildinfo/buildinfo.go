// Package buildinfo holds facts stamped into the binary at image build time.
package buildinfo

// Revision is the git commit the binary was built from. The server Dockerfile sets it with
// -ldflags -X from the REVISION build argument; any other build reports "dev".
var Revision = "dev"
