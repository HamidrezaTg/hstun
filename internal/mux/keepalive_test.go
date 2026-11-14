package mux

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hstun/internal/proto"
)

func TestNextPingVariesAndStaysBelowTheIdleLimit(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 400; i++ {
		d := NextPing(25 * time.Second)
		if d < 15*time.Second || d > 35*time.Second {
			t.Fatalf("25 s base gave %v, want 15-35 s", d)
		}
		seen[d.Round(100*time.Millisecond)] = true
		if d := NextPing(60 * time.Second); d > 55*time.Second {
			t.Fatalf("60 s base gave %v: above the 80 s idle limit margin", d)
		}
	}
	if len(seen) < 50 {
		t.Fatalf("only %d distinct intervals in 400 draws", len(seen))
	}
}

// A peer that does not know the capability stream refuses it; Announce must not
// disturb the session or panic, and a peer that knows it hears the announcement.
func TestAnnounceReachesANewPeerAndIsRefusedByAnOldOne(t *testing.T) {
	for _, old := range []bool{false, true} {
		a, b := net.Pipe()
		cli, _ := yamux.Client(a, nil)
		srv, _ := yamux.Server(b, nil)
		heard := make(chan string, 1)
		go func() {
			if !old {
				Refuse(srv, func(p string) { heard <- p })
				return
			}
			for { // an older peer: reads the open header, answers "failure"
				st, err := srv.AcceptStream()
				if err != nil {
					return
				}
				go func() {
					_, _ = proto.ReadOpen(st)
					_ = proto.WriteReply(st, proto.ReplyFailure)
					_ = st.Close()
				}()
			}
		}()
		Announce(cli)
		select {
		case p := <-heard:
			if old || p != proto.CapPadding {
				t.Fatalf("old=%v heard %q", old, p)
			}
		case <-time.After(1500 * time.Millisecond):
			if !old {
				t.Fatal("a new peer never heard the announcement")
			}
		}
		if cli.IsClosed() {
			t.Fatal("the announcement broke the session")
		}
		cli.Close()
		srv.Close()
	}
}
