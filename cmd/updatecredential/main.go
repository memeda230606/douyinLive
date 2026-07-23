package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jwwsjlm/douyinLive/v2/internal/update"
)

func main() {
	output := flag.String("output", "", "absolute DPAPI LocalMachine credential file")
	flag.Parse()
	if *output == "" || !filepath.IsAbs(*output) {
		fmt.Fprintln(os.Stderr, "UPDATE_PUBLISH_CREDENTIAL_PATH_INVALID")
		os.Exit(2)
	}
	absolute, err := filepath.Abs(filepath.Clean(*output))
	if err != nil || absolute != *output {
		fmt.Fprintln(os.Stderr, "UPDATE_PUBLISH_CREDENTIAL_PATH_INVALID")
		os.Exit(2)
	}
	if err := update.CreateProtectedPublishingCredentials(*output, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, update.ErrorCode(err))
		os.Exit(1)
	}
	fmt.Println("UPDATE_PUBLISH_CREDENTIAL_CREATED")
}
