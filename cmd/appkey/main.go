// Command appkey makes an app's Ed25519 key. The private seed (base64) goes
// to stdout, for piping straight into a secret store; the public key, for
// WAKESHIFT_APPS, goes to stderr.
//
//	go run ./cmd/appkey | gcloud secrets versions add my-app-key --data-file=-
//
// With -app <id>, it prints instead, on stdout, the two lines a server you
// run yourself and your app need: the app's key for the SDKs
// (WAKESHIFT_KEY=wst_…) and the server's WAKESHIFT_APPS, which registers
// the app by its public key.
//
//	go run ./cmd/appkey -app example
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wakeshift/server/internal/appkey"
)

func main() {
	app := flag.String("app", "", "print WAKESHIFT_KEY and WAKESHIFT_APPS lines for an app with this ID (lowercase letters, digits and dashes)")
	name := flag.String("name", "", "with -app: the app's name (default: its ID)")
	flag.Parse()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *app != "" {
		if err := writeEnv(os.Stdout, *app, *name, seed); err != nil {
			fmt.Fprintln(os.Stderr, "appkey:", err)
			os.Exit(2)
		}
		return
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	fmt.Print(base64.StdEncoding.EncodeToString(seed))
	fmt.Fprintln(os.Stderr, "public key:", base64.StdEncoding.EncodeToString(pub))
}

// writeEnv writes the app's key and its WAKESHIFT_APPS entry
// (appkey.ForServer, which the release gate uses too), as environment
// lines, for the key with seed.
func writeEnv(w io.Writer, id, name string, seed []byte) error {
	key, apps, err := appkey.ForServer(id, name, seed)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "WAKESHIFT_KEY=%s\nWAKESHIFT_APPS=%s\n", key, apps)
	return err
}
