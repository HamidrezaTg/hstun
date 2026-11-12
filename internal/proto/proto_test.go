package proto

import (
	"bytes"
	"net"
	"testing"
	"time"

	"hstun/internal/auth"
)

func keypair(t *testing.T) ([]byte, *auth.Keyring) {
	t.Helper()
	k, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	kr, err := auth.NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, kr
}

func TestHandshakeOK(t *testing.T) {
	key, kr := keypair(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	type res struct {
		r   *ServerResult
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := ServerHandshake(b, kr)
		ch <- res{r, err}
	}()
	cr, err := ClientHandshake(a, key, CarrierWS, "exit-a")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	sr := <-ch
	if sr.err != nil {
		t.Fatalf("server: %v", sr.err)
	}
	if sr.r.Name != "exit-a" || sr.r.Carrier != CarrierWS {
		t.Fatalf("bad result %+v", sr.r)
	}
	if !bytes.Equal(cr.SessionKey, sr.r.SessionKey) {
		t.Fatal("session keys differ")
	}
}

func TestHandshakeWrongKey(t *testing.T) {
	_, kr := keypair(t)
	bad, _ := auth.Generate()
	// Make the wrong key look known by putting both in a ring under the same id is
	// impossible; instead test an unknown key id is rejected cleanly.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = a.SetDeadline(time.Now().Add(2 * time.Second))
	_ = b.SetDeadline(time.Now().Add(2 * time.Second))
	errc := make(chan error, 1)
	go func() { _, err := ServerHandshake(b, kr); errc <- err }()
	go func() { _, _ = ClientHandshake(a, bad, CarrierWS, "x") }()
	if err := <-errc; err == nil {
		t.Fatal("expected failure for an unknown key")
	}
}

func TestHandshakeBadProof(t *testing.T) {
	key, kr := keypair(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = a.SetDeadline(time.Now().Add(2 * time.Second))
	_ = b.SetDeadline(time.Now().Add(2 * time.Second))
	errc := make(chan error, 1)
	go func() { _, err := ServerHandshake(b, kr); errc <- err }()
	// Hand-rolled client sending a garbage proof under a valid key id.
	h := &Hello{Version: Version, Carrier: CarrierWS, KeyID: auth.KeyID(key)}
	copy(h.NonceC[:], bytes.Repeat([]byte{7}, NonceLen))
	_ = WriteMsg(a, MsgHello, h.marshal())
	if _, _, err := ReadMsg(a); err != nil {
		t.Fatal(err)
	}
	_ = WriteMsg(a, MsgProof, bytes.Repeat([]byte{9}, 32))
	if err := <-errc; err == nil {
		t.Fatal("garbage proof accepted")
	}
}

func TestReadMsgRejectsOversize(t *testing.T) {
	// length field 0xFFFF must be rejected before any allocation of that size.
	r := bytes.NewReader([]byte{MsgHello, 0xFF, 0xFF})
	if _, _, err := ReadMsg(r); err != ErrTooLong {
		t.Fatalf("want ErrTooLong, got %v", err)
	}
}

func TestOpenHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteOpen(&buf, OpenHeader{Kind: KindConnect, Payload: "example.com:443"}); err != nil {
		t.Fatal(err)
	}
	h, err := ReadOpen(&buf)
	if err != nil || h.Kind != KindConnect || h.Payload != "example.com:443" {
		t.Fatalf("got %+v %v", h, err)
	}
}

func TestOpenHeaderLimits(t *testing.T) {
	if err := WriteOpen(&bytes.Buffer{}, OpenHeader{Kind: KindForward, Payload: string(make([]byte, MaxOpenPayload+1))}); err == nil {
		t.Fatal("oversize payload accepted")
	}
	// zero length and oversize length on the read side
	for _, raw := range [][]byte{{Version, 1, 0, 0}, {Version, 1, 0xFF, 0xFF}, {9, 1, 0, 1, 'x'}} {
		if _, err := ReadOpen(bytes.NewReader(raw)); err == nil {
			t.Fatalf("accepted %v", raw)
		}
	}
}

func FuzzReadMsg(f *testing.F) {
	f.Add([]byte{MsgHello, 0, 3, 1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = ReadMsg(bytes.NewReader(b))
	})
}

func FuzzReadOpen(f *testing.F) {
	f.Add([]byte{Version, 1, 0, 2, 'x', '1'})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ReadOpen(bytes.NewReader(b))
	})
}

func FuzzParseHello(f *testing.F) {
	f.Add([]byte("HST1\x01\x01\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseHello(b)
	})
}

// A recorded proof must not work on a new connection: the server picks a fresh
// nonce each time.
func TestHandshakeReplayRejected(t *testing.T) {
	key, kr := keypair(t)
	// Session 1: record the client's hello and proof as an eavesdropper would.
	st1, hello, _ := ClientBegin(key, CarrierWS, "x")
	s1, chal1, err := ServerBegin(hello, kr)
	if err != nil {
		t.Fatal(err)
	}
	proof1, _ := st1.Challenge(chal1)
	if _, _, err := s1.Finish(proof1); err != nil {
		t.Fatalf("honest handshake failed: %v", err)
	}
	// Session 2: the attacker replays the same hello and proof.
	s2, chal2, err := ServerBegin(hello, kr)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(chal1, chal2) {
		t.Fatal("server reused a nonce")
	}
	if _, _, err := s2.Finish(proof1); err == nil {
		t.Fatal("replayed proof accepted")
	}
}

// An attacker without the key cannot pass as the server by reflecting the
// client's own proof back as the accept.
func TestHandshakeReflectionRejected(t *testing.T) {
	key, _ := keypair(t)
	st, hello, _ := ClientBegin(key, CarrierWS, "x")
	ns := bytes.Repeat([]byte{3}, NonceLen)
	proof, _ := st.Challenge(ns)
	if _, err := st.Accept(proof); err == nil {
		t.Fatal("client accepted its own proof as the server's answer")
	}
	// ...nor a server answer computed with the wrong label or inputs.
	for _, bad := range [][]byte{
		auth.Mac(key, "hstun1-c", st.hb, ns, proof),
		auth.Mac(key, "hstun1-s", hello, ns),
		make([]byte, 32),
		nil,
	} {
		if _, err := st.Accept(bad); err == nil {
			t.Fatalf("bogus accept %x accepted", bad)
		}
	}
}

// A hello that tries to use a different version or key id is refused before a
// challenge is issued.
func TestServerBeginRefusesBadHello(t *testing.T) {
	key, kr := keypair(t)
	_, hello, _ := ClientBegin(key, CarrierWS, "x")
	for name, mutate := range map[string]func([]byte){
		"magic":   func(b []byte) { b[0] ^= 1 },
		"version": func(b []byte) { b[4] = 99 },
		"key id":  func(b []byte) { b[6] ^= 0xff },
	} {
		h := append([]byte(nil), hello...)
		mutate(h)
		if _, _, err := ServerBegin(h, kr); err == nil {
			t.Errorf("%s: hello accepted", name)
		}
	}
}
