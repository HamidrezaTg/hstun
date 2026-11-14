package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/config"
	"hstun/internal/mux"
	"hstun/internal/proto"
)

// ProbeOptions says what a probe run measures.
type ProbeOptions struct {
	Carrier     string        // ws, http or chunk
	Fingerprint string        // "" keeps the configured one
	Fragment    string        // "" keeps the configured one
	Sizes       []int         // echo sizes, escalating; nil = the default ladder
	Idle        time.Duration // quiet time before one more echo (0 = skip)
	Soak        time.Duration // one small echo per second for this long (0 = skip)
	BulkDown    int           // bytes downloaded for the speed figure (0 = skip)
	BulkUp      int           // bytes uploaded for the speed figure (0 = skip)
}

// DefaultSizes is the escalation ladder: the per-connection cuts seen on real
// paths sit between 20 and 50 KB, so the ladder crosses them.
var DefaultSizes = []int{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20}

// Step is one escalation step.
type Step struct {
	Bytes int     `json:"bytes"`
	OK    bool    `json:"ok"`
	Ms    float64 `json:"ms"`
	Err   string  `json:"err,omitempty"`
}

// ProbeResult is what `hstun probe` reports.
type ProbeResult struct {
	Carrier     string  `json:"carrier"`
	Fingerprint string  `json:"fingerprint"`
	Fragment    string  `json:"fragment"`
	ConnectMs   float64 `json:"connect_ms"`
	Connected   bool    `json:"connected"`
	Error       string  `json:"error,omitempty"`
	// ExitProbe is false when the exit is older than 0.5.0 (or has probes off): only the
	// handshake and a forward stream could be checked.
	ExitProbe bool    `json:"exit_probe"`
	ForwardOK bool    `json:"forward_ok"`
	Steps     []Step  `json:"steps,omitempty"`
	IdleOK    *bool   `json:"idle_ok,omitempty"`
	SoakSent  int     `json:"soak_sent,omitempty"`
	SoakLost  int     `json:"soak_lost,omitempty"`
	SoakP50Ms float64 `json:"soak_p50_ms,omitempty"`
	DownMbit  float64 `json:"down_mbit,omitempty"`
	UpMbit    float64 `json:"up_mbit,omitempty"`
	Verdict   string  `json:"verdict"` // ok, unstable or blocked
	Seconds   float64 `json:"seconds"`
}

// Probe dials one carrier session with the given options and measures it against
// the exit. It needs a host that is the entry of a direct-mode pair (the dialing
// side that opens streams); anything else only gets the handshake check.
func Probe(ctx context.Context, cfg *config.ClientFile, log *slog.Logger, po ProbeOptions) (res ProbeResult) {
	start := time.Now()
	c2 := *cfg
	c2.Client.Carrier = po.Carrier
	c2.Client.AutoOrder = nil
	if po.Fingerprint != "" {
		c2.Client.TLSFingerprint = po.Fingerprint
	}
	if po.Fragment != "" {
		c2.Client.TLSFragment = po.Fragment
	}
	res = ProbeResult{Carrier: po.Carrier, Fingerprint: c2.Client.TLSFingerprint, Fragment: c2.Client.TLSFragment}
	if res.Fragment == "" {
		res.Fragment = "off"
	}
	defer func() { res.Seconds = time.Since(start).Seconds(); res.Verdict = verdict(res) }()
	c, err := New(&c2, log)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	t0 := time.Now()
	conn, err := c.dialCarrier(ctx, po.Carrier)
	if err != nil {
		res.Error = "connect: " + err.Error()
		return res
	}
	defer conn.Close()
	var mopts []mux.Option
	if carrierID(po.Carrier) == proto.CarrierHTTP {
		mopts = append(mopts, mux.SlowCarrier())
	}
	ms, err := mux.Dialer(conn, c.ping, mopts...)
	if err != nil {
		res.Error = "mux: " + err.Error()
		return res
	}
	defer ms.Close()
	res.ConnectMs = float64(time.Since(t0).Microseconds()) / 1000
	res.Connected = true
	go mux.Refuse(ms, func(string) { conn.SetPeerPadding(true) })
	mux.Announce(ms)

	// Does the exit speak probe streams? (0.5.0 and later, probes not disabled.)
	echo, err := openProbe(ms, "echo")
	if err != nil {
		// An older exit: at least see that a forward stream opens.
		if name := firstForward(&c2); name != "" {
			res.ForwardOK = openForward(ms, name)
		}
		return res
	}
	res.ExitProbe = true
	defer echo.Close()
	sizes := po.Sizes
	if sizes == nil {
		sizes = DefaultSizes
	}
	for _, n := range sizes {
		st := echoStep(echo, n)
		res.Steps = append(res.Steps, st)
		if !st.OK {
			return res // the stream is no longer in step after a failure
		}
		if ctx.Err() != nil {
			return res
		}
	}
	if po.Idle > 0 {
		select {
		case <-time.After(po.Idle):
		case <-ctx.Done():
			return res
		}
		ok := echoStep(echo, 1<<10).OK
		res.IdleOK = &ok
		if !ok {
			return res
		}
	}
	if po.Soak > 0 {
		res.SoakSent, res.SoakLost, res.SoakP50Ms = soak(ctx, echo, po.Soak)
	}
	if po.BulkDown > 0 {
		if mbit, err := bulkDown(ms, po.BulkDown); err == nil {
			res.DownMbit = mbit
		}
	}
	if po.BulkUp > 0 {
		if mbit, err := bulkUp(ms, po.BulkUp); err == nil {
			res.UpMbit = mbit
		}
	}
	return res
}

