package install

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The control plane and workers run an Orchard built at OrchardForkCommit, and grove's own client
// code is compiled against go.mod's replace of the same fork. Two different commits would let a
// fix in the fork ship in one and not the other, so they are one pin.
func TestOrchardForkCommitIsTheCommitGoModBuildsAgainst(t *testing.T) {
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^replace github\.com/cirruslabs/orchard => github\.com/gm2211/orchard v[0-9.]+-\d{14}-([0-9a-f]{12})$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no pseudo-version replace of github.com/cirruslabs/orchard with github.com/gm2211/orchard")
	}
	if !strings.HasPrefix(OrchardForkCommit, string(m[1])) {
		t.Errorf("go.mod builds against gm2211/orchard %s, but installs build Orchard at OrchardForkCommit %s; pin both to the same commit", m[1], OrchardForkCommit)
	}
}
