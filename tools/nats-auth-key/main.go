// Command nats-auth-key generates one NATS account NKey seed/public pair for
// the broker authorization callout.
//
// scripts/bootstrap-secrets calls it to create the issuer.seed and issuer.pub
// keys of the kodama-nats-auth Secret. Both values are written to caller-owned
// temporary files; nothing is printed, so the seed never reaches stdout, argv
// or a log.
//
// The encoding matches internal/shell/integration/nats/nkey.go: an account
// NKey prefix (0), the Ed25519 seed/public bytes, the XMODEM CRC16 checksum and
// unpadded base32. It also matches the reference nkeys implementation; the
// known-vector test pins that compatibility without importing the nkeys module
// as a new direct dependency.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	nkeyPrefixSeed    byte = 18 << 3
	nkeyPrefixAccount byte = 0
)

var nkeyBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "nats-auth-key: %v\n", err)
		os.Exit(1)
	}
}

// run parses the flags and writes one freshly generated account pair. It is a
// function so the file contract (mode, path and round trip) is testable.
func run(args []string) error {
	flags := flag.NewFlagSet("nats-auth-key", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	seedFile := flags.String("seed-file", "", "path to write the account seed (required)")
	publicFile := flags.String("public-file", "", "path to write the account public key (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *seedFile == "" || *publicFile == "" {
		return errors.New("-seed-file and -public-file are required")
	}
	if filepath.Clean(*seedFile) == filepath.Clean(*publicFile) {
		return errors.New("-seed-file and -public-file must be different paths")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	seedBytes := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seedBytes); err != nil {
		return fmt.Errorf("read entropy: %w", err)
	}
	seed, public, err := accountKeyPair(seedBytes)
	if err != nil {
		return err
	}
	if err := writePrivateFile(*seedFile, []byte(seed)); err != nil {
		return fmt.Errorf("write seed file: %w", err)
	}
	if err := writePrivateFile(*publicFile, []byte(public)); err != nil {
		return fmt.Errorf("write public file: %w", err)
	}
	return nil
}

// accountKeyPair encodes one account seed and its matching public key. The
// public half is derived from the seed, so the two files can never disagree.
func accountKeyPair(seed []byte) (string, string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", "", fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	private := ed25519.NewKeyFromSeed(seed)
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return "", "", errors.New("derived public key is unavailable")
	}
	return encodeNkeySeed(nkeyPrefixAccount, seed), encodeNkeyPublic(nkeyPrefixAccount, public), nil
}

// writePrivateFile creates or truncates a file with 0600 permissions. O_CREATE
// alone keeps an existing file's mode, so the explicit chmod also repairs a
// broader mode left by an interrupted earlier run.
func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// encodeNkeySeed encodes 32 seed bytes with the account public prefix.
func encodeNkeySeed(prefix byte, seed []byte) string {
	raw := make([]byte, 0, 2+len(seed)+2)
	raw = append(raw, nkeyPrefixSeed|(prefix>>5), (prefix&31)<<3)
	raw = append(raw, seed...)
	var checksum [2]byte
	binary.LittleEndian.PutUint16(checksum[:], crc16(raw))
	raw = append(raw, checksum[:]...)
	return nkeyBase32.EncodeToString(raw)
}

// encodeNkeyPublic encodes one public key with its NKey prefix and checksum.
func encodeNkeyPublic(prefix byte, public ed25519.PublicKey) string {
	raw := make([]byte, 0, 1+len(public)+2)
	raw = append(raw, prefix)
	raw = append(raw, public...)
	var checksum [2]byte
	binary.LittleEndian.PutUint16(checksum[:], crc16(raw))
	raw = append(raw, checksum[:]...)
	return nkeyBase32.EncodeToString(raw)
}

// crc16 is the XMODEM checksum NKeys append to every encoded key.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