// verdict: blocked when no session came up, unstable when anything that should have
// worked failed or was lost, ok otherwise.
func verdict(r ProbeResult) string {
	switch {
	case !r.Connected:
		return "blocked"
	case !r.ExitProbe:
		if r.ForwardOK {
			return "ok"
		}
		return "unstable"
	}
	for _, s := range r.Steps {
		if !s.OK {
			return "unstable"
		}
	}
	if r.IdleOK != nil && !*r.IdleOK {
		return "unstable"
	}
	if r.SoakLost > 0 {
		return "unstable"
	}
	return "ok"
}

func firstForward(cfg *config.ClientFile) string {
	if len(cfg.Forward) > 0 {
		return cfg.Forward[0].Name
	}
	return ""
}

func openForward(ms *yamux.Session, name string) bool {
	st, err := ms.OpenStream()
	if err != nil {
		return false
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(15 * time.Second))
	if proto.WriteOpen(st, proto.OpenHeader{Kind: proto.KindForward, Payload: name}) != nil {
		return false
	}
	code, err := proto.ReadReply(st)
	return err == nil && code == proto.ReplyOK
}

func openProbe(ms *yamux.Session, payload string) (*yamux.Stream, error) {
	st, err := ms.OpenStream()
	if err != nil {
		return nil, err
	}
	_ = st.SetDeadline(time.Now().Add(20 * time.Second))
	if err := proto.WriteOpen(st, proto.OpenHeader{Kind: proto.KindProbe, Payload: payload}); err != nil {
		st.Close()
		return nil, err
	}
	code, err := proto.ReadReply(st)
	if err != nil || code != proto.ReplyOK {
		st.Close()
		if err == nil {
			err = fmt.Errorf("exit refused the probe stream (code %d)", code)
		}
		return nil, err
	}
	_ = st.SetDeadline(time.Time{})
	return st, nil
}

// echoStep sends n random bytes and expects them back unchanged, within a deadline
// that grows with n (a path that stalls must not hang the run).
func echoStep(st *yamux.Stream, n int) Step {
	want := make([]byte, n)
	_, _ = rand.Read(want)
	t0 := time.Now()
	_ = st.SetDeadline(time.Now().Add(20*time.Second + time.Duration(n)*20*time.Microsecond))
	errc := make(chan error, 1)
	go func() { _, err := st.Write(want); errc <- err }()
	got := make([]byte, n)
	if _, err := io.ReadFull(st, got); err != nil {
		return Step{Bytes: n, Err: err.Error(), Ms: float64(time.Since(t0).Milliseconds())}
	}
	if err := <-errc; err != nil {
		return Step{Bytes: n, Err: err.Error()}
	}
	if !bytes.Equal(got, want) {
		return Step{Bytes: n, Err: "echo differs"}
	}
	return Step{Bytes: n, OK: true, Ms: float64(time.Since(t0).Microseconds()) / 1000}
}

// soak sends a 64-byte echo every second and reports how many were sent, how many
// did not come back in 5 s, and the median round trip.
func soak(ctx context.Context, st *yamux.Stream, d time.Duration) (sent, lost int, p50 float64) {
	var rtts []float64
	end := time.Now().Add(d)
	buf := make([]byte, 64)
	for time.Now().Before(end) && ctx.Err() == nil {
		sent++
		_, _ = rand.Read(buf)
		t0 := time.Now()
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.Write(buf); err != nil {
			lost += int(time.Until(end)/time.Second) + 1
			break
		}
		got := make([]byte, len(buf))
		if _, err := io.ReadFull(st, got); err != nil || !bytes.Equal(got, buf) {
			lost++
			break // the stream is out of step after a miss
		}
		rtts = append(rtts, float64(time.Since(t0).Microseconds())/1000)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
	if len(rtts) > 0 {
		sort.Float64s(rtts)
		p50 = rtts[len(rtts)/2]
	}
	return
}

func bulkDown(ms *yamux.Session, n int) (float64, error) {
	st, err := openProbe(ms, "source:"+strconv.Itoa(n))
	if err != nil {
		return 0, err
	}
	defer st.Close()
	t0 := time.Now()
	_ = st.SetDeadline(time.Now().Add(2 * time.Minute))
	got, err := io.Copy(io.Discard, io.LimitReader(st, int64(n)))
	if err != nil || got != int64(n) {
		return 0, fmt.Errorf("got %d of %d bytes: %v", got, n, err)
	}
	return float64(n) * 8 / 1e6 / time.Since(t0).Seconds(), nil
}

func bulkUp(ms *yamux.Session, n int) (float64, error) {
	st, err := openProbe(ms, "sink")
	if err != nil {
		return 0, err
	}
	defer st.Close()
	t0 := time.Now()
	_ = st.SetDeadline(time.Now().Add(2 * time.Minute))
	chunk := make([]byte, 32<<10)
	for sent := 0; sent < n; {
		k := min(len(chunk), n-sent)
		if _, err := st.Write(chunk[:k]); err != nil {
			return 0, err
		}
		sent += k
	}
	if err := st.Close(); err != nil { // FIN: the server reads to the end and answers with the count
		return 0, err
	}
	var b [8]byte
	if _, err := io.ReadFull(st, b[:]); err != nil || int(binary.BigEndian.Uint64(b[:])) != n {
		return 0, fmt.Errorf("the exit counted %d of %d bytes: %v", binary.BigEndian.Uint64(b[:]), n, err)
	}
	return float64(n) * 8 / 1e6 / time.Since(t0).Seconds(), nil
}
