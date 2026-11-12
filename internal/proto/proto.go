// Package proto is the hstun wire format: the authenticated handshake that
// runs inside a carrier connection, and the small header at the start of each
// multiplexed stream. Every length is capped before anything is allocated.
package proto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"hstun/internal/auth"
)

const (
	Magic = "HST1"
	// Version 2 adds the sealed (AEAD) stream after the handshake.
	Version = 2

	// Handshake messages.
	MsgHello     = 1
	MsgChallenge = 2
	MsgProof     = 3
	MsgAccept    = 4
	MsgReject    = 5

	// MaxHandshakeMsg caps every pre-authentication message.
	MaxHandshakeMsg = 256
	NonceLen        = 32
	NameLen         = 16

	// Carrier ids carried in the hello (informational and logged).
	CarrierWS   = 1
	CarrierHTTP = 2

	// Stream open kinds.
	KindForward = 1
	KindConnect = 2

	// MaxOpenPayload caps the stream open header payload.
	MaxOpenPayload = 270

	// Reply byte values: 0 is OK; others follow SOCKS5 REP codes.
	ReplyOK          = 0
	ReplyFailure     = 1
	ReplyNotAllowed  = 2
	ReplyNetUnreach  = 3
	ReplyHostUnreach = 4
	ReplyRefused     = 5
	ReplyTTLExpired  = 6
)

var (
	ErrBadMagic   = errors.New("bad magic")
	ErrBadVersion = errors.New("unsupported version")
	ErrUnknownKey = errors.New("unknown key id")
	ErrBadProof   = errors.New("bad proof")
	ErrTooLong    = errors.New("message too long")
	ErrRejected   = errors.New("rejected by server")
)

// WriteMsg writes: u8 type | u16 len | body.
func WriteMsg(w io.Writer, typ byte, body []byte) error {
	if len(body) > MaxHandshakeMsg {
		return ErrTooLong
	}
	buf := make([]byte, 3+len(body))
	buf[0] = typ
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(body)))
	copy(buf[3:], body)
	_, err := w.Write(buf) // one Write so it becomes one carrier message
	return err
}

