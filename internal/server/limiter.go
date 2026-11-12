package server

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// failLimiter counts failed handshakes per source IP in a one-minute window.
type failLimiter struct {
	mu    sync.Mutex
	limit int
	m     map[string]*failBucket
	sweep time.Time // last time expired buckets were removed
}

type failBucket struct {
	start time.Time
	n     int
}

func newFailLimiter(limit int) *failLimiter {
	return &failLimiter{limit: limit, m: map[string]*failBucket{}}
}

// limiterKey groups IPv6 addresses by /64: one host can rotate through a whole
// /64, so counting single addresses would never trip.
func limiterKey(ip string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip
}

func (f *failLimiter) allow(ip string) bool {
	if f.limit <= 0 {
		return true
	}
	ip = limiterKey(ip)
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.m[ip]
	if b == nil || time.Since(b.start) > time.Minute {
		return true
	}
	return b.n < f.limit
}

func (f *failLimiter) fail(ip string) {
	ip = limiterKey(ip)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) > 10000 { // bound memory: drop only expired buckets, never live ones
		// Sweep at most every few seconds so a flood of distinct IPs cannot
		// turn every failure into a full scan under the lock.
		if time.Since(f.sweep) > 5*time.Second {
			f.sweep = time.Now()
			for k, v := range f.m {
				if time.Since(v.start) > time.Minute {
					delete(f.m, k)
				}
			}
		}
		if len(f.m) > 10000 {
			if _, tracked := f.m[ip]; !tracked {
				return
			}
		}
	}
	b := f.m[ip]
	if b == nil || time.Since(b.start) > time.Minute {
		f.m[ip] = &failBucket{start: time.Now(), n: 1}
		return
	}
	b.n++
}

// ranges is a set of CIDRs loaded from a file (one per line, # comments).
type ranges []*net.IPNet

func loadRanges(path string) (ranges, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r ranges
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		_, n, err := net.ParseCIDR(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", path, line, err)
		}
		r = append(r, n)
	}
	if len(r) == 0 {
		return nil, fmt.Errorf("%s: no ranges", path)
	}
	return r, sc.Err()
}

func (r ranges) contains(ip net.IP) bool {
	for _, n := range r {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
