// Package socks5 is a minimal CONNECT-only, no-auth SOCKS5 server side. It
// listens on loopback by default; it only parses the request and writes the
// reply, the tunnel does the connecting.
package socks5

import (
	"errors"
	"io"
	"net"
	"strconv"
)

const (
	repOK              = 0
	repCmdNotSupported = 7
	repAtypNotSupp     = 8
)

var ErrUnsupported = errors.New("socks5: unsupported request")

// ReadRequest does the greeting and reads one CONNECT request, returning
// "host:port". The caller sets a deadline on c first.
func ReadRequest(c io.ReadWriter) (string, error) {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return "", err
	}
	if h[0] != 5 {
		return "", ErrUnsupported
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{5, 0xFF})
		return "", ErrUnsupported
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	var r [4]byte
	if _, err := io.ReadFull(c, r[:]); err != nil {
		return "", err
	}
	if r[0] != 5 {
		return "", ErrUnsupported
	}
	if r[1] != 1 { // CONNECT only
		_ = WriteReply(c, repCmdNotSupported)
		return "", ErrUnsupported
	}
	var host string
	switch r[3] {
	case 1:
		var b [4]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return "", err
		}
		host = net.IP(b[:]).String()
	case 4:
		var b [16]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return "", err
		}
		host = net.IP(b[:]).String()
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return "", err
		}
		if l[0] == 0 {
			_ = WriteReply(c, repAtypNotSupp)
			return "", ErrUnsupported
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		_ = WriteReply(c, repAtypNotSupp)
		return "", ErrUnsupported
	}
	var p [2]byte
	if _, err := io.ReadFull(c, p[:]); err != nil {
		return "", err
	}
	port := int(p[0])<<8 | int(p[1])
	if port == 0 {
		return "", ErrUnsupported
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// WriteReply sends a reply with a zero bind address.
func WriteReply(c io.Writer, rep byte) error {
	_, err := c.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

// OK is the success reply code.
const OK = repOK
