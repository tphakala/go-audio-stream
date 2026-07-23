package rtsp

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Fixture strings reused across the auth tests, factored out to keep the
// linter's duplicate-string check quiet.
const (
	// hikNonce is a Hikvision-style hex nonce reused across the no-qop fixtures.
	hikNonce      = "4e4f4e43452d31323334353637383930"
	testRealmHik  = "IP Camera(12345)"
	testMethodGET = "GET"
	testUserAdmin = "admin"
	testPassword  = "password"
)

func TestParseChallenges(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   []Challenge
	}{
		{
			name:   "single digest quoted params",
			values: []string{`Digest realm="test", nonce="abc", qop="auth", algorithm=SHA-256, opaque="xyz"`},
			want: []Challenge{{
				Scheme: AuthDigest,
				Realm:  "test",
				Params: map[string]string{
					paramRealm:     "test",
					paramNonce:     "abc",
					paramQOP:       qopAuth,
					paramAlgorithm: algSHA256,
					paramOpaque:    "xyz",
				},
			}},
		},
		{
			name:   "legacy no-qop hikvision",
			values: []string{`Digest realm="IP Camera(12345)", nonce="` + hikNonce + `"`},
			want: []Challenge{{
				Scheme: AuthDigest,
				Realm:  testRealmHik,
				Params: map[string]string{
					paramRealm: testRealmHik,
					paramNonce: hikNonce,
				},
			}},
		},
		{
			name:   "basic only",
			values: []string{`Basic realm="camera"`},
			want: []Challenge{{
				Scheme: AuthBasic,
				Realm:  "camera",
				Params: map[string]string{paramRealm: "camera"},
			}},
		},
		{
			name:   "two challenges in one line",
			values: []string{`Digest realm="r", nonce="n", Basic realm="b"`},
			want: []Challenge{
				{
					Scheme: AuthDigest,
					Realm:  "r",
					Params: map[string]string{paramRealm: "r", paramNonce: "n"},
				},
				{
					Scheme: AuthBasic,
					Realm:  "b",
					Params: map[string]string{paramRealm: "b"},
				},
			},
		},
		{
			name:   "two header lines",
			values: []string{`Digest realm="r", nonce="n"`, `Basic realm="b"`},
			want: []Challenge{
				{
					Scheme: AuthDigest,
					Realm:  "r",
					Params: map[string]string{paramRealm: "r", paramNonce: "n"},
				},
				{
					Scheme: AuthBasic,
					Realm:  "b",
					Params: map[string]string{paramRealm: "b"},
				},
			},
		},
		{
			name:   "comma inside quoted value",
			values: []string{`Digest realm="a,b", nonce="n"`},
			want: []Challenge{{
				Scheme: AuthDigest,
				Realm:  "a,b",
				Params: map[string]string{paramRealm: "a,b", paramNonce: "n"},
			}},
		},
		{
			name:   "unknown scheme skipped",
			values: []string{`Negotiate abc==, Digest realm="r", nonce="n"`},
			want: []Challenge{{
				Scheme: AuthDigest,
				Realm:  "r",
				Params: map[string]string{paramRealm: "r", paramNonce: "n"},
			}},
		},
		{
			name:   "malformed among valid",
			values: []string{`Garbage, Digest realm="r", nonce="n"`},
			want: []Challenge{{
				Scheme: AuthDigest,
				Realm:  "r",
				Params: map[string]string{paramRealm: "r", paramNonce: "n"},
			}},
		},
		{
			name:   "empty values",
			values: nil,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseChallenges(tt.values)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseChallenges(%q) =\n  %#v\nwant\n  %#v", tt.values, got, tt.want)
			}
		})
	}
}

