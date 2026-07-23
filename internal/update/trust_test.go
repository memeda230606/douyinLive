package update

import (
	"net/url"
	"testing"
)

func TestProductionTrustConfiguration(t *testing.T) {
	keys, err := ProductionTrustedKeys()
	origin, parseErr := url.Parse(ProductionBaseURL)
	if err != nil || parseErr != nil || len(keys[ProductionKeyID]) != 32 {
		t.Fatalf("trust configuration = (%v, %v, %v)", keys, err, parseErr)
	}
	if origin.Scheme != "https" || origin.Host == "" || ProductionChannel != "stable" {
		t.Fatalf("production endpoint = %q/%q", ProductionBaseURL, ProductionChannel)
	}
}
