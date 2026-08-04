package httpsource_test

import (
	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/httpsource"
)

// Compile-time confirmation that the exported Client satisfies the root
// source-agnostic capture contract when referenced by its qualified name.
var _ audiostream.Source = (*httpsource.Client)(nil)
