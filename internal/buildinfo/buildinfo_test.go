package buildinfo

import (
	"strings"
	"testing"
)

func TestCurrentContainsReleaseAndSchemaIdentity(t *testing.T) {
	got := Current(6, 2, "basic-analysis/v1", "analysis-export/v1")
	if got.ProductVersion == "" || got.GoVersion == "" || got.WailsVersion != WailsVersion {
		t.Fatalf("incomplete tool identity: %#v", got)
	}
	if got.DatabaseSchemaVersion != 6 || got.SettingsSchemaVersion != 2 {
		t.Fatalf("unexpected schema identity: %#v", got)
	}
	if got.AnalysisAlgorithmVersion != "basic-analysis/v1" || got.ExportSchemaVersion != "analysis-export/v1" {
		t.Fatalf("unexpected report identity: %#v", got)
	}
	if len(got.FFmpegSHA256) != 64 || strings.ToLower(got.FFmpegSHA256) != got.FFmpegSHA256 {
		t.Fatalf("invalid FFmpeg checksum: %q", got.FFmpegSHA256)
	}
}

func TestCurrentContainsNoHostPathOrCredential(t *testing.T) {
	got := Current(6, 2, "basic-analysis/v1", "analysis-export/v1")
	values := []string{got.ProductVersion, got.GitCommit, got.BuildTime, got.BuildSource, got.GoVersion,
		got.WailsVersion, got.NodeVersion, got.FFmpegVersion, got.FFmpegSHA256}
	for _, value := range values {
		lower := strings.ToLower(value)
		if strings.ContainsAny(value, `\\/`) || strings.Contains(lower, "cookie") || strings.Contains(lower, "token") {
			t.Fatalf("unsafe build value %q", value)
		}
	}
}
