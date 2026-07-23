package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jwwsjlm/douyinLive/v2/internal/update"
)

func main() {
	job := flag.String("job", "", "absolute path to a validated update install job")
	flag.Parse()
	if *job == "" || !filepath.IsAbs(*job) {
		fmt.Fprintln(os.Stderr, "UPDATE_INSTALL_JOB_PATH_INVALID")
		os.Exit(2)
	}
	absolute, err := filepath.Abs(filepath.Clean(*job))
	if err != nil || absolute != *job {
		fmt.Fprintln(os.Stderr, "UPDATE_INSTALL_JOB_PATH_INVALID")
		os.Exit(2)
	}
	if err := update.RunInstallHelper(*job); err != nil {
		code := update.ErrorCode(err)
		fmt.Fprintln(os.Stderr, code)
		os.Exit(1)
	}
	fmt.Println("UPDATE_INSTALL_PASSED")
}
