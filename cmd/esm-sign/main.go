// Command esm-sign signs a release's SHA256SUMS with the release key, and
// makes a new key pair.
//
// Signing (the release workflow does this):
//
//	ESM_RELEASE_KEY=<base64 seed> esm-sign -in SHA256SUMS -out SHA256SUMS.sig
//
// The key comes only from the environment, never from the command line.
// The signature is checked against the public key built into ESM before it
// is written, so a release key that does not belong to ESM fails here and
// not on every box that tries to update.
//
// A new key pair:
//
//	esm-sign -keygen <file>
//
// writes the private seed (base64) to the new file <file>, readable by its
// owner only, and prints the public key, which goes into
// internal/release.PublicKey. The private seed is never printed.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// KeyEnv names the environment variable that holds the private seed.
const KeyEnv = "ESM_RELEASE_KEY"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv(KeyEnv), os.Stdout, os.Stderr))
}

func run(args []string, key string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("esm-sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "file to sign (SHA256SUMS)")
	out := fs.String("out", "", "signature file to write (SHA256SUMS.sig)")
	keygen := fs.String("keygen", "", "write a new private seed to this new file and print the public key")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	var err error
	switch {
	case *keygen != "":
		err = makeKey(*keygen, stdout)
	case *in != "" && *out != "":
		err = sign(*in, *out, key, release.BuiltinKey())
	default:
		fmt.Fprintln(stderr, "usage: esm-sign -in SHA256SUMS -out SHA256SUMS.sig   (key in $"+KeyEnv+")\n       esm-sign -keygen <file>")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "esm-sign:", err)
		return 1
	}
	return 0
}

func makeKey(path string, stdout io.Writer) error {
	pub, priv, err := release.NewKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the key file must be new: %w", err)
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(pub))
	return nil
}

func sign(in, out, key string, builtin ed25519.PublicKey) error {
	if key == "" {
		return errors.New("$" + KeyEnv + " is empty: set the release key (the repository secret " + KeyEnv + ")")
	}
	priv, err := release.ParseSeed(key)
	if err != nil {
		return fmt.Errorf("$%s: %w", KeyEnv, err)
	}
	msg, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	sig := release.Sign(priv, msg)
	if err := release.Verify(builtin, msg, sig); err != nil {
		return errors.New("the key in $" + KeyEnv + " does not belong to the public key built into ESM (internal/release.PublicKey)")
	}
	return os.WriteFile(out, sig, 0o644)
}
