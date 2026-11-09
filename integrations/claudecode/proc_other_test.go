//go:build !unix

package claudecode

import (
	"strings"
	"testing"
)

// TestNewRefusesPlatform: without process groups the harness cannot end a turn's tool processes,
// so New refuses before it looks at the binary or the rest of the config.
func TestNewRefusesPlatform(t *testing.T) {
	h, err := New(Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("New = %v, %v, want an unsupported platform error", h, err)
	}
}
