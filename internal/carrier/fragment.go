package carrier

import (
	"encoding/binary"
	"math/rand/v2"
	"net"
	"time"
)

// Some SNI filters read only the first TCP segment of a connection, or only the
// first TLS record, and never reassemble. Fragmenting the ClientHello (the first
// thing a client sends) defeats those. Both kinds are legal TLS and TCP, and every
// server reassembles them.
const (
	FragmentOff    = "off"
	FragmentTCP    = "tcp"    // the ClientHello goes out as several small TCP segments, a few ms apart
	FragmentRecord = "record" // the ClientHello is re-framed as several TLS records
	FragmentBoth   = "both"   // records, then segments
)

// ValidFragment reports whether v is a known tls_fragment value ("" means off).
func ValidFragment(v string) bool {
	switch v {
	case "", FragmentOff, FragmentTCP, FragmentRecord, FragmentBoth:
		return true
	}
	return false
}

// fragmentConn rewrites the first handshake record written through it.
type fragmentConn struct {
	net.Conn
	mode string
	done bool
}

func newFragmentConn(c net.Conn, mode string) net.Conn {
	if mode == "" || mode == FragmentOff {
		return c
	}
	return &fragmentConn{Conn: c, mode: mode}
}

func (f *fragmentConn) Write(p []byte) (int, error) {
	if f.done || len(p) < 6 || p[0] != 0x16 { // 0x16: TLS handshake record
		return f.Conn.Write(p)
	}
	f.done = true
	out := p
	if f.mode == FragmentRecord || f.mode == FragmentBoth {
		out = splitRecord(p)
	}
	if f.mode == FragmentTCP || f.mode == FragmentBoth {
		if err := writeSegments(f.Conn, out); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if _, err := f.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// splitRecord re-frames one TLS record as two to four records that carry the same
// payload (a handshake message may be spread over records). The first piece is small.
func splitRecord(rec []byte) []byte {
	n := int(binary.BigEndian.Uint16(rec[3:5]))
	if len(rec) < 5+n || n < 8 {
		return rec
	}
	payload := rec[5 : 5+n]
	parts := 2 + rand.IntN(3)
	cuts := []int{0}
	for i := 1; i < parts; i++ {
		prev := cuts[len(cuts)-1]
		room := n - prev - (parts - i) // keep at least one byte per remaining piece
		if room < 1 {
			break
		}
		step := 1 + rand.IntN(min(room, 24+rand.IntN(120)))
		cuts = append(cuts, prev+step)
	}
	cuts = append(cuts, n)
	out := make([]byte, 0, len(rec)+5*len(cuts))
	for i := 0; i+1 < len(cuts); i++ {
		piece := payload[cuts[i]:cuts[i+1]]
		out = append(out, rec[0], rec[1], rec[2], byte(len(piece)>>8), byte(len(piece)))
		out = append(out, piece...)
	}
	return append(out, rec[5+n:]...)
}

// writeSegments sends b as several TCP writes of 20-120 bytes (at most eight, the
// last one takes the rest) with 1-6 ms between them.
func writeSegments(c net.Conn, b []byte) error {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	for i := 0; len(b) > 0; i++ {
		n := 20 + rand.IntN(101)
		if n >= len(b) || i >= 7 {
			n = len(b)
		}
		if _, err := c.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(time.Millisecond + time.Duration(rand.IntN(6))*time.Millisecond)
		}
	}
	return nil
}
