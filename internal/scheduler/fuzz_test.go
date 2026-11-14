package scheduler

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// reader hands out values from fuzz bytes, then zeros once they run out.
type reader struct{ b []byte }

func (r *reader) byte() byte {
	if len(r.b) == 0 {
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) u64() uint64 {
	var buf [8]byte
	for i := range buf {
		buf[i] = r.byte()
	}
	return binary.LittleEndian.Uint64(buf[:])
}

// cpu returns a small multiple of 0.1 most of the time, and any float64
// (NaN, infinities, negatives, huge values) otherwise.
func (r *reader) cpu() float64 {
	c := r.byte()
	if c >= 0xf0 {
		return math.Float64frombits(r.u64())
	}
	return float64(c%64) / 10
}

func (r *reader) mem() int64 {
	c := r.byte()
	if c >= 0xf0 {
		return int64(r.u64())
	}
	return int64(c%16) << 28
}

func (r *reader) pick(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[int(r.byte())%len(list)]
}

// FuzzPlace builds a cluster from the fuzz bytes. The ports string is split on
// commas into the pool of host-port entries, so the fuzzer also explores
// malformed ones. Place must not panic, and checkInvariants must hold.
func FuzzPlace(f *testing.F) {
	f.Add([]byte{3, 40, 4, 1, 0, 40, 4, 1, 0, 40, 4, 1, 0, 0, 4, 1, 10, 2, 1, 1, 0}, "0.0.0.0:80/tcp,127.0.0.1:80/tcp")
	f.Add([]byte{2, 0xf0, 0, 0, 0, 0, 0, 0xf8, 0x7f, 1, 1, 0}, "80/tcp,[::]:80/tcp,garbage")
	f.Add([]byte{}, "")
	f.Add([]byte{5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, "10.0.0.1:80/tcp,10.0.0.2:80/tcp,[::1]:53/udp,99999/tcp,/")
	f.Fuzz(func(t *testing.T, data []byte, ports string) {
		pool := strings.Split(ports, ",")
		if len(pool) > 16 {
			pool = pool[:16]
		}
		r := &reader{b: data}
		ids := []string{"a", "b", "c", "d", "e", "f", ""}

		var nodes []Node
		for range int(r.byte() % 8) {
			nodes = append(nodes, Node{
				ID:         r.pick(ids),
				CPU:        r.cpu(),
				Memory:     r.mem(),
				Ready:      r.byte()%4 != 0,
				ReadySince: now.Add(-time.Duration(int8(r.byte())) * time.Second),
			})
		}
		mk := func(i int) Replica {
			rp := Replica{
				App:        r.pick([]string{"web", "db", "api"}),
				Index:      i,
				Generation: int64(r.byte() % 3),
				Resources:  spec.Resources{CPU: r.cpu(), Memory: spec.ByteSize(r.mem())},
			}
			for range int(r.byte() % 4) {
				rp.HostPorts = append(rp.HostPorts, r.pick(pool))
			}
			if r.byte()%4 == 0 {
				rp.PinnedNode = r.pick(ids)
			}
			return rp
		}
		var existing []Assignment
		for i := range int(r.byte() % 8) {
			existing = append(existing, Assignment{Replica: mk(-1 - i), NodeID: r.pick(ids)})
		}
		var pending []Replica
		for i := range int(r.byte() % 12) {
			pending = append(pending, mk(i))
		}
		opts := Options{MinReady: time.Duration(int8(r.byte())) * time.Second}
		checkInvariants(t, nodes, existing, pending, now, opts)
	})
}
