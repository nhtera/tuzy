package tunnel

import "net/url"

// fileTargetScheme marks Options.Target as a file:// upstream (internal/fileserver behind
// Options.LocalTransport, not a network dial): FileTarget returns the placeholder URL used for it,
// and ws_stream.go checks this scheme to answer 501 instead of attempting a dial.
const fileTargetScheme = "file"

// FileTarget is the Options.Target placeholder for a file:// upstream (internal/upstream +
// internal/fileserver build the matching Options.LocalTransport). Its host/path carry no meaning —
// BuildLocalRequest joins them into the request path, so both are left empty and the served
// directory is only ever named where a human reads it (the "serving files from …" startup line).
func FileTarget() *url.URL { return &url.URL{Scheme: fileTargetScheme, Host: "local-file"} }

// A user-supplied target (bare port, host:port, http://, https:// or file://) is parsed by
// internal/upstream (upstream.Parse), which also builds the per-kind transport internal/cli wires
// into Options.LocalTransport. This package no longer parses targets itself (its former
// ParseTarget was dead code outside its own test — every real caller goes through
// internal/upstream); internal/upstream deliberately does not import this package, to avoid an
// import cycle with this package's own upstream-target test files (package tunnel).
