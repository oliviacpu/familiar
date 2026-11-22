// familiar-sign signs a release's SHA256SUMS with the release key, for the
// release workflow. The key is FAMILIAR_SIGNING_KEY: a base64 Ed25519 seed
// (32 bytes). The signature is written as base64.
//
//	familiar-sign SHA256SUMS > SHA256SUMS.sig
//	familiar-sign -public     # prints the public key for update.go
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("FAMILIAR_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(os.Stderr, "familiar-sign: FAMILIAR_SIGNING_KEY must be a base64 32-byte seed")
		os.Exit(1)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if len(os.Args) == 2 && os.Args[1] == "-public" {
		fmt.Println(base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: familiar-sign <file> | -public")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "familiar-sign:", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(key, data)))
}
