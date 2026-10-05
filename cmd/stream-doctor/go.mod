module github.com/tphakala/go-audio-stream/cmd/stream-doctor

go 1.27

replace github.com/tphakala/go-audio-stream => ../..

require (
	github.com/tphakala/go-aac v0.7.0
	github.com/tphakala/go-audio-stream v0.6.0
	github.com/tphakala/go-opus v1.1.0
	github.com/tphakala/go-wav v1.1.0
)

require (
	github.com/tphakala/simd v1.11.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)
