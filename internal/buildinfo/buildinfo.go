// Package buildinfo exposes the immutable, privacy-safe release identity used
// by the desktop About and diagnostics surfaces.
package buildinfo

import "runtime"

const (
	WailsVersion        = "2.13.0"
	FFmpegVersion       = "8.1.2-essentials_build-www.gyan.dev"
	FFmpegSHA256        = "1326dde4c84ff1f96fe6b8916c5bed29e163e9b5dccf995f6f3db069d143ec5e"
	FFmpegArchiveSHA256 = "db580001caa24ac104c8cb856cd113a87b0a443f7bdf47d8c12b1d740584a2ec"
	FFmpegLicense       = "GPL-3.0-or-later"
)

// These values are overridden only by the audited release builder through
// Go -ldflags -X. Defaults are explicit so local builds remain diagnosable.
var (
	productVersion = "0.1.0-dev"
	gitCommit      = "unknown"
	buildTime      = "unknown"
	buildSource    = "local"
	nodeVersion    = "unknown"
)

type Info struct {
	ProductVersion           string `json:"productVersion"`
	GitCommit                string `json:"gitCommit"`
	BuildTime                string `json:"buildTime"`
	BuildSource              string `json:"buildSource"`
	GoVersion                string `json:"goVersion"`
	WailsVersion             string `json:"wailsVersion"`
	NodeVersion              string `json:"nodeVersion"`
	FFmpegVersion            string `json:"ffmpegVersion"`
	FFmpegSHA256             string `json:"ffmpegSHA256"`
	FFmpegLicense            string `json:"ffmpegLicense"`
	DatabaseSchemaVersion    int    `json:"databaseSchemaVersion"`
	SettingsSchemaVersion    int    `json:"settingsSchemaVersion"`
	AnalysisAlgorithmVersion string `json:"analysisAlgorithmVersion"`
	ExportSchemaVersion      string `json:"exportSchemaVersion"`
}

func Current(databaseSchemaVersion, settingsSchemaVersion int, analysisAlgorithmVersion, exportSchemaVersion string) Info {
	return Info{
		ProductVersion: productVersion, GitCommit: gitCommit, BuildTime: buildTime,
		BuildSource: buildSource, GoVersion: runtime.Version(), WailsVersion: WailsVersion,
		NodeVersion: nodeVersion, FFmpegVersion: FFmpegVersion,
		FFmpegSHA256: FFmpegSHA256, FFmpegLicense: FFmpegLicense,
		DatabaseSchemaVersion: databaseSchemaVersion, SettingsSchemaVersion: settingsSchemaVersion,
		AnalysisAlgorithmVersion: analysisAlgorithmVersion, ExportSchemaVersion: exportSchemaVersion,
	}
}
