// Package exit is the side of the tunnel that serves the streams the entry
// opens: named forwards go to fixed local targets taken from this host's own
// config, and connect requests go through the SOCKS exit only if it is
// enabled. It works the same whether it dialed the carrier or accepted it.
package exit

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/config"
	"hstun/internal/mux"
	"hstun/internal/netx"
	"hstun/internal/proto"
)

type Exit struct {
	se      config.SocksExit
	maxStr  int
	targets map[string]string
	allow   map[int]bool
	idle    time.Duration
	dialer  net.Dialer
	lookup  func(ctx context.Context, host string) ([]net.IP, error)
}

// New builds the exit. maxStreams caps concurrent streams per session (0 = no
// cap): a compromised or buggy entry cannot make this host open unbounded
// outbound connections.
func New(forwards []config.Forward, se config.SocksExit, idle time.Duration, maxStreams int) *Exit {
	x := &Exit{
		se: se, idle: idle, maxStr: maxStreams,
		targets: map[string]string{},
		allow:   map[int]bool{},
		dialer:  net.Dialer{Timeout: 8 * time.Second, KeepAlive: netx.KeepAlive},
	}
	for _, f := range forwards {
		x.targets[f.Name] = f.Target
	}
	for _, p := range se.AllowPorts {
		x.allow[p] = true
	}
	x.lookup = func(ctx context.Context, h string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
		if err != nil {
			return nil, err
		}
		out := make([]net.IP, len(addrs))
		for i, a := range addrs {
			out[i] = a.IP
		}
		return out, nil
	}
	return x
}

// Handle serves one stream the entry opened and closes it when done.
func (x *Exit) Handle(ctx context.Context, st net.Conn) {
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr, err := proto.ReadOpen(st)
	if err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	var target net.Conn
	var code byte
	switch hdr.Kind {
	case proto.KindForward:
		addr, ok := x.targets[hdr.Payload]
		if !ok {
			code = proto.ReplyNotAllowed
			break
		}
		target, code = x.dial(ctx, addr)
	case proto.KindConnect:
		if !x.se.Enabled {
			code = proto.ReplyNotAllowed
			break
		}
		target, code = x.dialExit(ctx, hdr.Payload)
	default:
		code = proto.ReplyFailure
	}
	if target == nil {
		_ = proto.WriteReply(st, code)
		_ = st.Close()
		return
	}
	netx.Tune(target)
	if err := proto.WriteReply(st, proto.ReplyOK); err != nil {
		_ = target.Close()
		_ = st.Close()
		return
	}
	netx.Pipe(st, target, x.idle)
}

func (x *Exit) dial(ctx context.Context, addr string) (net.Conn, byte) {
	conn, err := x.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, replyFor(err)
	}
	return conn, proto.ReplyOK
}

func replyFor(err error) byte {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return proto.ReplyRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return proto.ReplyNetUnreach
	case errors.Is(err, syscall.EHOSTUNREACH), netx.IsTimeout(err):
		return proto.ReplyHostUnreach
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return proto.ReplyHostUnreach
	}
	return proto.ReplyFailure
}

// dialExit resolves the host itself, refuses private destinations, and dials
// the resolved IP directly so a second DNS answer cannot redirect it.
func (x *Exit) dialExit(ctx context.Context, hostport string) (net.Conn, byte) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, proto.ReplyFailure
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, proto.ReplyFailure
	}
	if len(x.allow) > 0 && !x.allow[port] {
		return nil, proto.ReplyNotAllowed
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if ips, err = x.lookup(rctx, host); err != nil || len(ips) == 0 {
			return nil, proto.ReplyHostUnreach
		}
	}
	var own []net.IP
	if x.se.DenyPrivate {
		own = ownAddrs()
	}
	// The entry gives up after 25 s and the user usually sooner: bound the total
	// time spent trying every address of a dead destination.
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var last byte = proto.ReplyNotAllowed
	for _, ip := range ips {
		if x.se.DenyPrivate && forbidden(ip, own) {
			continue
		}
		conn, code := x.dial(ctx, net.JoinHostPort(ip.String(), portStr))
		if conn != nil {
			return conn, proto.ReplyOK
		}
		last = code
	}
	return nil, last
}

func ownAddrs() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out = append(out, ipn.IP)
		}
	}
	return out
}

// forbidden reports whether ip is loopback, private, link-local, multicast,
// unspecified, a carrier-grade NAT address, or one of this host's own
// addresses.
func forbidden(ip net.IP, own []net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xC0 == 64 { // 100.64.0.0/10 (CGNAT, Tailscale)
			return true
		}
		if v4[0] == 0 || v4[0] >= 240 {
			return true
		}
	}
	// IPv6 forms that embed an IPv4 address must be judged by that address
	// (NAT64 64:ff9b::/96, 6to4 2002::/16), or private IPv4 hides behind them.
	if ip.To4() == nil && len(ip) == net.IPv6len {
		if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && allZero(ip[4:12]) {
			if forbidden(net.IP(ip[12:16]), own) {
				return true
			}
		}
		if ip[0] == 0x20 && ip[1] == 0x02 && forbidden(net.IP(ip[2:6]), own) {
			return true
		}
	}
	for _, o := range own {
		if o.Equal(ip) {
			return true
		}
	}
	return false
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// Serve accepts streams on the session until it ends.
func (x *Exit) Serve(ctx context.Context, ms *yamux.Session) error {
	var sem chan struct{}
	if x.maxStr > 0 {
		sem = make(chan struct{}, x.maxStr)
	}
	for {
		st, err := ms.AcceptStream()
		if err != nil {
			return err
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			default: // over the cap: refuse instead of queueing
				_ = st.Close()
				continue
			}
		}
		go func() {
			if sem != nil {
				defer func() { <-sem }()
			}
			x.Handle(ctx, mux.Stream{Stream: st})
		}()
	}
}
