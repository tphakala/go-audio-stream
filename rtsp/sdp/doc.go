// Package sdp parses the subset of SDP (RFC 8866) that RTSP DESCRIBE
// responses need: session and media sections, a=control, a=rtpmap and
// a=fmtp attributes, with strict size caps because input is untrusted.
package sdp
