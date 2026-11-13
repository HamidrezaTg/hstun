package httpmode

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"hstun/internal/auth"
	"hstun/internal/decoy"
	"hstun/internal/proto"
)

// Hooks connect the carrier to the server's rate limiter and session handling.
type Hooks struct {
	// OnSession owns the authenticated conn until the session ends.
	OnSession func(c net.Conn, res *proto.ServerResult, ip string)
	// IP returns the real client IP of a request.
	IP func(r *http.Request) string
	// Allow and Fail implement the per-IP failed-handshake limit.
	Allow func(ip string) bool
	Fail  func(ip string)
}

type Server struct {
	kr  *auth.Keyring
	h   Hooks
	log *slog.Logger

	mu       sync.Mutex
	pending  map[string]*pendingHS
	sessions map[string]*srvSession
}

type pendingHS struct {
	st  *proto.ServerState
	ip  string
	exp time.Time
}

func NewServer(kr *auth.Keyring, h Hooks, log *slog.Logger) *Server {
	s := &Server{kr: kr, h: h, log: log, pending: map[string]*pendingHS{}, sessions: map[string]*srvSession{}}
	go s.reaper()
	return s
}

// Handler serves the carrier endpoints; mount it with the base path stripped.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc(pathHello, s.hello)
	m.HandleFunc(pathAuth, s.authenticate)
	m.HandleFunc(pathUp, s.up)
	m.HandleFunc(pathDown, s.down)
	m.HandleFunc(pathClose, s.close)
	m.HandleFunc("/", decoy.NotFound)
	return m
}

// readBody reads a request body with a hard cap, refusing oversize requests
// by Content-Length before reading anything.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	if r.Method != http.MethodPost {
		decoy.NotFound(w, r)
		return nil, false
	}
	if r.ContentLength < 0 || r.ContentLength > max {
		http.Error(w, "size", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(15 * time.Second))
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return nil, false
	}
	return b, true
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	ip := s.h.IP(r)
	if !s.h.Allow(ip) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	body, ok := readBody(w, r, proto.MaxHandshakeMsg)
	if !ok {
		return
	}
	st, chal, err := proto.ServerBegin(body, s.kr)
	if err != nil {
		s.h.Fail(ip)
		s.log.Warn("handshake failed", "ip", ip, "carrier", "http", "reason", err.Error())
		decoy.NotFound(w, r)
		return
	}
	cid := make([]byte, 16)
	_, _ = rand.Read(cid)
	key := hex.EncodeToString(cid)
	s.mu.Lock()
	now := time.Now()
	mine := 0
	for k, p := range s.pending {
		if now.After(p.exp) {
			delete(s.pending, k)
		} else if p.ip == ip {
			mine++
		}
	}
	// A hello only needs a known key id (one byte, sent in clear), so without a
	// per-source cap one client could hold every pending slot and lock out the
	// real ones until the entries expire.
	if mine >= maxPendingPerIP {
		s.mu.Unlock()
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	s.pending[key] = &pendingHS{st: st, ip: ip, exp: now.Add(pendingTTL)}
	s.mu.Unlock()
	w.Header().Set(hdrCid, key)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(chal)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, proto.MaxHandshakeMsg)
	if !ok {
		return
	}
	cid := r.Header.Get(hdrCid)
	s.mu.Lock()
	p := s.pending[cid]
	delete(s.pending, cid)
	s.mu.Unlock()
	if p == nil || time.Now().After(p.exp) {
		http.Error(w, "expired", http.StatusGone)
		return
	}
	res, acc, err := p.st.Finish(body)
	if err != nil {
		s.h.Fail(p.ip)
		s.log.Warn("handshake failed", "ip", p.ip, "carrier", "http", "reason", err.Error())
		decoy.NotFound(w, r)
		return
	}
	res.Carrier = proto.CarrierHTTP
	sid := make([]byte, 16)
	_, _ = rand.Read(sid)
	sess := newSrvSession(sid, res.SessionKey, p.ip, func(id string) {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
	})
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	go s.h.OnSession(sess.conn, res, p.ip)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(append(acc, sid...))
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*srvSession, []byte) {
	// A wrong method or a missing/malformed session id is what a scanner sends,
	// not our client, so it gets the same page as any unknown path.
	sidHex := r.Header.Get(hdrSid)
	sid, err := hex.DecodeString(sidHex)
	if r.Method != http.MethodPost || err != nil || len(sid) != 16 {
		decoy.NotFound(w, r)
		return nil, nil
	}
	s.mu.Lock()
	sess := s.sessions[sidHex]
	s.mu.Unlock()
	if sess == nil {
		http.Error(w, "gone", http.StatusGone)
		return nil, nil
	}
	return sess, sid
}

