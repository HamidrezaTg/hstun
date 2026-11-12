package httpmode

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"hstun/internal/carrier"
	"hstun/internal/netx"
	"hstun/internal/proto"
)

const (
	upWorkers   = 3
	downPollers = 2
	failAfter   = 60 * time.Second
)

type clientConn struct {
	*sconn
	hc     *http.Client
	base   string
	sid    []byte
	sidHex string
	sk     []byte
	ctx    context.Context
	cancel context.CancelFunc

	upSeq uint64

	rmu      sync.Mutex
	recvNext uint64
	recvBuf  map[uint64][]byte
	lastData time.Time
}

// Dial runs the handshake over /o and /a and returns an authenticated
// byte-stream conn backed by upload workers and download pollers.
func Dial(ctx context.Context, o carrier.Options, key []byte, name string) (net.Conn, []byte, error) {
	var d net.Dialer
	d.Timeout = 10 * time.Second
	d.KeepAlive = netx.KeepAlive
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if o.Connect != "" {
				addr = o.Connect
			}
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				netx.Tune(c)
			}
			return c, err
		},
		TLSClientConfig:     o.TLS.Clone(),
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true,
		MaxIdleConnsPerHost: upWorkers + downPollers + 2,
		MaxConnsPerHost:     upWorkers + downPollers + 2,
		IdleConnTimeout:     90 * time.Second,
	}
	hc := &http.Client{Transport: carrier.WithUserAgent(tr, o.UserAgent)}
	base := "https://" + o.Server + o.HTTPPath

	st, hello, err := proto.ClientBegin(key, proto.CarrierHTTP, name)
	if err != nil {
		return nil, nil, err
	}
	cid, chal, err := post(ctx, hc, base+pathHello, hello, nil)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, nil, fmt.Errorf("hello: %w", err)
	}
	cidHdr := cid.Get(hdrCid)
	proof, err := st.Challenge(chal)
	if err != nil {
		return nil, nil, err
	}
	_, resp, err := post(ctx, hc, base+pathAuth, proof, http.Header{hdrCid: {cidHdr}})
	if err != nil {
		tr.CloseIdleConnections()
		return nil, nil, fmt.Errorf("auth: %w", err)
	}
	if len(resp) != 32+16 {
		return nil, nil, errors.New("bad accept response")
	}
	res, err := st.Accept(resp[:32])
	if err != nil {
		return nil, nil, err
	}
	sid := append([]byte(nil), resp[32:]...)

	cctx, cancel := context.WithCancel(context.Background())
	c := &clientConn{
		hc: hc, base: base, sid: sid, sidHex: hex.EncodeToString(sid), sk: res.SessionKey,
		ctx: cctx, cancel: cancel, recvNext: 1, recvBuf: map[uint64][]byte{}, lastData: time.Now(),
	}
	c.sconn = newSconn("hstun-http-client", o.Server, func() {
		cancel()
		go c.sendClose()
		tr.CloseIdleConnections()
	})
	for i := 0; i < upWorkers; i++ {
		go c.upWorker()
	}
	for i := 0; i < downPollers; i++ {
		go c.poller(i)
	}
	return c, res.SessionKey, nil
}

// post sends a small handshake request and returns the response headers and
// body (capped).
func post(ctx context.Context, hc *http.Client, url string, body []byte, hdr http.Header) (http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Header, b, nil
}

func (c *clientConn) newReq(ctx context.Context, path string, body []byte, seq, ack uint64) (*http.Request, error) {
	var rd io.Reader = http.NoBody
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrSid, c.sidHex)
	if seq != 0 {
		req.Header.Set(hdrSeq, strconv.FormatUint(seq, 10))
	}
	if path == pathDown {
		req.Header.Set(hdrAck, strconv.FormatUint(ack, 10))
	}
	req.Header.Set(hdrTag, hex.EncodeToString(reqTag(c.sk, path, c.sid, seq, ack, body)))
	req.Header.Set("Content-Type", "application/octet-stream")
	return req, nil
}

func (c *clientConn) sendClose() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := c.newReq(ctx, pathClose, nil, 0, 0)
	if err != nil {
		return
	}
	if resp, err := c.hc.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func jitter(d time.Duration) time.Duration { return d/2 + time.Duration(rand.Int64N(int64(d))) }

