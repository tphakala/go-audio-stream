package rtsp

import (
	"math"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// rtcpReportInterval is the fixed cadence at which the client emits RTCP
// Receiver Reports, independent of the RTSP keepalive. It is the RECOMMENDED
// minimum RTCP transmission interval of RFC 3550 section 6.2, which also
// happens to be often enough for the camera firmware (Reolink-class) that
// stalls without RTCP activity.
const rtcpReportInterval = 5 * time.Second

// keepaliveInterval is the RTSP keepalive period: half the negotiated session
// timeout, with a 1s floor.
//
// A session whose SETUP response carried no Session header at all has no
// negotiated timeout, and halving zero would leave the floor sending a keepalive
// every second for the life of the stream, 60 times the traffic intended. The
// floor is there for a server that advertises an implausibly short timeout, not
// as the value for "nothing was negotiated", so that case takes the same
// DefaultSessionTimeout ParseSession applies when the parameter is absent.
func (c *Client) keepaliveInterval() time.Duration {
	c.mu.Lock()
	timeout := c.sessionTimeout
	c.mu.Unlock()
	if timeout <= 0 {
		timeout = DefaultSessionTimeout
	}
	return max(timeout/2, time.Second)
}

// keepaliveLoop is the keepalive/RTCP timer goroutine, started by Play. It
// sends an RTSP keepalive every sessionTimeout/2 (floor 1s) using the
// negotiated method to the session base URL, fire-and-forget, and emits RTCP
// Receiver Reports every rtcpReportInterval on each non-discard track's RTCP
// channel. It reads no socket and never touches rtp.Stream: Receiver Reports
// are built from the per-track atomic snapshot the reader publishes. It exits
// when closing is signaled, and the deferred wg.Done lets the reader's teardown
// join it.
func (c *Client) keepaliveLoop() {
	defer c.wg.Done()

	keepalive := time.NewTicker(c.keepaliveInterval())
	defer keepalive.Stop()
	reports := time.NewTicker(rtcpReportInterval)
	defer reports.Stop()

	for {
		select {
		case <-c.closing:
			return
		case <-keepalive.C:
			c.sendKeepalive()
		case <-reports.C:
			c.sendReceiverReports()
		}
	}
}

// sendKeepalive writes one RTSP keepalive to the session base URL,
// fire-and-forget: it allocates a CSeq, registers no pending channel, and never
// waits for a reply. Liveness is judged by the watchdog on received frames,
// never by a keepalive reply (many cameras never reply). A write error is
// ignored; the reader's next read will fail and funnel shutdown. Credentials
// come from marshalBareRequest, which every request built outside roundTrip
// shares.
func (c *Client) sendKeepalive() {
	c.mu.Lock()
	method := c.keepaliveMethod
	reqURL := c.baseURL
	c.mu.Unlock()
	if method == "" {
		method = methodOptions
	}

	// Allocate the nonce count (inside marshalBareRequest) and write under one
	// writeMu, so a concurrent sender cannot put a later nonce count on the wire
	// ahead of this one (RFC 7616 section 3.4.3, issue #17).
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	raw, err := c.marshalBareRequest(method, reqURL)
	if err != nil {
		// Nothing to fall back to: the same request will be built the same way
		// on the next tick and fail the same way, so retrying is pointless and
		// there is no partial state to unwind. Logged rather than discarded,
		// because the visible symptom is a session that expires at the server's
		// own timeout with no other trace.
		logWarn(c.cfg.Logger, "could not marshal a keepalive; the session may expire",
			"method", method, "error", err)
		return
	}
	if err := c.armWriteDeadline(c.cfg.Timeout); err != nil {
		return
	}
	_, _ = c.conn.Write(raw)
}

// sendReceiverReports emits one RTCP Receiver Report per non-discard track on
// that track's RTCP channel, fire-and-forget, built solely from the track's
// atomic snapshot so the timer never touches the reader-owned rtp.Stream.
func (c *Client) sendReceiverReports() {
	c.mu.Lock()
	tracks := c.tracks
	reporter := c.reporterSSRC
	c.mu.Unlock()

	now := time.Now()
	for _, tr := range tracks {
		if tr.discard {
			continue
		}
		rr := tr.buildReceiverReport(reporter, now)
		raw, err := MarshalInterleaved(tr.rtcpChannel, rr.Marshal())
		if err != nil {
			// Only reachable for a channel outside 0-255 or an oversized
			// report, neither of which a later tick would fix. Skip this
			// track's report and keep the others: Receiver Reports are
			// advisory, and one track's failure is not the session's.
			logWarn(c.cfg.Logger, "could not marshal a receiver report; skipping this track",
				"track", tr.id, "error", err)
			continue
		}
		_ = c.writeMessage(raw)
	}
}

// buildReceiverReport assembles one RTCP Receiver Report for the track from its
// atomic snapshot fields (never rtp.Stream). FractionLost and Jitter are 0 in
// phase 1: the presence of a Receiver Report is what quirky cameras require;
// accurate jitter is deferred. DLSR is the delay since the last Sender Report
// in units of 1/65536 s, 0 when no SR has been seen.
func (tr *track) buildReceiverReport(reporterSSRC uint32, now time.Time) rtp.ReceiverReport {
	var dlsr uint32
	if lastSRNano := tr.lastSRUnixNano.Load(); lastSRNano != 0 {
		dlsr = dlsrUnits(now.Sub(time.Unix(0, lastSRNano)))
	}
	return rtp.ReceiverReport{
		ReporterSSRC: reporterSSRC,
		Blocks: []rtp.ReportBlock{{
			SSRC:             tr.senderSSRC.Load(),
			FractionLost:     0,
			CumulativeLost:   tr.rrCumulativeLost.Load(), // already saturated to 24 bits.
			HighestSequence:  tr.rrHighestSeq.Load(),
			Jitter:           0,
			LastSR:           tr.lastSR.Load(),
			DelaySinceLastSR: dlsr,
		}},
	}
}

// maxDLSRSeconds is the largest delay the 32-bit DLSR field can express in its
// units of 1/65536 s.
const maxDLSRSeconds = 1 << 16

// dlsrUnits converts an elapsed duration into DLSR units of 1/65536 seconds
// (RFC 3550 section 6.4.1). A non-positive duration is 0.
//
// The seconds and the fraction are scaled separately, and the whole thing
// saturates. Scaling the nanosecond count in one shift overflows a uint64 past
// about 78 hours, and even below that the product exceeds the 32-bit field past
// 18.2 hours, so a single Sender Report followed by a long silence used to wrap
// to a small delay: a 24-hour-old report read back as 5.8 hours.
func dlsrUnits(elapsed time.Duration) uint32 {
	if elapsed <= 0 {
		return 0
	}
	if elapsed >= maxDLSRSeconds*time.Second {
		return math.MaxUint32
	}
	sec := uint64(elapsed / time.Second)
	frac := uint64(elapsed % time.Second)
	return uint32(sec<<16 + (frac<<16)/uint64(time.Second))
}
