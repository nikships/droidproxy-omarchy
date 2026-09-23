// Command release builds the release artifacts for DroidProxy: key generation,
// signing, update-feed manifests, and verification.
//
// Subcommands:
//
//	release keygen <private-key-file>   print the base64 public key, write the
//	                                    base64 private key to the file (0600)
//	release sign <file>                 write <file>.sig using the private key
//	                                    from $DROIDPROXY_SIGNING_KEY
//	release manifest --version V --tag T --notes-file F --dist DIR --base-url URL
//	                                    write latest.json + SHA256SUMS from the
//	                                    tarballs and signatures in DIR
//	release verify <file>               verify <file>.sig against the embedded
//	                                    public key
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	var err error
	switch cmd {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "release: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`usage: release <subcommand> [args]

  keygen <file>              generate an ed25519 keypair; print the base64
                             public key to stdout and write the base64
                             private key to <file> with mode 0600
  sign <file>                write <file>.sig (base64 ed25519 signature over
                             the raw file bytes); the private key comes from
                             $DROIDPROXY_SIGNING_KEY (base64)
  manifest --version V --tag T --notes-file F --dist DIR --base-url URL
                             write DIR/latest.json and DIR/SHA256SUMS from
                             the *.tar.gz and *.tar.gz.sig files in DIR
  verify <file>              verify <file>.sig against the embedded public
                             key and the file's raw bytes
`)
}

func fail(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func keygen(args []string) error {
	if len(args) != 1 {
		return fail("keygen requires exactly one file argument")
	}
	out := args[0]
	if _, err := os.Stat(out); err == nil {
		return fail("%s already exists; refusing to overwrite a key", out)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(priv[32:]))
	return nil
}

func sign(args []string) error {
	if len(args) != 1 {
		return fail("sign requires exactly one file argument")
	}
	path := args[0]
	keyB64 := os.Getenv("DROIDPROXY_SIGNING_KEY")
	if keyB64 == "" {
		return fail("DROIDPROXY_SIGNING_KEY is not set (base64 ed25519 private key)")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return fail("DROIDPROXY_SIGNING_KEY is not a valid base64 ed25519 private key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.PrivateKey(key), data)
	return os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)), 0o644)
}

func verify(args []string) error {
	if len(args) != 1 {
		return fail("verify requires exactly one file argument")
	}
	path := args[0]
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		return err
	}
	if err := verifyAgainstEmbeddededKey(path, sig); err != nil {
		return err
	}
	fmt.Printf("OK %s\n", path)
	return nil
}

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	version := fs.String("version", "", "release version (e.g. 1.2.3)")
	tag := fs.String("tag", "", "release tag (e.g. v1.2.3)")
	notesFile := fs.String("notes-file", "", "file with release notes (markdown)")
	dist := fs.String("dist", "", "directory with the tarballs and signatures")
	baseURL := fs.String("base-url", "", "public URL the tarballs will be served from")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" || *tag == "" || *dist == "" || *baseURL == "" {
		return fail("manifest requires --version, --tag, --dist and --base-url")
	}
	notes := ""
	if *notesFile != "" {
		data, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		notes = string(data)
	}

	assets := map[string]map[string]any{}
	sums := []string{}
	for _, plat := range []string{"linux-amd64", "linux-arm64"} {
		name := fmt.Sprintf("droidproxy-%s-%s.tar.gz", *version, plat)
		path := filepath.Join(*dist, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fail("missing tarball for %s: %v", plat, err)
		}
		sigPath := path + ".sig"
		sig, err := os.ReadFile(sigPath)
		if err != nil {
			return fail("missing signature for %s: %v", plat, err)
		}
		assets[plat] = map[string]any{
			"url":       strings.TrimSuffix(*baseURL, "/") + "/" + name,
			"sha256":    fmt.Sprintf("%x", sha256Sum(data)),
			"size":      len(data),
			"signature": strings.TrimSpace(string(sig)),
		}
		sums = append(sums, fmt.Sprintf("%x  %s", sha256Sum(data), name))
		// Also verify the signature we are about to publish.
		if err := verifyFn(data, sig); err != nil {
			return fail("signature for %s does not verify against the embedded key: %v", name, err)
		}
	}

	feed := map[string]any{
		"version":     *version,
		"tag":         *tag,
		"publishedAt": nowRFC3339(),
		"notes":       notes,
		"releaseUrl":  releaseURLFor(*tag),
		"assets":      assets,
	}
	feedJSON, err := json.MarshalIndent(feed, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dist, "latest.json"), append(feedJSON, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dist, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
}
