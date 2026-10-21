package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestStringDefaults(t *testing.T) {
	got := String()
	for _, want := range []string{"dev (commit unknown, unknown, ", runtime.Version()} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
