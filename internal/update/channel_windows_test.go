//go:build windows

package update

import "testing"

func TestValidateProductionUpdateChannel(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		configured bool
		want       string
		wantError  bool
	}{
		{name: "default", configured: false, want: "stable"},
		{name: "stable", value: "stable", configured: true, want: "stable"},
		{name: "canary", value: " CANARY ", configured: true, want: "canary"},
		{name: "invalid", value: "preview", configured: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateProductionUpdateChannel(test.value, test.configured)
			if got != test.want || (err != nil) != test.wantError {
				t.Fatalf("validateProductionUpdateChannel() = (%q, %v)", got, err)
			}
		})
	}
}
