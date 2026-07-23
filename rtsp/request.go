package rtsp

import (
	"context"
	"fmt"
	"time"
)

// roundTrip sends req (assigning a fresh CSeq and adding User-Agent and, when
// set, Session), registers a pending response channel, and waits for the
// matching response, bounded by Config.Timeout merged with ctx. On a write
// error, timeout, or ctx cancellation it funnels into shutdown and returns the
// cause. It does not interpret the status; callers apply ClassifyStatus.
//
// The 401 auth retry wrapper is added in a later task; this is the bare
// round-trip Dial's OPTIONS uses.
func (c *Client) roundTrip(ctx context.Context, req *Request) (*Response, error) {
	cseq := int(c.cseq.Add(1))
	req.CSeq = cseq
	if req.Header == nil {
		req.Header = Header{}
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	c.mu.Lock()
	sess := c.sessionID
	c.mu.Unlock()
	if sess != "" {
		req.Header.Set("Session", sess)
	}

	raw, err := MarshalRequest(req)
	if err != nil {
		// A marshal failure (for example a request URI over MaxRequestURILen)
		// happens before the pending entry is registered and before any write,
		// so nothing else will ever funnel shutdown. Funnel it here as the
		// first cause so closing/done close and callers (Dial's cleanup, Wait)
		// unblock instead of parking on a bare receive forever.
		c.initiateShutdown(err)
		return nil, err
	}

	ch := make(chan *Response, 1)
	c.pendMu.Lock()
	c.pending[cseq] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, cseq)
		c.pendMu.Unlock()
	}()

	if werr := c.writeMessage(raw); werr != nil {
		wrapped := fmt.Errorf("%w: %w", ErrConnectionClosed, werr)
		c.initiateShutdown(wrapped)
		return nil, wrapped
	}

	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		c.initiateShutdown(ctx.Err())
		return nil, ctx.Err()
	case <-timer.C:
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		terr := fmt.Errorf("%w after %v", ErrRequestTimeout, c.cfg.Timeout)
		c.initiateShutdown(terr)
		return nil, terr
	case <-c.done:
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		if te := c.termError(); te != nil {
			return nil, te
		}
		return nil, ErrConnectionClosed
	}
}

// drain reports whether a response was already delivered to ch, without
// blocking. Every terminal branch of roundTrip's select must consult it first.
//
// select picks uniformly at random among ready cases, and dispatchResponse
// hands the response to a buffered channel without yielding, so the reader can
// close done (or the deadline can elapse) before the waiter is scheduled. Both
// cases are then ready and the waiter takes the terminal branch half the time,
// discarding a fully parsed, valid response. Reproduced at roughly 1 in 10000
// dials against a server that answers OPTIONS and ends the session in the same
// segment, which is what any server closing after a response produces.
func drain(ch <-chan *Response) (*Response, bool) {
	select {
	case resp := <-ch:
		return resp, true
	default:
		return nil, false
	}
}
