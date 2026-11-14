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
	"sync"
	"sync/atomic"
	"time"

	"hstun/internal/carrier"
	"hstun/internal/netx"
	"hstun/internal/proto"
)

const (
	upWorkers   = 3
	downPollers = 2
	failAfter   = 60 * time.Second
	// chunkFailAfter: in chunk mode a request that cannot pass for this long is stuck on a
	// chunk the path will not carry; the session ends and the next one asks for less.
	chunkFailAfter = 12 * time.Second
	// chunkStuckFailures: this many failed polls with no data delivered in between (and no
	// quiet half minute) make a chunk-mode session give up.
	chunkStuckFailures = 5

	// Chunk mode defaults. Measured from Iran to a Cloudflare customer name:
	// about 20 KB upstream and 48 KB downstream per connection get through, more
	// is cut. These keep a safe margin below both.
	DefaultChunkUp   = 12 << 10
	DefaultChunkDown = 32 << 10
	DefaultChunkPar  = 8

	// A healthy chunk request takes 0.2-0.6 s (measured through Cloudflare), while a
	// connection the path swallows never answers. A swallowed chunk holds up every
	// chunk behind it (delivery is in order), so these are kept short.
	chunkAttempt     = 3 * time.Second  // one request must finish within this in chunk mode
	chunkBodyStall   = 4 * time.Second  // a response body must complete this soon after its headers
	chunkConnect     = 3 * time.Second  // dial and TLS handshake
	chunkPollTimeout = 15 * time.Second // a long-poll the server never answers frees its poller after this (server holds are shorter)
	chunkHedge       = time.Second      // an upstream chunk not answered by now is also sent on a second connection
)

type clientConn struct {
	*sconn
	hc     *http.Client
	base   string
	names  metaNames
	cookie bool // metadata travels in cookies (the server answered the hello in that style)
	sid    []byte
	sidHex string
	sk     []byte
	ctx    context.Context
	cancel context.CancelFunc

	upSeq uint64

	chunk   bool // short-connection mode
	upSize  int  // largest upstream body
	downMax int  // downstream payload budget asked of the server (chunk mode)
	tuning  *carrier.ChunkTuning
	// failAfter is how long a request may keep failing before the session gives up.
	failAfter time.Duration
	born      time.Time
	failed    atomic.Bool
	attempt   time.Duration // timeout of one upstream request

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
	nUp, nDown, upSize, downMax, attempt := upWorkers, downPollers, upChunk, 0, 30*time.Second
	fail := failAfter
	tlsTimeout := 10 * time.Second
	if o.Chunk {
		defer func() { println("DEBUG sizes", upSize, downMax) }()
		par := orDefault(o.ChunkPar, DefaultChunkPar)
		nUp, nDown = par, par
		upSize = min(orDefault(o.ChunkUp, DefaultChunkUp), upChunk)
		downMax = min(orDefault(o.ChunkDown, DefaultChunkDown), downChunk)
		if o.Tuning != nil {
			tu, td := o.Tuning.Sizes()
			upSize, downMax = min(upSize, tu), min(downMax, td)
		}
		attempt = chunkAttempt
		fail = chunkFailAfter
		d.Timeout, tlsTimeout = chunkConnect, chunkConnect
	}
	tr := &http.Transport{
		// The TLS handshake is ours so the ClientHello can look like a browser's.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if o.Connect != "" {
				addr = o.Connect
			}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			netx.Tune(c)
			hctx, cancel := context.WithTimeout(ctx, tlsTimeout)
			defer cancel()
			tc, err := o.Handshake(hctx, c)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			return tc, nil
		},
		DisableKeepAlives:   o.Chunk, // one connection per request
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true,
		MaxIdleConnsPerHost: nUp + nDown + 2,
		MaxConnsPerHost:     nUp + nDown + 2,
		IdleConnTimeout:     90 * time.Second,
	}
	hc := &http.Client{Transport: carrier.WithBrowserHeaders(tr, o)}
	base := "https://" + o.Server + o.HTTPPath

	st, hello, err := proto.ClientBegin(key, proto.CarrierHTTP, name)
	if err != nil {
		return nil, nil, err
	}
	names := namesFor(o.HTTPPath)
	helloHdr := http.Header{"Cookie": {names.helloCookie()}}
	if o.NoCookies {
		helloHdr = nil
	}
	cid, chal, err := post(ctx, hc, base+pathHello, hello, helloHdr)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, nil, fmt.Errorf("hello: %w", err)
	}
	cidVal, useCookie := names.responseID(cid)
	proof, err := st.Challenge(chal)
	if err != nil {
		return nil, nil, err
	}
	authHdr := http.Header{hdrCid: {cidVal}}
	if useCookie {
		authHdr = http.Header{"Cookie": {names.id + "=" + cidVal}}
	}
	_, resp, err := post(ctx, hc, base+pathAuth, proof, authHdr)
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
		hc: hc, base: base, names: names, cookie: useCookie, sid: sid, sidHex: hex.EncodeToString(sid), sk: res.SessionKey,
		ctx: cctx, cancel: cancel, recvNext: 1, recvBuf: map[uint64][]byte{}, lastData: time.Now(),
		chunk: o.Chunk, upSize: upSize, downMax: downMax, attempt: attempt, tuning: o.Tuning, born: time.Now(), failAfter: fail,
	}
	c.sconn = newSconn("hstun-http-client", o.Server, func() {
		cancel()
		if c.tuning != nil && !c.failed.Load() && time.Since(c.born) > growAfter {
			c.tuning.Grow() // it ran fine for a while: try bigger chunks next time
		}
		go c.sendClose()
		tr.CloseIdleConnections()
	})
	if o.Chunk {
		c.sconn.setTxLimit(chunkTxLimit)
	}
	for i := 0; i < nUp; i++ {
		go c.upWorker()
	}
	for i := 0; i < nDown; i++ {
		go c.poller(i)
	}
	return c, res.SessionKey, nil
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
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
	mx := 0
	if path == pathDown {
		mx = c.downMax
	}
	c.names.writeReq(req.Header, c.cookie, c.sidHex, seq, ack, mx, reqTag(c.sk, path, c.sid, seq, ack, body), seq != 0, path == pathDown, path)
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
		data, seq, closed, wake := c.nextChunk(c.upSize, &c.upSeq)
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
			c.stuck()
			return
		}
		c.confirm(len(data))
	}
}

