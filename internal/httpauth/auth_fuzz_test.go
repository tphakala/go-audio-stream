package httpauth_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/internal/httpauth"
)

// challengeSeeds are WWW-Authenticate values exercising the challenge
// tokenizer's paths: quoted params, no-qop legacy, multiple challenges per
// line, quoted commas, unknown schemes, malformed leading tokens, and the
// degenerate scheme-only and empty inputs.
var challengeSeeds = []string{
	`Digest realm="test", nonce="abc", qop="auth", algorithm=SHA-256, opaque="xyz"`,
	`Digest realm="IP Camera(12345)", nonce="4e4f4e43452d31323334353637383930"`,
	`Basic realm="camera"`,
	`Digest realm="r", nonce="n", Basic realm="b"`,
	`Digest realm="a,b", nonce="n"`,
	`Negotiate abc==, Digest realm="r", nonce="n"`,
	`Garbage, Digest realm="r", nonce="n"`,
	`Digest realm="r", nonce="n2", stale=true`,
	"",
	"Digest",
	"Digest =",
	"Basic",
}

func FuzzParseChallenges(f *testing.F) {
	for _, s := range challengeSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// The contract is total: parsing any single header value never
		// panics. A well-formed challenge must carry a usable scheme.
		for _, c := range httpauth.ParseChallenges([]string{s}) {
			if c.Scheme != httpauth.AuthBasic && c.Scheme != httpauth.AuthDigest {
				t.Fatalf("ParseChallenges(%q) returned unusable scheme %v", s, c.Scheme)
			}
		}
	})
}
