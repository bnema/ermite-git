// SPDX-License-Identifier: MIT

package git

import "testing"

// BenchmarkResolveRefMiss is what a revision lookup costs for the candidates
// that do not exist: the adapter tries heads, then tags, then the name as
// written.
func BenchmarkResolveRefMiss(b *testing.B) {
	fx := getBench(b)
	names := []string{"refs/heads/no-such-branch", "refs/tags/no-such-tag", "refs/remotes/origin/no-such-branch"}
	b.Run("LookupRefID", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			if _, ok, err := fx.r.LookupRefID(names[i%len(names)]); err != nil || ok {
				b.Fatal("found or failed", err)
			}
			i++
		}
	})
	b.Run("ResolveRef", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			if _, err := fx.r.ResolveRef(names[i%len(names)]); err == nil {
				b.Fatal("found")
			}
			i++
		}
	})
}
