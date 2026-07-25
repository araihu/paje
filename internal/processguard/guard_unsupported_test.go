//go:build !linux

package processguard

import "testing"

func TestHardenFailsClosedWithoutProcessIsolation(t *testing.T) {
	if err := Harden(); err == nil {
		t.Fatal("Harden() error = nil on unsupported platform")
	}
}
