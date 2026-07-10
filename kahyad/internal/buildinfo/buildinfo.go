// Package buildinfo holds build-time version metadata shared by kahyad and the kahya CLI.
package buildinfo

// Version is the build's version string. It defaults to "0.0.0-dev" for
// unreleased/manual builds; `make build` overrides it via
// -ldflags "-X kahya/kahyad/internal/buildinfo.Version=<git describe>" so
// GET /health can report a real version (W12-01 step 3).
var Version = "0.0.0-dev"