// ReadMsg reads one message, enforcing the cap before allocating.
func ReadMsg(r io.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:3]))
	if n > MaxHandshakeMsg {
		return 0, nil, ErrTooLong
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

// Hello is the first client message.
type Hello struct {
	Version byte
	Carrier byte
	KeyID   byte
	Name    [NameLen]byte
	NonceC  [NonceLen]byte
}

func (h *Hello) marshal() []byte {
	b := make([]byte, 0, 4+3+NameLen+NonceLen)
	b = append(b, Magic...)
	b = append(b, h.Version, h.Carrier, h.KeyID)
	b = append(b, h.Name[:]...)
	b = append(b, h.NonceC[:]...)
	return b
}

func parseHello(b []byte) (*Hello, error) {
	if len(b) != 4+3+NameLen+NonceLen {
		return nil, ErrTooLong
	}
	if string(b[:4]) != Magic {
		return nil, ErrBadMagic
	}
	h := &Hello{Version: b[4], Carrier: b[5], KeyID: b[6]}
	copy(h.Name[:], b[7:7+NameLen])
	copy(h.NonceC[:], b[7+NameLen:])
	if h.Version != Version {
		return h, ErrBadVersion
	}
	return h, nil
}

// PackName pads or truncates a client name to the fixed field.
func PackName(s string) [NameLen]byte {
	var n [NameLen]byte
	copy(n[:], s)
	return n
}

func NameString(n [NameLen]byte) string {
	return string(bytes.TrimRight(n[:], "\x00"))
}

// ClientResult is what the client learns from a successful handshake.
type ClientResult struct {
	SessionKey []byte
}

// ClientState is the client side of a handshake in progress. The steps are
// separate so the HTTP carrier can run them as separate requests.
type ClientState struct {
	key   []byte
	h     *Hello
	hb    []byte
	ns    []byte
	proof []byte
}

// ClientBegin builds the hello body.
//
//	C -> S Hello(nonce_c)
//	S -> C Challenge(nonce_s)
//	C -> S Proof  = HMAC(key, "hstun1-c" | hello | nonce_s)
//	S -> C Accept = HMAC(key, "hstun1-s" | hello | nonce_s | proof)
func ClientBegin(key []byte, carrier byte, name string) (*ClientState, []byte, error) {
	h := &Hello{Version: Version, Carrier: carrier, KeyID: auth.KeyID(key), Name: PackName(name)}
	if _, err := rand.Read(h.NonceC[:]); err != nil {
		return nil, nil, err
	}
	hb := h.marshal()
	return &ClientState{key: key, h: h, hb: hb}, hb, nil
}

// Challenge consumes the server nonce and returns the proof body.
func (s *ClientState) Challenge(ns []byte) ([]byte, error) {
	if len(ns) != NonceLen {
		return nil, errors.New("bad challenge length")
	}
	s.ns = append([]byte(nil), ns...)
	s.proof = auth.Mac(s.key, "hstun1-c", s.hb, s.ns)
	return s.proof, nil
}

// Accept verifies the server's accept and derives the session key.
func (s *ClientState) Accept(acc []byte) (*ClientResult, error) {
	want := auth.Mac(s.key, "hstun1-s", s.hb, s.ns, s.proof)
	if !auth.Equal(acc, want) {
		return nil, errors.New("server failed to prove it knows the key")
	}
	sk, err := auth.SessionKey(s.key, s.h.NonceC[:], s.ns)
	if err != nil {
		return nil, err
	}
	return &ClientResult{SessionKey: sk}, nil
}

// ClientHandshake runs all client steps over a connected carrier conn. The
// caller sets a deadline on c beforehand.
func ClientHandshake(c net.Conn, key []byte, carrier byte, name string) (*ClientResult, error) {
	st, hb, err := ClientBegin(key, carrier, name)
	if err != nil {
		return nil, err
	}
	if err := WriteMsg(c, MsgHello, hb); err != nil {
		return nil, err
	}
	typ, ns, err := ReadMsg(c)
	if err != nil {
		return nil, err
	}
	if typ == MsgReject {
		return nil, fmt.Errorf("%w: %s", ErrRejected, string(ns))
	}
	if typ != MsgChallenge {
		return nil, errors.New("unexpected challenge message")
	}
	proof, err := st.Challenge(ns)
	if err != nil {
		return nil, err
	}
	if err := WriteMsg(c, MsgProof, proof); err != nil {
		return nil, err
	}
	typ, acc, err := ReadMsg(c)
	if err != nil {
		return nil, err
	}
	if typ == MsgReject {
		return nil, fmt.Errorf("%w: %s", ErrRejected, string(acc))
	}
	if typ != MsgAccept {
		return nil, errors.New("unexpected accept message")
	}
	return st.Accept(acc)
}

// ServerResult describes an authenticated client.
type ServerResult struct {
	KeyID      byte
	Name       string
	Carrier    byte
	SessionKey []byte
}

// ServerState is the server side of a handshake in progress.
type ServerState struct {
	key []byte
	h   *Hello
	hb  []byte
	ns  []byte
}

// ServerBegin validates the hello and returns the challenge body. Errors
// carry short reasons ("bad_hello", "unknown_key_id") for logs.
func ServerBegin(hb []byte, kr *auth.Keyring) (*ServerState, []byte, error) {
	h, err := parseHello(hb)
	if err != nil {
		return nil, nil, fmt.Errorf("bad_hello: %w", err)
	}
	key, ok := kr.Get(h.KeyID)
	if !ok {
		return nil, nil, fmt.Errorf("unknown_key_id %02x: %w", h.KeyID, ErrUnknownKey)
	}
	ns := make([]byte, NonceLen)
	if _, err := rand.Read(ns); err != nil {
		return nil, nil, err
	}
	return &ServerState{key: key, h: h, hb: append([]byte(nil), hb...), ns: ns}, ns, nil
}

// Finish verifies the proof and returns the accept body.
func (s *ServerState) Finish(proof []byte) (*ServerResult, []byte, error) {
	want := auth.Mac(s.key, "hstun1-c", s.hb, s.ns)
	if !auth.Equal(proof, want) {
		return nil, nil, fmt.Errorf("bad_proof: %w", ErrBadProof)
	}
	acc := auth.Mac(s.key, "hstun1-s", s.hb, s.ns, proof)
	sk, err := auth.SessionKey(s.key, s.h.NonceC[:], s.ns)
	if err != nil {
		return nil, nil, err
	}
	return &ServerResult{KeyID: s.h.KeyID, Name: NameString(s.h.Name), Carrier: s.h.Carrier, SessionKey: sk}, acc, nil
}

// ServerHandshake runs all server steps over a connected carrier conn. The
// caller handles the random delay and closing on failure.
func ServerHandshake(c net.Conn, kr *auth.Keyring) (*ServerResult, error) {
	typ, hb, err := ReadMsg(c)
	if err != nil {
		return nil, fmt.Errorf("read_hello: %w", err)
	}
	if typ != MsgHello {
		return nil, errors.New("not_hello")
	}
	st, chal, err := ServerBegin(hb, kr)
	if err != nil {
		return nil, err
	}
	if err := WriteMsg(c, MsgChallenge, chal); err != nil {
		return nil, fmt.Errorf("write_challenge: %w", err)
	}
	typ, proof, err := ReadMsg(c)
	if err != nil {
		return nil, fmt.Errorf("read_proof: %w", err)
	}
	if typ != MsgProof {
		return nil, errors.New("not_proof")
	}
	res, acc, err := st.Finish(proof)
	if err != nil {
		return nil, err
	}
	if err := WriteMsg(c, MsgAccept, acc); err != nil {
		return nil, fmt.Errorf("write_accept: %w", err)
	}
	return res, nil
}

// RejectDelay is a short random pause before closing a failed handshake.
func RejectDelay() time.Duration {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return 500*time.Millisecond + time.Duration(b[0])*4*time.Millisecond
}

// OpenHeader starts every multiplexed stream: u8 ver | u8 kind | u16 len | payload.
type OpenHeader struct {
	Kind    byte
	Payload string
}

func WriteOpen(w io.Writer, h OpenHeader) error {
	if len(h.Payload) == 0 || len(h.Payload) > MaxOpenPayload {
		return ErrTooLong
	}
	buf := make([]byte, 4+len(h.Payload))
	buf[0] = Version
	buf[1] = h.Kind
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(h.Payload)))
	copy(buf[4:], h.Payload)
	_, err := w.Write(buf)
	return err
}

func ReadOpen(r io.Reader) (OpenHeader, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return OpenHeader{}, err
	}
	if hdr[0] != Version {
		return OpenHeader{}, ErrBadVersion
	}
	n := int(binary.BigEndian.Uint16(hdr[2:4]))
	if n == 0 || n > MaxOpenPayload {
		return OpenHeader{}, ErrTooLong
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return OpenHeader{}, err
	}
	return OpenHeader{Kind: hdr[1], Payload: string(p)}, nil
}

// WriteReply sends the one-byte answer to a stream open.
func WriteReply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{code})
	return err
}

func ReadReply(r io.Reader) (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}