func TestChallengeStale(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"stale true lowercase", `Digest realm="r", nonce="n2", stale=true`, true},
		{"stale TRUE uppercase", `Digest realm="r", nonce="n2", stale=TRUE`, true},
		{"stale quoted true", `Digest realm="r", nonce="n2", stale="true"`, true},
		{"stale absent", `Digest realm="r", nonce="n2"`, false},
		{"stale false", `Digest realm="r", nonce="n2", stale=false`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := ParseChallenges([]string{tt.value})
			if len(cs) != 1 {
				t.Fatalf("expected 1 challenge, got %d", len(cs))
			}
			if got := cs[0].Stale(); got != tt.want {
				t.Fatalf("Stale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectChallenge(t *testing.T) {
	basic := Challenge{Scheme: AuthBasic, Realm: "b", Params: map[string]string{paramRealm: "b"}}
	md5 := Challenge{Scheme: AuthDigest, Realm: "r", Params: map[string]string{paramRealm: "r", paramNonce: "n", paramAlgorithm: algMD5}}
	sha := Challenge{Scheme: AuthDigest, Realm: "r", Params: map[string]string{paramRealm: "r", paramNonce: "n", paramAlgorithm: algSHA256}}
	digestNoAlg := Challenge{Scheme: AuthDigest, Realm: "r", Params: map[string]string{paramRealm: "r", paramNonce: "n"}}

	tests := []struct {
		name   string
		in     []Challenge
		want   Challenge
		wantOK bool
	}{
		{"sha256 preferred over md5 and basic", []Challenge{basic, md5, sha}, sha, true},
		{"sha256 preferred regardless of order", []Challenge{sha, md5, basic}, sha, true},
		{"md5 digest when no sha256", []Challenge{basic, md5}, md5, true},
		{"digest without algorithm beats basic", []Challenge{basic, digestNoAlg}, digestNoAlg, true},
		{"basic only", []Challenge{basic}, basic, true},
		{"none usable", nil, Challenge{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SelectChallenge(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("SelectChallenge ok = %v, want %v", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SelectChallenge = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// RFC 7616 section 3.9.1 inputs, shared by the SHA-256 and MD5 Digest
// vectors: qop=auth, method GET, uri /dir/index.html, nc 00000001.
func rfc7616Challenge(algorithm string) Challenge {
	return Challenge{
		Scheme: AuthDigest,
		Realm:  "http-auth@example.org",
		Params: map[string]string{
			paramRealm:     "http-auth@example.org",
			paramNonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
			paramQOP:       qopAuth,
			paramAlgorithm: algorithm,
			paramOpaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		},
	}
}

func rfc7616Input() DigestInput {
	return DigestInput{
		Method:     testMethodGET,
		URI:        "/dir/index.html",
		CNonce:     "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		NonceCount: 1,
	}
}

var rfc7616Creds = Credentials{Username: "Mufasa", Password: "Circle of Life"}

func TestKVQuotedEscaping(t *testing.T) {
	t.Parallel()
	// A value carrying a double-quote and a backslash must be escaped so it
	// cannot break out of the quoted-string or forge extra auth-params:
	// backslash first (a"b\c -> a"b\\c), then double-quote (-> a\"b\\c).
	if got := kvQuoted("username", "a\"b\\c"); got != `username="a\"b\\c"` {
		t.Errorf("kvQuoted escaping = %q, want %q", got, `username="a\"b\\c"`)
	}
	// A value with neither character is wrapped verbatim (the RFC 7616
	// golden vectors below rely on this).
	if got := kvQuoted("realm", "http-auth@example.org"); got != `realm="http-auth@example.org"` {
		t.Errorf("kvQuoted plain = %q, want %q", got, `realm="http-auth@example.org"`)
	}
}

func TestBasicAuthorization(t *testing.T) {
	tests := []struct {
		name  string
		creds Credentials
		want  string
	}{
		{"rfc 7617 example", Credentials{Username: "Aladdin", Password: "open sesame"}, "Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ=="},
		{"admin password", Credentials{Username: testUserAdmin, Password: testPassword}, "Basic YWRtaW46cGFzc3dvcmQ="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := basicAuthorization(tt.creds); got != tt.want {
				t.Fatalf("basicAuthorization = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDigestAuthorizationSHA256(t *testing.T) {
	got, err := digestAuthorization(rfc7616Challenge(algSHA256), rfc7616Creds, rfc7616Input())
	if err != nil {
		t.Fatalf("digestAuthorization error: %v", err)
	}
	want := `Digest username="Mufasa", realm="http-auth@example.org", ` +
		`nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", uri="/dir/index.html", ` +
		`algorithm=SHA-256, qop=auth, nc=00000001, ` +
		`cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", ` +
		`response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1", ` +
		`opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`
	if got != want {
		t.Fatalf("digestAuthorization (SHA-256) =\n  %q\nwant\n  %q", got, want)
	}
	for _, sub := range []string{
		`response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`,
		`algorithm=SHA-256`,
		`qop=auth`,
		`nc=00000001`,
		`cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("SHA-256 header missing %q", sub)
		}
	}
}

func TestDigestAuthorizationMD5(t *testing.T) {
	got, err := digestAuthorization(rfc7616Challenge(algMD5), rfc7616Creds, rfc7616Input())
	if err != nil {
		t.Fatalf("digestAuthorization error: %v", err)
	}
	if !strings.Contains(got, `response="8ca523f5e9506fed4657c9700eebdbec"`) {
		t.Fatalf("MD5 header = %q, want response 8ca523f5e9506fed4657c9700eebdbec", got)
	}
	if !strings.Contains(got, `algorithm=MD5`) {
		t.Errorf("MD5 header missing algorithm=MD5: %q", got)
	}
}

func TestDigestAuthorizationLegacyNoQOP(t *testing.T) {
	challenge := Challenge{
		Scheme: AuthDigest,
		Realm:  testRealmHik,
		Params: map[string]string{
			paramRealm: testRealmHik,
			paramNonce: hikNonce,
		},
	}
	in := DigestInput{Method: "DESCRIBE", URI: "rtsp://192.168.1.64:554/Streaming/Channels/101"}
	got, err := digestAuthorization(challenge, Credentials{Username: testUserAdmin, Password: testPassword}, in)
	if err != nil {
		t.Fatalf("digestAuthorization error: %v", err)
	}
	if !strings.Contains(got, `response="c4605499acdec65d8a0119b26ee86e34"`) {
		t.Fatalf("legacy header = %q, want response c4605499acdec65d8a0119b26ee86e34", got)
	}
	for _, absent := range []string{"qop=", "nc=", "cnonce=", "algorithm="} {
		if strings.Contains(got, absent) {
			t.Errorf("legacy header must not contain %q: %q", absent, got)
		}
	}
}

func TestDigestAuthorizationNonceCountFormatting(t *testing.T) {
	in := rfc7616Input()
	in.NonceCount = 16
	got, err := digestAuthorization(rfc7616Challenge(algMD5), rfc7616Creds, in)
	if err != nil {
		t.Fatalf("digestAuthorization error: %v", err)
	}
	if !strings.Contains(got, "nc=00000010") {
		t.Fatalf("header = %q, want nc=00000010", got)
	}
}

func TestAuthorizeDispatch(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		got, err := Authorize(Challenge{Scheme: AuthBasic}, Credentials{Username: testUserAdmin, Password: testPassword}, DigestInput{})
		if err != nil {
			t.Fatalf("Authorize error: %v", err)
		}
		if got != "Basic YWRtaW46cGFzc3dvcmQ=" {
			t.Fatalf("Authorize (Basic) = %q", got)
		}
	})
	t.Run("digest", func(t *testing.T) {
		challenge := rfc7616Challenge(algSHA256)
		in := rfc7616Input()
		got, err := Authorize(challenge, rfc7616Creds, in)
		if err != nil {
			t.Fatalf("Authorize error: %v", err)
		}
		want, err := digestAuthorization(challenge, rfc7616Creds, in)
		if err != nil {
			t.Fatalf("digestAuthorization error: %v", err)
		}
		if got != want {
			t.Fatalf("Authorize (Digest) = %q, want %q", got, want)
		}
	})
}

func TestAuthorizeErrors(t *testing.T) {
	tests := []struct {
		name      string
		challenge Challenge
		in        DigestInput
		wantErr   error
	}{
		{
			name:      "qop present but cnonce empty",
			challenge: rfc7616Challenge(algMD5),
			in:        DigestInput{Method: testMethodGET, URI: "/", NonceCount: 1},
			wantErr:   ErrMissingCNonce,
		},
		{
			name: "digest without nonce",
			challenge: Challenge{
				Scheme: AuthDigest,
				Realm:  "r",
				Params: map[string]string{paramRealm: "r"},
			},
			in:      DigestInput{Method: testMethodGET, URI: "/"},
			wantErr: ErrMissingNonce,
		},
		{
			name: "unsupported sess algorithm",
			challenge: Challenge{
				Scheme: AuthDigest,
				Realm:  "r",
				Params: map[string]string{
					paramRealm:     "r",
					paramNonce:     "n",
					paramQOP:       qopAuth,
					paramAlgorithm: "SHA-256-sess",
				},
			},
			in:      rfc7616Input(),
			wantErr: ErrUnsupportedAuth,
		},
		{
			name:      "unknown scheme",
			challenge: Challenge{Scheme: AuthUnknown},
			in:        DigestInput{},
			wantErr:   ErrUnsupportedAuth,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Authorize(tt.challenge, rfc7616Creds, tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Authorize error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
