package bot

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// copySources are the files that hold user-visible text.
var copySources = []string{"messages.go", "keyboards.go", "state.go", "service.go"}

// Stripping the "(Step n/4)" markers left strings like "🚀: Starting booking...".
// An emoji sitting directly on a colon is always that kind of leftover.
var orphanColon = regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}\x{2B00}-\x{2BFF}] ?:`)

// deadRefs are button labels and step markers that no longer exist anywhere in
// the UI. Copy that names them tells the user to press something that is not
// on their screen -- exactly the bug that made the original login unusable.
var deadRefs = []string{
	"Run All Accounts",
	"▶️ Run",
	"🔐 Login",
	"🔑 Login",
	"Step 1/4", "Step 2/4", "Step 3/4", "Step 4/4",
	"Fetch Patients",
	"➕ Add Account",
}

func TestCopyHasNoOrphanedColons(t *testing.T) {
	for _, f := range copySources {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `"`) {
				continue
			}
			if m := orphanColon.FindString(line); m != "" {
				t.Errorf("%s:%d: orphaned colon after emoji (%q): %s", f, i+1, m, strings.TrimSpace(line))
			}
		}
	}
}

func TestCopyDoesNotNameButtonsThatDoNotExist(t *testing.T) {
	for _, f := range copySources {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(line, `"`) {
				continue
			}
			for _, dead := range deadRefs {
				idx := strings.Index(line, dead)
				if idx < 0 {
					continue
				}
				// A dead label that is merely the prefix of a longer word is
				// not a reference ("▶️ Run" inside the status string "▶️ Running").
				if r, _ := utf8.DecodeRuneInString(line[idx+len(dead):]); unicode.IsLetter(r) {
					continue
				}
				t.Errorf("%s:%d: copy names a button that no longer exists (%q): %s", f, i+1, dead, trimmed)
			}
		}
	}
}
