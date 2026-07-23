package rtsp

import (
	"errors"
	"strconv"

	audiostream "github.com/tphakala/go-audio-stream"
)

// RTSP status codes (RFC 2326 section 7.1.1, with the RTSP-specific codes
// that fall outside the HTTP status space). Only the codes this client
// reasons about are named; ClassifyStatus handles any numeric code.
const (
	StatusContinue                  = 100
	StatusOK                        = 200
	StatusCreated                   = 201
	StatusLowOnStorageSpace         = 250
	StatusMultipleChoices           = 300
	StatusMovedPermanently          = 301
	StatusMovedTemporarily          = 302
	StatusSeeOther                  = 303
	StatusNotModified               = 304
	StatusUseProxy                  = 305
	StatusBadRequest                = 400
	StatusUnauthorized              = 401
	StatusPaymentRequired           = 402
	StatusForbidden                 = 403
	StatusNotFound                  = 404
	StatusMethodNotAllowed          = 405
	StatusNotAcceptable             = 406
	StatusProxyAuthRequired         = 407
	StatusRequestTimeout            = 408
	StatusGone                      = 410
	StatusPreconditionFailed        = 412
	StatusRequestEntityTooLarge     = 413
	StatusRequestURITooLong         = 414
	StatusUnsupportedMediaType      = 415
	StatusParameterNotUnderstood    = 451
	StatusConferenceNotFound        = 452
	StatusNotEnoughBandwidth        = 453
	StatusSessionNotFound           = 454
	StatusMethodNotValidInThisState = 455
	StatusHeaderFieldNotValid       = 456
	StatusInvalidRange              = 457
	StatusParameterIsReadOnly       = 458
	StatusAggregateOpNotAllowed     = 459
	StatusOnlyAggregateOpAllowed    = 460
	StatusUnsupportedTransport      = 461
	StatusDestinationUnreachable    = 462
	StatusKeyManagementFailure      = 463
	StatusInternalServerError       = 500
	StatusNotImplemented            = 501
	StatusBadGateway                = 502
	StatusServiceUnavailable        = 503
	StatusGatewayTimeout            = 504
	StatusRTSPVersionNotSupported   = 505
	StatusOptionNotSupported        = 551
)

// ErrNilResponse is returned by ClassifyStatus when it is handed a nil
// response. The parsers here never return one, so this reports a caller
// mistake without breaking the package's no-panic contract.
var ErrNilResponse = errors.New("rtsp: nil response")

// ResponseError reports a non-success RTSP status the client did not
// otherwise special-case (not a 2xx, not a 3xx redirect, not a 401).
type ResponseError struct {
	// Code is the numeric status.
	Code int
	// Reason is the server's reason phrase.
	Reason string
}

// Error satisfies error.
func (e *ResponseError) Error() string {
	if e.Reason == "" {
		return "rtsp: status " + strconv.Itoa(e.Code)
	}
	return "rtsp: status " + strconv.Itoa(e.Code) + " " + e.Reason
}

// UnauthorizedError reports a 401 response. Challenges holds the raw
// WWW-Authenticate header field values (one string per header line,
// unparsed); the client feeds them to ParseChallenges to run the auth flow.
type UnauthorizedError struct {
	// Challenges are the raw WWW-Authenticate header values, in order.
	Challenges []string
}

// Error satisfies error.
func (e *UnauthorizedError) Error() string {
	return "rtsp: 401 unauthorized"
}

// ClassifyStatus maps resp to the error the client should act on:
//   - 1xx or 2xx: nil (success).
//   - 3xx: *audiostream.RedirectError with Location from the header (the
//     Location may be "" when the server omitted it; following the redirect,
//     and refusing an rtsps->rtsp downgrade, is M4b policy, not decided here).
//   - 401: *UnauthorizedError carrying the raw WWW-Authenticate values.
//   - any other code: *ResponseError.
//
// It reads resp.StatusCode, resp.Header and, for an unclassified code,
// resp.Reason. A nil resp yields ErrNilResponse rather than a panic: the
// parsers in this package never produce one, so a nil here is a caller
// mistake that should surface as an error like any other.
func ClassifyStatus(resp *Response) error {
	if resp == nil {
		return ErrNilResponse
	}
	code := resp.StatusCode
	switch {
	case code >= 100 && code < 300:
		return nil
	case code >= 300 && code < 400:
		return &audiostream.RedirectError{Location: resp.Header.Get("Location")}
	case code == StatusUnauthorized:
		return &UnauthorizedError{Challenges: resp.Header.Values(wwwAuthenticateHeader)}
	default:
		return &ResponseError{Code: code, Reason: resp.Reason}
	}
}

// wwwAuthenticateHeader is the field name carrying 401 auth challenges.
const wwwAuthenticateHeader = "WWW-Authenticate"
