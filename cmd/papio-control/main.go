// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Command papio-control is the maintainer tool for signed adapter control
// (ADR-0021 Decision 4). It is deliberately a separate binary, not a papio
// subcommand: users never need the signing role, and goreleaser does not ship
// it.
//
//	papio-control keygen -out <private key file>
//	papio-control sign   -key <private key file> -sequence N [-suspend-revision ID]... [-suspend-domain ID]... [-out file]
//	papio-control sign   -key <private key file> -in <document.json> [-out file]
//	papio-control verify -pub <base64 key or file> <envelope file>
//
// `sign` writes the envelope to commit as control/adapter-control.json. A
// higher -sequence with fewer suspensions lifts a suspension.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"papio/internal/adaptercontrol"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "papio-control:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	if len(args) == 0 {
		return errors.New("usage: papio-control keygen|sign|verify (see -h of each)")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:], stdout, stderr)
	case "sign":
		return sign(args[1:], stdout, stderr, now)
	case "verify":
		return verify(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q (want keygen, sign, or verify)", args[0])
	}
}

func keygen(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "", "file to write the PEM private key to (mode 0600; refuses to overwrite)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || flags.NArg() != 0 {
		return errors.New("keygen needs -out <path>")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// O_CREATE honours the umask; the key must be 0600 whatever it is.
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if err := pem.Encode(file, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	fmt.Fprintf(stderr, "private key written to %s (mode 0600). Keep it offline and out of the repository.\n", *out)
	fmt.Fprintf(stderr, "Set the embedded public key in internal/adaptercontrol/trust.go to the value on stdout.\n")
	fmt.Fprintln(stdout, encoded)
	return nil
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func sign(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyPath := flags.String("key", "", "private key file (PEM PKCS#8, raw 32-byte seed or 64-byte key, or their base64)")
	in := flags.String("in", "", "document JSON to sign (schema and issued_at default); alternative to the flags below")
	out := flags.String("out", "", "write the envelope here instead of stdout")
	sequence := flags.Int64("sequence", 0, "monotonic sequence (> the last published one)")
	keyID := flags.String("key-id", "papio-control-1", "key identifier recorded in the document")
	issuedAt := flags.String("issued-at", "", "RFC 3339 issue time (default now)")
	var revisions, domains listFlag
	flags.Var(&revisions, "suspend-revision", "exact packaged revision ID to suspend, e.g. sciencedirect@0.3.0 (repeatable)")
	flags.Var(&domains, "suspend-domain", "safety domain ID to suspend (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || flags.NArg() != 0 {
		return errors.New("sign needs -key <path>")
	}
	key, err := readPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	var doc adaptercontrol.Document
	if *in != "" {
		if *sequence != 0 || len(revisions) != 0 || len(domains) != 0 {
			return errors.New("-in cannot be combined with -sequence, -suspend-revision, or -suspend-domain")
		}
		raw, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&doc); err != nil {
			return fmt.Errorf("reading %s: %w", *in, err)
		}
	} else {
		doc = adaptercontrol.Document{Sequence: *sequence, KeyID: *keyID, SuspendedRevisions: revisions, SuspendedSafetyDomains: domains}
	}
	if doc.IssuedAt == "" {
		doc.IssuedAt = now().UTC().Format(time.RFC3339)
		if *issuedAt != "" {
			doc.IssuedAt = *issuedAt
		}
	}
	envelope, err := adaptercontrol.Sign(doc, key)
	if err != nil {
		return err
	}
	// Refuse to emit anything the daemon would not accept under this key.
	if _, err := adaptercontrol.Verify(envelope, key.Public().(ed25519.PublicKey)); err != nil {
		return fmt.Errorf("signed envelope does not verify: %w", err)
	}
	if *out != "" {
		return os.WriteFile(*out, envelope, 0o644) //nolint:gosec // G306: a signed envelope is published to every daemon; it holds no secret.
	}
	_, err = stdout.Write(envelope)
	return err
}

func verify(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pub := flags.String("pub", "", "public key: base64, or a file holding it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *pub == "" || flags.NArg() != 1 {
		return errors.New("verify needs -pub <key> and one envelope file")
	}
	encoded := strings.TrimSpace(*pub)
	if raw, err := os.ReadFile(encoded); err == nil {
		encoded = strings.TrimSpace(string(raw))
	}
	key, err := adaptercontrol.ParsePublicKey(encoded)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(flags.Arg(0))
	if err != nil {
		return err
	}
	verified, err := adaptercontrol.Verify(raw, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: sequence %d, %d revision(s), %d safety domain(s) suspended, digest %s\n",
		verified.Document.Sequence, len(verified.Document.SuspendedRevisions), len(verified.Document.SuspendedSafetyDomains), verified.Digest)
	return nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(raw); block != nil {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing PEM private key: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("PEM key is not Ed25519")
		}
		return key, nil
	}
	if key, ok := rawKey(raw); ok {
		return key, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil {
		if key, ok := rawKey(decoded); ok {
			return key, nil
		}
	}
	return nil, errors.New("key file is not an Ed25519 PEM, a raw 32-byte seed or 64-byte key, or base64 of one")
}

func rawKey(raw []byte) (ed25519.PrivateKey, bool) {
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), true
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(append([]byte(nil), raw...)), true
	}
	return nil, false
}
