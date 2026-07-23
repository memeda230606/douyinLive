package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/jwwsjlm/douyinLive/v2/internal/update"
)

func main() {
	keyID := flag.String("key-id", "", "stable identifier for this release signing key")
	output := flag.String("output", "", "absolute path outside the repository for the DPAPI-protected key")
	flag.Parse()
	if runtime.GOOS != "windows" {
		fmt.Fprintln(os.Stderr, "UPDATE_SIGNING_KEY_WINDOWS_REQUIRED")
		os.Exit(2)
	}
	if *keyID == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "UPDATE_SIGNING_KEY_ARGUMENT_INVALID")
		os.Exit(2)
	}
	publicKey, err := update.CreateProtectedSigningKey(*output, *keyID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "UPDATE_SIGNING_KEY_CREATE_FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("UPDATE_SIGNING_KEY_CREATED")
	fmt.Println("keyId=" + *keyID)
	fmt.Println("publicKey=" + publicKey)
}
