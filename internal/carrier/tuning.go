package carrier

import "sync/atomic"

// ChunkTuning is the chunk size the chunk carrier asks for, shared by every session
// of a client. A session that gets stuck (a request that cannot pass for half a
// minute) takes a third off the next one; a session that ran for a few minutes
// without trouble adds a tenth, up to the configured size. So the client settles on
// what the path lets through, without ever cutting an already numbered chunk.
type ChunkTuning struct {
	up, down       atomic.Int64
	maxUp, maxDown int64
	minUp, minDown int64
}

// Smallest sizes: the server refuses downstream budgets under 4 KB.
const (
	minTunedUp   = 2 << 10
	minTunedDown = 4 << 10
)

// NewChunkTuning starts at the configured sizes, which are also the ceiling.
func NewChunkTuning(up, down int) *ChunkTuning {
	t := &ChunkTuning{maxUp: int64(up), maxDown: int64(down), minUp: min(int64(up), minTunedUp), minDown: min(int64(down), minTunedDown)}
	t.up.Store(int64(up))
	t.down.Store(int64(down))
	return t
}

// Sizes returns the upstream body size and downstream payload budget for a new session.
func (t *ChunkTuning) Sizes() (up, down int) { return int(t.up.Load()), int(t.down.Load()) }

func shrink(v *atomic.Int64, floor int64) {
	for {
		c := v.Load()
		if v.CompareAndSwap(c, max(floor, c*2/3)) {
			return
		}
	}
}

func grow(v *atomic.Int64, ceil int64) {
	for {
		c := v.Load()
		if v.CompareAndSwap(c, min(ceil, c*11/10+1)) {
			return
		}
	}
}

// Shrink takes a third off both sizes.
func (t *ChunkTuning) Shrink() { shrink(&t.up, t.minUp); shrink(&t.down, t.minDown) }

// Grow adds a tenth to both sizes, up to the configured ones.
func (t *ChunkTuning) Grow() { grow(&t.up, t.maxUp); grow(&t.down, t.maxDown) }