// sendUp retries one chunk until the server accepts it. The server dedups by
// sequence number, so a resend after a lost response is harmless. It returns
// false when the session is dead.
func (c *clientConn) sendUp(seq uint64, data []byte) bool {
	if c.chunk {
		return c.sendUpHedged(seq, data)
	}
	backoff := 200 * time.Millisecond
	give := time.Now().Add(c.failAfter)
	for c.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(c.ctx, c.attempt)
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
		if backoff < 3*time.Second && !c.chunk {
			backoff *= 2
		}
	}
	return false
}

// sendUpHedged is sendUp for chunk mode. A request the path swallows never
// answers, and the chunks behind it cannot be delivered in order until it does,
// so after chunkHedge the same chunk (same sequence number, deduplicated by the
// server) goes out on a second connection; the first answer wins.
func (c *clientConn) sendUpHedged(seq uint64, data []byte) bool {
	const (
		ok = iota + 1
		busy
		fatal
		failed
	)
	results := make(chan int, 16)
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel() // abandons any attempt still in flight once one has answered
	attempt := func() {
		actx, acancel := context.WithTimeout(ctx, c.attempt)
		defer acancel()
		req, err := c.newReq(actx, pathUp, data, seq, 0)
		if err != nil {
			results <- failed
			return
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			results <- failed
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent:
			results <- ok
		case http.StatusTooManyRequests:
			results <- busy
		case http.StatusGone, http.StatusForbidden, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			results <- fatal
		default:
			results <- failed
		}
	}
	give := time.Now().Add(c.failAfter)
	outstanding := 1
	go attempt()
	hedge := time.NewTimer(chunkHedge)
	defer hedge.Stop()
	for {
		select {
		case r := <-results:
			outstanding--
			switch r {
			case ok:
				return true
			case fatal:
				return false
			}
			if time.Now().After(give) {
				return false // hedged attempts keep overlapping, so outstanding may never reach zero
			}
			if outstanding > 0 {
				continue
			}
			wait := jitter(100 * time.Millisecond)
			if r == busy {
				wait = jitter(150 * time.Millisecond)
			}
			if !sleepCtx(c.ctx, wait) {
				return false
			}
			outstanding++
			go attempt()
			hedge.Reset(chunkHedge)
		case <-hedge.C:
			if outstanding < 3 {
				outstanding++
				go attempt()
			}
			hedge.Reset(chunkHedge)
		case <-c.ctx.Done():
			return false
		}
	}
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
	nfail, lastFail := 0, time.Time{} // chunk mode: failures that no data delivery has cleared
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
			if status == http.StatusOK || !c.chunk {
				failing, nfail = time.Time{}, 0
			}
		case err == nil && (status == http.StatusGone || status == http.StatusForbidden || status == http.StatusBadRequest):
			c.Close()
			return
		default:
			if failing.IsZero() {
				failing = time.Now()
			}
			if c.chunk {
				// An empty answer (204) does not mean the path is fine: a downstream chunk that
				// is too big for the path is cut again and again, with empty answers in between.
				// Count the failures instead of timing an unbroken run of them.
				if time.Since(lastFail) > 30*time.Second {
					nfail = 0
				}
				nfail++
				lastFail = time.Now()
				if nfail >= chunkStuckFailures {
					c.stuck()
					return
				}
			}
			if time.Since(failing) > c.failAfter {
				c.stuck()
				return
			}
			if !sleepCtx(c.ctx, jitter(backoff)) {
				return
			}
			if backoff < 3*time.Second && !c.chunk {
				backoff *= 2
			}
		}
	}
}

func (c *clientConn) pollOnce() (int, error) {
	pollTimeout := hold + 20*time.Second
	if c.chunk {
		pollTimeout = chunkPollTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, pollTimeout)
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
	if c.chunk {
		// A connection that stalls mid-response (the path cut it) must not hold a
		// poller for the whole hold interval; the server resends unacked chunks.
		stall := time.AfterFunc(chunkBodyStall, cancel)
		defer stall.Stop()
	}
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

// growAfter is how long a session must have run before its chunk sizes count as safe.
const growAfter = 3 * time.Minute

// stuck closes a session that could not move a request for failAfter. In chunk
// mode that usually means a chunk is bigger than what the path lets through, and
// a chunk that is already numbered cannot be cut smaller, so the next session
// starts with smaller sizes instead.
func (c *clientConn) stuck() {
	println("DEBUG stuck; sizes", c.upSize, c.downMax, time.Since(c.born).String())
	c.failed.Store(true)
	if c.tuning != nil {
		c.tuning.Shrink()
	}
	c.Close()
}
