// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"sync/atomic"
	"testing"
)

// The benchmarks of bench_test.go read one object over and over, which a cache
// answers from its first hit. These are the cold ones. Each has a serial and
// a parallel variant (b.RunParallel, GOMAXPROCS goroutines, each starting at
// another object) since a server reads from many requests at once:
//
//   - "nocache": WithDeltaCacheSize(0), so a deltified object is rebuilt from
//     its base each time, with the cost of inflating the base;
//   - "rotating": a different object each time, from a set of up to 8192.

// coldSet returns up to n objects of the repository of the fixture, spread
// over its packs, with their kinds: the first return holds the deltified
// ones, the second the others.
func coldSet(tb testing.TB, r *Repo, n int) (deltified, plain []ObjectID) {
	tb.Helper()
	s := r.snap.Load()
	for _, p := range s.packs {
		step := max(1, int(p.idx.n)/(2*n))
		for i := uint32(0); i < p.idx.n && (len(deltified) < n || len(plain) < n); i += uint32(step) {
			off, err := p.idx.offset(i)
			if err != nil {
				tb.Fatal(err)
			}
			h, err := parseEntryHeader(p.data, off)
			if err != nil {
				tb.Fatal(err)
			}
			var id ObjectID
			copy(id[:], p.idx.id(i))
			switch {
			case h.isDelta() && len(deltified) < n:
				deltified = append(deltified, id)
			case !h.isDelta() && len(plain) < n:
				plain = append(plain, id)
			}
		}
	}
	if len(deltified) == 0 {
		tb.Fatal("no deltified object in the packs")
	}
	return deltified, plain
}

// reopen opens the repository of the fixture again, without the work of
// the timer.
func reopen(b *testing.B, fx *benchFixture, opts ...Option) *Repo {
	b.Helper()
	dir := fx.r.Dir()
	r, err := Open(dir, opts...)
	if err != nil {
		b.Fatal(err)
	}
	return r
}

// runSerialAndParallel runs the step returned by setup b.N times in one
// goroutine ("serial"), then b.N times spread over RunParallel's goroutines
// ("parallel", one setup per goroutine). mk opens what the steps use.
func runSerialAndParallel(b *testing.B, mk func(b *testing.B) (setup func() func(i int) error, closeFn func())) {
	b.Run("serial", func(b *testing.B) {
		setup, closeFn := mk(b)
		defer closeFn()
		step := setup()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := step(i); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		setup, closeFn := mk(b)
		defer closeFn()
		var worker atomic.Int64
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			step := setup()
			i := int(worker.Add(1)) * 7919
			for pb.Next() {
				if err := step(i); err != nil {
					b.Error(err)
					return
				}
				i++
			}
		})
	})
}

// BenchmarkReadObjectCold: nocache/<kind> reads one deltified object with no
// delta cache; rotating reads a different object each time (deltified and
// plain, default cache); rotating-nocache the same without the cache.
func BenchmarkReadObjectCold(b *testing.B) {
	fx := getBench(b)
	delta, plain := coldSet(b, fx.r, 4096)
	mixed := append(append([]ObjectID(nil), delta...), plain...)
	for _, c := range []struct {
		name  string
		ids   []ObjectID
		cache bool
	}{
		{"nocache/deltified-blob", []ObjectID{fx.deltaBlob}, false},
		{"nocache/deltified-tree", []ObjectID{fx.deltaTree}, false},
		{"rotating-nocache", mixed, false},
		{"rotating", mixed, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			runSerialAndParallel(b, func(b *testing.B) (func() func(int) error, func()) {
				var opts []Option
				if !c.cache {
					opts = append(opts, WithDeltaCacheSize(0))
				}
				r := reopen(b, fx, opts...)
				return func() func(int) error {
					var buf []byte
					return func(i int) error {
						_, data, err := r.ReadObject(c.ids[i%len(c.ids)], buf)
						buf = data
						return err
					}
				}, func() { _ = r.Close() }
			})
		})
	}
}

// BenchmarkObjectHeaderCold: first-lap reads the header of a different
// deltified object each time and reopens the Repo (so nothing is cached) after
// every lap over the set: the cold cost alone. warm-rotating reads them again
// and again, as GetTree does for the files of a directory.
func BenchmarkObjectHeaderCold(b *testing.B) {
	fx := getBench(b)
	delta, _ := coldSet(b, fx.r, 4096)
	b.Run("deltified-first-lap", func(b *testing.B) {
		b.ReportAllocs()
		r := reopen(b, fx)
		defer func() { _ = r.Close() }()
		for i := 0; i < b.N; i++ {
			if i > 0 && i%len(delta) == 0 {
				b.StopTimer()
				_ = r.Close()
				r = reopen(b, fx)
				b.StartTimer()
			}
			if _, _, err := r.ObjectHeader(delta[i%len(delta)]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("deltified-warm-rotating", func(b *testing.B) {
		r := reopen(b, fx)
		defer func() { _ = r.Close() }()
		for _, id := range delta { // warm the cache
			if _, _, err := r.ObjectHeader(id); err != nil {
				b.Fatal(err)
			}
		}
		runSerialAndParallel(b, func(b *testing.B) (func() func(int) error, func()) {
			return func() func(int) error {
				return func(i int) error {
					_, _, err := r.ObjectHeader(delta[i%len(delta)])
					return err
				}
			}, func() {}
		})
	})
}

// BenchmarkWalkCold walks 100 commits from a different start each time, with
// no delta cache: the commits come from the pack, deltified or not, and the
// graph if the repository has one.
func BenchmarkWalkCold(b *testing.B) {
	fx := getBench(b)
	starts := fx.allCommits
	if len(starts) > 4096 {
		starts = starts[:4096]
	}
	for _, shape := range []struct {
		name   string
		idOnly bool
	}{{"ids", true}, {"full", false}} {
		b.Run(shape.name, func(b *testing.B) {
			runSerialAndParallel(b, func(b *testing.B) (func() func(int) error, func()) {
				r := reopen(b, fx, WithDeltaCacheSize(0))
				return func() func(int) error {
					return func(i int) error {
						for _, err := range r.Walk(context.Background(), starts[i%len(starts)], WalkOptions{Limit: 100, IDOnly: shape.idOnly}) {
							if err != nil {
								return err
							}
						}
						return nil
					}
				}, func() { _ = r.Close() }
			})
		})
	}
}