func u64(r *http.Request, name string) (uint64, bool) {
	v, err := strconv.ParseUint(r.Header.Get(name), 10, 64)
	return v, err == nil
}

func (s *Server) up(w http.ResponseWriter, r *http.Request) {
	sess, sid := s.lookup(w, r)
	if sess == nil {
		return
	}
	seq, ok := u64(r, hdrSeq)
	if !ok || seq == 0 {
		http.Error(w, "seq", http.StatusBadRequest)
		return
	}
	body, ok := readBody(w, r, upChunk)
	if !ok {
		return
	}
	tag, _ := hex.DecodeString(r.Header.Get(hdrTag))
	if !auth.Equal(tag, reqTag(sess.key, pathUp, sid, seq, 0, body)) {
		http.Error(w, "tag", http.StatusForbidden)
		return
	}
	sess.touch()
	ack, accepted := sess.acceptUp(seq, body)
	if !accepted {
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	w.Header().Set(hdrAck, strconv.FormatUint(ack, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) down(w http.ResponseWriter, r *http.Request) {
	sess, sid := s.lookup(w, r)
	if sess == nil {
		return
	}
	if r.ContentLength != 0 {
		http.Error(w, "size", http.StatusRequestEntityTooLarge)
		return
	}
	ack, ok := u64(r, hdrAck)
	if !ok {
		http.Error(w, "ack", http.StatusBadRequest)
		return
	}
	tag, _ := hex.DecodeString(r.Header.Get(hdrTag))
	if !auth.Equal(tag, reqTag(sess.key, pathDown, sid, 0, ack, nil)) {
		http.Error(w, "tag", http.StatusForbidden)
		return
	}
	sess.touch()
	sess.ackDown(ack)
	// Chunk-mode clients ask for smaller responses (a path that cuts connections
	// after a few tens of KB); anything else gets the full size.
	max, holdFor, retx := downChunk, hold, rto
	if v, ok := u64(r, hdrMax); ok && v >= minDownChunk && v < downChunk {
		max, holdFor, retx = int(v), chunkHoldIdle, chunkRTO
		sess.conn.setTxLimit(chunkTxLimit)
		if sess.busy() {
			holdFor = chunkHoldBusy
		}
	}
	deadline := time.NewTimer(holdFor)
	defer deadline.Stop()
	for {
		// Take the wake channel before looking for data, so a Write that lands
		// in between is not missed.
		wake := sess.waitDown()
		frames, closed := sess.collectDown(max, retx)
		if len(frames) > 0 {
			var buf bytes.Buffer
			for _, f := range frames {
				var h [12]byte
				binary.BigEndian.PutUint64(h[:8], f.seq)
				binary.BigEndian.PutUint32(h[8:], uint32(len(f.data)))
				buf.Write(h[:])
				buf.Write(f.data)
			}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			_, _ = w.Write(buf.Bytes())
			return
		}
		if closed {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		select {
		case <-wake:
		case <-time.After(retx): // re-check retransmit eligibility
		case <-deadline.C:
			sess.touch()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	sess, sid := s.lookup(w, r)
	if sess == nil {
		return
	}
	if _, ok := readBody(w, r, 64); !ok {
		return
	}
	tag, _ := hex.DecodeString(r.Header.Get(hdrTag))
	if !auth.Equal(tag, reqTag(sess.key, pathClose, sid, 0, 0, nil)) {
		http.Error(w, "tag", http.StatusForbidden)
		return
	}
	sess.conn.Close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reaper() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		var dead []*srvSession
		s.mu.Lock()
		now := time.Now()
		for k, p := range s.pending {
			if now.After(p.exp) {
				delete(s.pending, k)
			}
		}
		for _, sess := range s.sessions {
			if sess.idleFor() > sessIdle {
				dead = append(dead, sess)
			}
		}
		s.mu.Unlock()
		for _, sess := range dead {
			s.log.Info("http session idle, closing", "ip", sess.ip)
			sess.conn.Close()
		}
	}
}

// srvSession is the server end of one HTTP-carrier session.
type srvSession struct {
	id   string
	sid  []byte
	key  []byte
	ip   string
	conn *sconn

	mu       sync.Mutex
	upNext   uint64
	upBuf    map[uint64][]byte
	downSeq  uint64
	unacked  []*dchunk
	lastSeen time.Time
	lastData time.Time // last time a data chunk moved in either direction
}

type dchunk struct {
	seq    uint64
	data   []byte
	sentAt time.Time
}

func newSrvSession(sid, key []byte, ip string, onGone func(string)) *srvSession {
	id := hex.EncodeToString(sid)
	s := &srvSession{id: id, sid: sid, key: key, ip: ip, upNext: 1, upBuf: map[uint64][]byte{}, lastSeen: time.Now()}
	s.conn = newSconn("hstun-http-server", ip, func() { onGone(id) })
	return s
}

// busy reports whether payload moved recently (chunk mode shortens its hold then).
func (s *srvSession) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastData) < chunkBusyFor
}

func (s *srvSession) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *srvSession) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSeen)
}

