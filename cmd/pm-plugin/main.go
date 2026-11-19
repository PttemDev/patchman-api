// pm-plugin signs PatchMan plugin packages (PLUGIN_MARKETPLACE_RFC §18).
//
//	pm-plugin keygen -out mykey            writes mykey.key (private) and mykey.pub
//	pm-plugin sign   -key mykey.key DIR    writes DIR/SIGNATURE.json
//	pm-plugin verify -pub mykey.pub DIR    checks DIR against SIGNATURE.json
//
// sign covers manifest.json, backend.wasm and, when the manifest has a custom
// UI (ui.entry), every file under ui/. Keep the .key file secret; share the
// .pub file (its content is what users add as a trusted publisher).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"patchman-be/internal/plugins/manifest"
	"patchman-be/internal/plugins/pkgsig"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pm-plugin:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pm-plugin keygen -out NAME | sign -key NAME.key DIR | verify -pub NAME.pub DIR")
	os.Exit(2)
}

func keygen(args []string) error {
	fl := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fl.String("out", "", "file name prefix (writes PREFIX.key and PREFIX.pub)")
	fl.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if _, err := os.Stat(*out + ".key"); err == nil {
		return fmt.Errorf("%s.key exists; refusing to overwrite a key", *out)
	}
	if err := os.WriteFile(*out+".key", []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*out+".pub", []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("key id %s\nprivate key: %s.key (keep it secret)\npublic key:  %s.pub\n", pkgsig.KeyID(pub), *out, *out)
	return nil
}

// packageFiles collects the files that belong to the package in dir.
func packageFiles(dir string) (*manifest.Manifest, map[string][]byte, error) {
	files := map[string][]byte{}
	for _, f := range []string{"manifest.json", "backend.wasm"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, nil, err
		}
		files[f] = b
	}
	m, err := manifest.Parse(files["manifest.json"])
	if err != nil {
		return nil, nil, err
	}
	if m.UI != nil && m.UI.Entry != "" {
		root := filepath.Join(dir, "ui")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if !d.Type().IsRegular() || !manifest.UIPathOK(rel) {
				return fmt.Errorf("ui/%s: not allowed in a package (regular files with allowed types only)", rel)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files["ui/"+rel] = b
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if _, ok := files[m.UI.Entry]; !ok {
			return nil, nil, fmt.Errorf("ui.entry %s not found", m.UI.Entry)
		}
	}
	return m, files, nil
}

func sign(args []string) error {
	fl := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fl.String("key", "", "private key file from keygen")
	fl.Parse(args)
	if *keyFile == "" || fl.NArg() != 1 {
		return errors.New("usage: sign -key NAME.key DIR")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	priv, err := pkgsig.ParsePrivateKey(string(raw))
	if err != nil {
		return err
	}
	dir := fl.Arg(0)
	m, files, err := packageFiles(dir)
	if err != nil {
		return err
	}
	sig := pkgsig.Sign(priv, m.ID, m.Version, files)
	out, _ := json.MarshalIndent(sig, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, pkgsig.FileName), append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s@%s (%d files) with key %s\n", m.ID, m.Version, len(files), sig.KeyID)
	return nil
}

func verify(args []string) error {
	fl := flag.NewFlagSet("verify", flag.ExitOnError)
	pubFile := fl.String("pub", "", "public key file")
	fl.Parse(args)
	if *pubFile == "" || fl.NArg() != 1 {
		return errors.New("usage: verify -pub NAME.pub DIR")
	}
	raw, err := os.ReadFile(*pubFile)
	if err != nil {
		return err
	}
	pub, err := pkgsig.ParsePublicKey(string(raw))
	if err != nil {
		return err
	}
	dir := fl.Arg(0)
	sraw, err := os.ReadFile(filepath.Join(dir, pkgsig.FileName))
	if err != nil {
		return err
	}
	sig, err := pkgsig.Parse(sraw)
	if err != nil {
		return err
	}
	if _, err := sig.Verify([]pkgsig.TrustedKey{{KeyID: pkgsig.KeyID(pub), Name: "key", Key: pub}}); err != nil {
		return err
	}
	for p := range sig.Files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return err
		}
		if err := sig.CheckFile(p, b); err != nil {
			return err
		}
	}
	fmt.Printf("ok: %s@%s, %d files, signed by %s\n", sig.ID, sig.Version, len(sig.Files), strings.ToLower(sig.KeyID))
	return nil
}
