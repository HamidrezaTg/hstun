// Package seal is the end-to-end encryption layer between the handshake and
// the multiplexer. The carrier (TLS, possibly terminated by a CDN) only gets
// to see ciphertext, and any change, replay, reorder or reflection of frames
// is detected, so the tunnel does not depend on who terminates TLS.
//
// Frame: u16 length (of ciphertext incl. tag) | AES-256-GCM(ciphertext).
// A length with the top bit set (padFlag) marks a padding frame: its plaintext
// is random bytes that the reader decrypts, checks and throws away. A sender
// only emits them after the peer said it understands them (see SetPeerPadding),
// and they are written in the same carrier message as a real frame, so on the
// wire they just make records vary in size.
// Each direction has its own key (HKDF from the handshake session key) and a
// strictly increasing 64-bit counter used as the nonce, so a nonce is never
// reused and a frame can only be accepted at its exact position. The length
// prefix is the AEAD additional data.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
)

const (
	// MaxPlain is the largest plaintext per frame.
	MaxPlain = 16 * 1024
	tagLen   = 16
	hdrLen   = 2
	padFlag  = 0x8000
	// MaxPad is the most padding one frame carries.
	MaxPad = 1400
)

// Padding modes (config `padding`).
const (
	PadOff    = "off"
	PadLight  = "light"  // small frames get 0-400 random bytes
	PadStrong = "strong" // the first frames look like 900-1400 byte records, small frames get more
)

var (
	ErrFrame = errors.New("seal: bad frame length")
	ErrAuth  = errors.New("seal: frame failed authentication")
)

func newAEAD(sessionKey []byte, label string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, sessionKey, nil, label, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

type conn struct {
	net.Conn
	rd, wr cipher.AEAD

	rmu  sync.Mutex
	rctr uint64
	rbuf []byte // ciphertext scratch
	left []byte // decrypted bytes not yet returned
	rerr error  // sticky: after any read failure the stream is unusable

	wmu  sync.Mutex
	wctr uint64
	wbuf []byte
	werr error

	padMode string      // our policy
	peerPad atomic.Bool // the peer reads padding frames
	nframes int         // frames written so far (strong mode shapes the first ones)
}

// Conn is what Wrap returns; the padding controls are on it.
type Conn interface {
	net.Conn
	// SetPadding sets the padding policy (PadOff, PadLight, PadStrong).
	SetPadding(mode string)
	// SetPeerPadding records that the peer understands padding frames.
	SetPeerPadding(ok bool)
	// WritePad sends one padding frame of n random bytes (1..MaxPad) if the peer supports it.
	WritePad(n int) error
}

// Wrap seals c with keys derived from sessionKey. dialer says which end this
// is; the two ends must pass opposite values.
func Wrap(c net.Conn, sessionKey []byte, dialer bool) (Conn, error) {
	d2l, err := newAEAD(sessionKey, "hstun2-dial-to-listen")
	if err != nil {
		return nil, err
	}
	l2d, err := newAEAD(sessionKey, "hstun2-listen-to-dial")
	if err != nil {
		return nil, err
	}
	s := &conn{Conn: c}
	if dialer {
		s.wr, s.rd = d2l, l2d
	} else {
		s.wr, s.rd = l2d, d2l
	}
	s.rbuf = make([]byte, MaxPlain+tagLen)
	s.wbuf = make([]byte, hdrLen+MaxPlain+tagLen+hdrLen+MaxPad+tagLen)
	s.padMode = PadOff
	return s, nil
}

func nonce(ctr uint64) []byte {
	var n [12]byte
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n[:]
}

func (s *conn) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.left) == 0 {
		if s.rerr != nil {
			return 0, s.rerr
		}
		var hdr [hdrLen]byte
		if _, err := io.ReadFull(s.Conn, hdr[:]); err != nil {
			s.rerr = err
			return 0, err
		}
		raw := binary.BigEndian.Uint16(hdr[:])
		n := int(raw &^ padFlag)
		if n <= tagLen || n > MaxPlain+tagLen {
			s.rerr = ErrFrame
			_ = s.Conn.Close()
			return 0, ErrFrame
		}
		ct := s.rbuf[:n]
		if _, err := io.ReadFull(s.Conn, ct); err != nil {
			s.rerr = err
			return 0, err
		}
		pt, err := s.rd.Open(ct[:0], nonce(s.rctr), ct, hdr[:])
		if err != nil {
			s.rerr = ErrAuth
			_ = s.Conn.Close()
			return 0, ErrAuth
		}
		s.rctr++
		if raw&padFlag != 0 {
			continue // padding: authenticated, then dropped
		}
		s.left = pt
	}
	n := copy(p, s.left)
	s.left = s.left[n:]
	return n, nil
}

func (s *conn) SetPadding(mode string) {
	s.wmu.Lock()
	s.padMode = mode
	s.wmu.Unlock()
}

func (s *conn) SetPeerPadding(ok bool) { s.peerPad.Store(ok) }

// padFor says how many padding bytes to add behind a real frame of n bytes.
func (s *conn) padFor(n int) int {
	if !s.peerPad.Load() {
		return 0
	}
	switch s.padMode {
	case PadLight:
		if n < 1024 {
			return rand.IntN(401)
		}
	case PadStrong:
		if s.nframes < 16 && n < 900 {
			return min(MaxPad, 900+rand.IntN(501)-n) // the record is 900-1400 bytes
		}
		if n < 4096 {
			return rand.IntN(601)
		}
	}
	return 0
}

// sealPad appends a padding frame of n random bytes to s.wbuf at off and
// returns the new end. The caller holds wmu.
func (s *conn) sealPad(off, n int) int {
	var pt [MaxPad + 8]byte
	for i := 0; i < n; i += 8 { // a filler that hides lengths; the AEAD keeps it authentic
		binary.LittleEndian.PutUint64(pt[i:], rand.Uint64())
	}
	binary.BigEndian.PutUint16(s.wbuf[off:off+hdrLen], padFlag|uint16(n+tagLen))
	out := s.wr.Seal(s.wbuf[off+hdrLen:off+hdrLen], nonce(s.wctr), pt[:n], s.wbuf[off:off+hdrLen])
	s.wctr++
	return off + hdrLen + len(out)
}

func (s *conn) WritePad(n int) error {
	if !s.peerPad.Load() || n < 1 {
		return nil
	}
	if n > MaxPad {
		n = MaxPad
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.werr != nil {
		return s.werr
	}
	end := s.sealPad(0, n)
	if _, err := s.Conn.Write(s.wbuf[:end]); err != nil {
		s.werr = err
		return err
	}
	return nil
}

func (s *conn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		if s.werr != nil {
			return total, s.werr
		}
		n := len(p)
		if n > MaxPlain {
			n = MaxPlain
		}
		binary.BigEndian.PutUint16(s.wbuf[:hdrLen], uint16(n+tagLen))
		out := s.wr.Seal(s.wbuf[hdrLen:hdrLen], nonce(s.wctr), p[:n], s.wbuf[:hdrLen])
		s.wctr++
		end := hdrLen + len(out)
		if pad := s.padFor(n); pad > 0 {
			end = s.sealPad(end, pad) // same carrier message as the real frame
		}
		s.nframes++
		// One Write per frame, so it is one carrier message. After a failed
		// write the stream position is unknown: never write again.
		if _, err := s.Conn.Write(s.wbuf[:end]); err != nil {
			s.werr = err
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
