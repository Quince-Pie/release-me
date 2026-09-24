package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Quince-Pie/release-me/internal/host/fake"
	"github.com/Quince-Pie/release-me/internal/manifest"
)

// TestRequestBudget records how many API requests a clean publication of n
// assets costs on each platform, with and without trusting the server
// digest, so that the price of the verify-before-publish gate is explicit
// (see docs/QUALIFICATION.md). It fails only if the count grows beyond the
// documented bound of 14 + 3n requests (GitHub downloads cost two requests each).
func TestRequestBudget(t *testing.T) {
	for _, kind := range kinds() {
		for _, trust := range []bool{false, true} {
			if kind == fake.Forgejo && trust {
				continue // Forgejo has no server digest; the flag changes nothing
			}
			e := setup(t, kind)
			// Six archives like a real release, plus manifest and signature.
			for i := 0; i < 4; i++ {
				write(t, e.dir, fmt.Sprintf("tool_1.0.0_extra%d.tar.gz", i), strings.Repeat("x", 100+i))
			}
			m, _ := manifest.FromDir(e.dir)
			os.WriteFile(filepath.Join(e.dir, manifest.FileName), m.Bytes(), 0o644)
			o := e.opts
			o.TrustServerDigest = trust
			o.Logf = nil
			if _, err := Run(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			n := 8 // 6 archives + SHA256SUMS + SHA256SUMS.sig
			total := len(e.s.Log)
			uploads := e.s.Count("POST") - 1 // minus the draft creation
			downloads := e.s.Count("GET /objects/") + e.s.Count("GET /attachments/")
			t.Logf("%s trust-digest=%v: %d requests for %d assets (%d uploads, %d asset downloads)", kind, trust, total, n, uploads, downloads)
			if total > 14+3*n {
				t.Errorf("%s: %d requests exceeds the documented bound %d", kind, total, 14+3*n)
			}
		}
	}
}
