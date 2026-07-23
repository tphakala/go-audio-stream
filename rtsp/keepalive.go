package rtsp

import (
	"time"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// rtcpReportInterval is the fixed cadence at which the client emits RTCP
// Receiver Reports, independent of the RTSP keepalive. Some camera firmware
// (Reolink-class) stalls without RTCP activity roughly this often.
const rtcpReportInterval = 5 * time.Second

// keepaliveInterval is the RTSP keepalive period, half the negotiated session
// timeout with a 1s floor.
func (c *Client) keepaliveInterval() time.Duration {
	c.mu.Lock()
	timeout := c.sessionTimeout
	c.mu.Unlock()
	interval := timeout / 2
	if interval < time.Second {
		interval = time.Second
	}
	return interval
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
// ignored; the reader's next read will fail and funnel shutdown.
func (c *Client) sendKeepalive() {
	c.mu.Lock()
	method := c.keepaliveMethod
	reqURL := c.baseURL
	sess := c.sessionID
	c.mu.Unlock()
	if method == "" {
		method = methodOptions
	}

	req := &Request{Method: method, URL: reqURL, CSeq: int(c.cseq.Add(1)), Header: Header{}}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if sess != "" {
		req.Header.Set("Session", sess)
	}
	// A keepalive must carry credentials once authentication is active. It does
	// not go through roundTrip, so nothing else would attach them, and a camera
	// that challenges every request would answer 401 to every keepalive; the
	// reply is dropped as an unknown CSeq, so the session would silently expire
	// at the server's own timeout with the client believing it had kept it warm.
	c.attachAuthorization(req)
	raw, err := MarshalRequest(req)
	if err != nil {
		return
	}
	_ = c.writeMessage(raw)
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

// dlsrUnits converts an elapsed duration into DLSR units of 1/65536 seconds
// (RFC 3550). A non-positive duration is 0; the shift keeps the arithmetic in
// integers, and the 32-bit truncation matches the DLSR field width.
func dlsrUnits(elapsed time.Duration) uint32 {
	if elapsed <= 0 {
		return 0
	}
	return uint32((uint64(elapsed) << 16) / uint64(time.Second))
}
