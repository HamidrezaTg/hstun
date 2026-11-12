package server

import (
	"net"
	"sync"
)

// guardListener enforces, before any bytes are read, an optional source
// allow-list and a cap on concurrent connections per peer IP. Peers inside the
// trusted-proxy ranges (Cloudflare) are exempt from the per-IP cap, since many
// users share those addresses; the allow-list always applies.
type guardListener struct {
	net.Listener
	allow  []*net.IPNet
	perIP  int
	exempt ranges

	mu sync.Mutex
	n  map[string]int
}

func newGuardListener(l net.Listener, allowFrom []string, perIP int, exempt ranges) (*guardListener, error) {
	g := &guardListener{Listener: l, perIP: perIP, exempt: exempt, n: map[string]int{}}
	for _, c := range allowFrom {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		g.allow = append(g.allow, n)
	}
	return g, nil
}

func peerIP(c net.Conn) net.IP {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	return nil
}

func (g *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := peerIP(c)
		if ip == nil {
			_ = c.Close()
			continue
		}
		if len(g.allow) > 0 {
			ok := false
			for _, n := range g.allow {
				if n.Contains(ip) {
					ok = true
					break
				}
			}
			if !ok {
				_ = c.Close()
				continue
			}
		}
		if g.perIP <= 0 || (g.exempt != nil && g.exempt.contains(ip)) {
			return c, nil
		}
		key := limiterKey(ip.String())
		g.mu.Lock()
		if g.n[key] >= g.perIP {
			g.mu.Unlock()
			_ = c.Close()
			continue
		}
		g.n[key]++
		g.mu.Unlock()
		return &guardConn{Conn: c, release: func() {
			g.mu.Lock()
			if g.n[key]--; g.n[key] <= 0 {
				delete(g.n, key)
			}
			g.mu.Unlock()
		}}, nil
	}
}

type guardConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *guardConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
