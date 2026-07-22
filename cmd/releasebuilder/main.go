package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jwwsjlm/douyinLive/v2/internal/releasegate"
)

func main() {
	version := flag.String("version", "", "SemVer without v prefix; must match Wails and frontend metadata")
	output := flag.String("output", "release", "repository-relative release output root")
	source := flag.String("source", "local-release", "privacy-safe build source identifier")
	allowDirty := flag.Bool("allow-dirty", false, "allow a dirty tree for development validation only")
	verifyOnly := flag.Bool("verify-only", false, "run inventories and gates without building the desktop executable")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "RELEASE_VERSION_REQUIRED")
		os.Exit(2)
	}
	root, err := releasegate.FindRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "RELEASE_ROOT_INVALID:", err)
		os.Exit(1)
	}
	result, err := releasegate.Run(releasegate.BuildOptions{
		RepoRoot: root, Version: *version, OutputRoot: *output, BuildSource: *source,
		AllowDirty: *allowDirty, SkipBuild: *verifyOnly,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "RELEASE_GATE_FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("RELEASE_GATE_PASSED\n")
	fmt.Printf("output=%s\n", result.OutputDirectory)
	fmt.Printf("commit=%s\n", result.Metadata.Commit)
	fmt.Printf("dirty=%t\n", result.Metadata.Dirty)
	fmt.Printf("components=%d\n", result.ComponentCount)
	fmt.Printf("scannedFiles=%d\n", result.ScanFileCount)
	if result.ArtifactPath != "" {
		fmt.Printf("artifact=%s\n", result.ArtifactPath)
		fmt.Printf("artifactSHA256=%s\n", result.ArtifactSHA256)
		fmt.Printf("artifactSize=%d\n", result.ArtifactSize)
	}
}
