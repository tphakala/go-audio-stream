module github.com/tphakala/go-audio-stream/cmd/stream-doctor

go 1.26.3

replace github.com/tphakala/go-audio-stream => ../..

require github.com/tphakala/go-audio-stream v0.0.0-00010101000000-000000000000

require (
	github.com/tphakala/go-aac v0.3.0 // indirect
	github.com/tphakala/go-opus v0.1.2 // indirect
	github.com/tphakala/go-wav v0.3.0 // indirect
	github.com/tphakala/simd v1.5.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)