func (c *clientConn) upWorker() {
	for {
		data, seq, closed, wake := c.nextChunk(upChunk, &c.upSeq)
		if closed {
			return
		}
		if data == nil {
			select {
			case <-wake:
			case <-c.ctx.Done():
				return
			}
			continue
		}
		if !c.sendUp(seq, data) {
			c.Close()
			return
		}
		c.confirm(len(data))
	}
}

// sendUp retries one chunk until the server accepts it. The server dedups by
// sequence number, so a resend after a lost response is harmless. It returns
// false when the session is dead.
func (c *clientConn) sendUp(seq uint64, data []byte) bool {
	backoff := 200 * time.Millisecond
	give := time.Now().Add(failAfter)
	for c.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		req, err := c.newReq(ctx, pathUp, data, seq, 0)
		if err == nil {
			var resp *http.Response
			if resp, err = c.hc.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				cancel()
				switch resp.StatusCode {
				case http.StatusNoContent:
					return true
				case http.StatusTooManyRequests:
					if !sleepCtx(c.ctx, jitter(150*time.Millisecond)) {
						return false
					}
					continue
				case http.StatusGone, http.StatusForbidden, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
					return false
				}
			}
		}
		cancel()
		if time.Now().After(give) {
			return false
		}
		if !sleepCtx(c.ctx, jitter(backoff)) {
			return false
		}
		if backoff < 3*time.Second {
			backoff *= 2
		}
	}
	return false
}

func (c *clientConn) ackValue() uint64 {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return c.recvNext - 1
}

func (c *clientConn) active() bool {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return time.Since(c.lastData) < 5*time.Second
}

// poller keeps a long-poll outstanding. Poller 0 always runs; extra pollers
// run only while data is flowing, so an idle session costs one empty request
// per hold interval.
func (c *clientConn) poller(i int) {
	backoff := 200 * time.Millisecond
	failing := time.Time{}
	for c.ctx.Err() == nil {
		if i > 0 && !c.active() && !c.hasTx() {
			if !sleepCtx(c.ctx, 200*time.Millisecond) {
				return
			}
			continue
		}
		if c.rxBuffered() >= rxLimit {
			if !sleepCtx(c.ctx, 50*time.Millisecond) {
				return
			}
			continue
		}
		status, err := c.pollOnce()
		switch {
		case err == nil && (status == http.StatusOK || status == http.StatusNoContent):
			backoff = 200 * time.Millisecond
			failing = time.Time{}
		case err == nil && (status == http.StatusGone || status == http.StatusForbidden || status == http.StatusBadRequest):
			c.Close()
			return
		default:
			if failing.IsZero() {
				failing = time.Now()
			}
			if time.Since(failing) > failAfter {
				c.Close()
				return
			}
			if !sleepCtx(c.ctx, jitter(backoff)) {
				return
			}
			if backoff < 3*time.Second {
				backoff *= 2
			}
		}
	}
}

func (c *clientConn) pollOnce() (int, error) {
	ctx, cancel := context.WithTimeout(c.ctx, hold+20*time.Second)
	defer cancel()
	ack := c.ackValue()
	req, err := c.newReq(ctx, pathDown, nil, 0, ack)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*downChunk+4096))
	if err != nil {
		return 0, err
	}
	for len(body) >= 12 {
		seq := binary.BigEndian.Uint64(body[:8])
		n := int(binary.BigEndian.Uint32(body[8:12]))
		if n > downChunk || 12+n > len(body) {
			return 0, errors.New("malformed downstream frame")
		}
		c.deliver(seq, body[12:12+n])
		body = body[12+n:]
	}
	return resp.StatusCode, nil
}

// deliver reorders downstream chunks and pushes contiguous data to the reader.
func (c *clientConn) deliver(seq uint64, data []byte) {
	cp := append([]byte(nil), data...)
	c.rmu.Lock()
	if seq >= c.recvNext {
		c.recvBuf[seq] = cp
	}
	c.lastData = time.Now()
	var ready [][]byte
	for {
		d, ok := c.recvBuf[c.recvNext]
		if !ok {
			break
		}
		delete(c.recvBuf, c.recvNext)
		c.recvNext++
		ready = append(ready, d)
	}
	// Push while still holding rmu: two pollers delivering at once must not
	// interleave their (individually ordered) batches.
	for _, d := range ready {
		c.pushRx(d)
	}
	c.rmu.Unlock()
}
