//go:build windows

package credentials

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestPlatformProtectorRoundTripsHighEntropyBinary(t *testing.T) {
	protector := platformProtector{}
	for attempt := 0; attempt < 16; attempt++ {
		plain := make([]byte, 32)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		protected, err := protector.Protect(plain)
		if err != nil {
			t.Fatalf("Protect attempt %d: %v", attempt, err)
		}
		restored, err := protector.Unprotect(protected)
		if err != nil {
			t.Fatalf("Unprotect attempt %d: %v", attempt, err)
		}
		if !bytes.Equal(restored, plain) {
			t.Fatalf("attempt %d did not round trip", attempt)
		}
	}
}