// acceptUp stores an upstream chunk and delivers everything now contiguous.
// accepted=false means "too busy, retry the same chunk later".
func (s *srvSession) acceptUp(seq uint64, data []byte) (ack uint64, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.upNext {
		return s.upNext - 1, true // duplicate
	}
	if _, dup := s.upBuf[seq]; dup {
		return s.upNext - 1, true
	}
	if seq >= s.upNext+window {
		return 0, false
	}
	if seq == s.upNext && s.conn.rxBuffered() >= rxLimit {
		return 0, false // reader is behind; push back
	}
	s.upBuf[seq] = data
	s.lastData = time.Now()
	for {
		d, ok := s.upBuf[s.upNext]
		if !ok {
			break
		}
		delete(s.upBuf, s.upNext)
		s.upNext++
		s.conn.pushRx(d)
	}
	return s.upNext - 1, true
}

// ackDown frees downstream chunks the client has confirmed.
func (s *srvSession) ackDown(ack uint64) {
	s.mu.Lock()
	freed := 0
	i := 0
	for i < len(s.unacked) && s.unacked[i].seq <= ack {
		freed += len(s.unacked[i].data)
		i++
	}
	s.unacked = s.unacked[i:]
	s.mu.Unlock()
	if freed > 0 {
		s.conn.confirm(freed)
	}
}

// collectDown returns chunks to send on this poll: unacked chunks older than
// the retransmit timeout first, then new data. closed reports a dead session
// with nothing left to send.
func (s *srvSession) collectDown(max int, retx time.Duration) (out []*dchunk, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	budget := max
	for _, c := range s.unacked {
		if budget <= 0 {
			break
		}
		if c.sentAt.IsZero() || now.Sub(c.sentAt) >= retx {
			c.sentAt = now
			out = append(out, c)
			budget -= len(c.data)
		}
	}
	for budget > 0 {
		d := s.conn.take(budget)
		if d == nil {
			break
		}
		s.downSeq++
		c := &dchunk{seq: s.downSeq, data: d, sentAt: now}
		s.unacked = append(s.unacked, c)
		out = append(out, c)
		budget -= len(d)
	}
	if len(out) == 0 && s.conn.isClosed() {
		return nil, true
	}
	if len(out) > 0 {
		s.lastData = now
	}
	return out, false
}

func (s *srvSession) waitDown() <-chan struct{} { return s.conn.bc.C() }
