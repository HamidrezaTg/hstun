// Package entry is the side of the tunnel that owns the user-facing listeners
// (named forwards and SOCKS5) and opens a multiplexed stream per connection.
// It works the same whether it dialed the carrier or accepted it.
package entry

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/mux"
	"hstun/internal/proto"
)

var (
	ErrNoSession = errors.New("no tunnel session available")
	ErrKeyCap    = errors.New("too many sessions for this key")
)

// Session is one authenticated carrier session registered with the hub.
type Session struct {
	id      uint64
	Name    string
	KeyID   byte
	Carrier byte
	IP      string
	Since   time.Time
	Mux     *yamux.Session
}

// Hub tracks live tunnel sessions and opens streams on the least loaded one.
type Hub struct {
	mu           sync.Mutex
	next         uint64
	sessions     map[uint64]*Session
	perKey       map[byte]int
	maxPerKey    int
	maxStreams   int
	openTimeout  time.Duration
	replyTimeout time.Duration
}

// NewHub creates a hub; maxPerKey 0 means no per-key session cap.
func NewHub(maxPerKey, maxStreams int) *Hub {
	return &Hub{
		sessions:     map[uint64]*Session{},
		perKey:       map[byte]int{},
		maxPerKey:    maxPerKey,
		maxStreams:   maxStreams,
		openTimeout:  10 * time.Second,
		replyTimeout: 10 * time.Second,
	}
}

// Add registers a session, enforcing the per-key cap.
func (h *Hub) Add(s *Session) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxPerKey > 0 && h.perKey[s.KeyID] >= h.maxPerKey {
		return ErrKeyCap
	}
	h.next++
	s.id = h.next
	h.sessions[s.id] = s
	h.perKey[s.KeyID]++
	return nil
}

// Remove unregisters a session.
func (h *Hub) Remove(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[s.id]; ok {
		delete(h.sessions, s.id)
		h.perKey[s.KeyID]--
	}
}

// Count is the number of live sessions.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// SessionInfo is the /stats view of a session.
type SessionInfo struct {
	ID      uint64    `json:"id"`
	Name    string    `json:"name"`
	Carrier string    `json:"carrier"`
	IP      string    `json:"ip"`
	Since   time.Time `json:"since"`
	Streams int       `json:"streams"`
}

// Snapshot lists live sessions.
func (h *Hub) Snapshot() []SessionInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SessionInfo, 0, len(h.sessions))
	for _, s := range h.sessions {
		c := "ws"
		if s.Carrier == proto.CarrierHTTP {
			c = "http"
		}
		out = append(out, SessionInfo{s.id, s.Name, c, s.IP, s.Since, s.Mux.NumStreams()})
	}
	return out
}

// ActiveStreams is the number of streams open on all live sessions.
func (h *Hub) ActiveStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.sessions {
		n += s.Mux.NumStreams()
	}
	return n
}

// CloseAll closes every live session (the streams on them end as resets).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	all := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		all = append(all, s)
	}
	h.mu.Unlock()
	for _, s := range all {
		_ = s.Mux.Close()
	}
}

// candidates returns live sessions under the stream cap, least loaded first.
func (h *Hub) candidates() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Session
	for _, s := range h.sessions {
		if s.Mux.IsClosed() || s.Mux.NumStreams() >= h.maxStreams {
			continue
		}
		out = append(out, s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Mux.NumStreams() < out[j-1].Mux.NumStreams(); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// OpenStream opens a stream on the least loaded session, sends the open
// header and waits for the client's one-byte reply. The returned code is the
// client's reply (0 = connected); err is non-nil if no stream could be set up.
func (h *Hub) OpenStream(ctx context.Context, hdr proto.OpenHeader) (net.Conn, byte, error) {
	cands := h.candidates()
	if len(cands) == 0 {
		return nil, proto.ReplyFailure, ErrNoSession
	}
	var lastErr error
	for _, s := range cands {
		if err := ctx.Err(); err != nil {
			return nil, proto.ReplyFailure, err
		}
		st, err := s.Mux.OpenStream()
		if err != nil {
			lastErr = err
			continue
		}
		_ = st.SetDeadline(time.Now().Add(h.replyTimeout))
		if err := proto.WriteOpen(st, hdr); err != nil {
			_ = st.Close()
			lastErr = err
			continue
		}
		code, err := proto.ReadReply(st)
		if err != nil {
			_ = st.Close()
			lastErr = err
			continue
		}
		_ = st.SetDeadline(time.Time{})
		if code != proto.ReplyOK {
			_ = st.Close()
			return nil, code, nil
		}
		return mux.Stream{Stream: st}, proto.ReplyOK, nil
	}
	return nil, proto.ReplyFailure, lastErr
}
