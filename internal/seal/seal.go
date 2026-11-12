// Package seal is the end-to-end encryption layer between the handshake and
// the multiplexer. The carrier (TLS, possibly terminated by a CDN) only gets
// to see ciphertext, and any change, replay, reorder or reflection of frames
// is detected, so the tunnel does not depend on who terminates TLS.
//
// Frame: u16 length (of ciphertext incl. tag) | AES-256-GCM(ciphertext).
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
	"net"
	"sync"
)

const (
	// MaxPlain is the largest plaintext per frame.
	MaxPlain = 16 * 1024
	tagLen   = 16
	hdrLen   = 2
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
}

// Wrap seals c with keys derived from sessionKey. dialer says which end this
// is; the two ends must pass opposite values.
func Wrap(c net.Conn, sessionKey []byte, dialer bool) (net.Conn, error) {
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
	s.wbuf = make([]byte, hdrLen+MaxPlain+tagLen)
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
		n := int(binary.BigEndian.Uint16(hdr[:]))
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
		s.left = pt
	}
	n := copy(p, s.left)
	s.left = s.left[n:]
	return n, nil
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
		// One Write per frame, so it is one carrier message. After a failed
		// write the stream position is unknown: never write again.
		if _, err := s.Conn.Write(s.wbuf[:hdrLen+len(out)]); err != nil {
			s.werr = err
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
