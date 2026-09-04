module github.com/tphakala/go-audio-stream/cmd/stream-doctor

go 1.27

replace github.com/tphakala/go-audio-stream => ../..

require (
	github.com/tphakala/go-aac v0.6.0
	github.com/tphakala/go-audio-stream v0.2.0
	github.com/tphakala/go-opus v1.1.0
	github.com/tphakala/go-wav v1.0.0
)

require (
	github.com/tphakala/simd v1.9.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
