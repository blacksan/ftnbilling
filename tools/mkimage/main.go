// Command mkimage builds a docker-save style image tarball (the format that
// `docker save` writes and RouterOS `/container add file=` imports) from a
// static binary, without needing a Docker daemon. Standard library only.
//
//	mkimage -bin build/ispbill -out dist/ispbill-arm64.tar [-arch arm64] [-ca /etc/ssl/certs/ca-certificates.crt]
//	mkimage -verify dist/ispbill-arm64.tar
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fixed so repeated builds of the same inputs give identical output.
var mtime = time.Unix(1767225600, 0).UTC() // 2026-01-01T00:00:00Z

func main() {
	bin := flag.String("bin", "build/ispbill", "static binary to package as /ispbill")
	ca := flag.String("ca", "/etc/ssl/certs/ca-certificates.crt", "CA bundle to include")
	excl := flag.String("exclude", "/usr/local/share/ca-certificates/*.crt", "glob of locally installed CA files to REMOVE from the bundle (compared by DER bytes)")
	out := flag.String("out", "dist/ispbill-arm64.tar", "output tarball")
	arch := flag.String("arch", "arm64", "image architecture")
	tag := flag.String("tag", "ispbill:latest", "repo:tag")
	verify := flag.String("verify", "", "verify an existing tarball and exit")
	flag.Parse()
	var err error
	if *verify != "" {
		err = verifyImage(*verify)
	} else {
		err = build(*bin, *ca, *excl, *out, *arch, *tag)
		if err == nil {
			err = verifyImage(*out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func layerTar(binPath string, caData []byte) ([]byte, error) {
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := func(name string, mode int64) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode, ModTime: mtime, Format: tar.FormatPAX})
	}
	file := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: mtime, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	steps := []func() error{
		func() error { return dir("data/", 0o755) },
		func() error { return dir("etc/", 0o755) },
		func() error { return dir("etc/ssl/", 0o755) },
		func() error { return dir("etc/ssl/certs/", 0o755) },
		func() error { return file("etc/ssl/certs/ca-certificates.crt", 0o644, caData) },
		func() error { return file("ispbill", 0o755, binData) },
		func() error { return dir("tmp/", 0o1777) },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func build(binPath, caPath, exclGlob, outPath, arch, tag string) error {
	bundle, removed, err := cleanBundle(caPath, exclGlob)
	if err != nil {
		return err
	}
	fmt.Printf("CA bundle: removed %d locally installed certificate(s) from %s\n", removed, caPath)
	layer, err := layerTar(binPath, bundle)
	if err != nil {
		return err
	}
	diffID := sum(layer)
	created := mtime.Format(time.RFC3339)
	cfgObj := map[string]any{
		"architecture": arch,
		"os":           "linux",
		"created":      created,
		"author":       "ispbill mkimage",
		"config": map[string]any{
			"Env": []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"DATA_DIR=/data", "LISTEN_ADDR=:8080",
			},
			"Entrypoint":   []string{"/ispbill"},
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"WorkingDir":   "/",
			"Labels":       map[string]string{"org.opencontainers.image.title": "ispbill"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + diffID}},
		"history": []map[string]any{{
			"created": created, "created_by": "mkimage: COPY ispbill, CA bundle, /data, /tmp", "comment": "ispbill",
		}},
	}
	cfg, err := json.Marshal(cfgObj) // map keys are sorted: deterministic
	if err != nil {
		return err
	}
	cfgName := sum(cfg) + ".json"
	layerDir := diffID // legacy layer id; any unique hex name works
	layerName := layerDir + "/layer.tar"

	manifest, _ := json.Marshal([]map[string]any{{"Config": cfgName, "RepoTags": []string{tag}, "Layers": []string{layerName}}})
	repo, ver, _ := strings.Cut(tag, ":")
	repositories, _ := json.Marshal(map[string]map[string]string{repo: {ver: layerDir}})
	legacy, _ := json.Marshal(map[string]any{"id": layerDir, "created": created, "architecture": arch, "os": "linux", "config": cfgObj["config"]})

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	put := func(name string, data []byte, isDir bool) error {
		h := &tar.Header{Name: name, Mode: 0o644, ModTime: mtime, Format: tar.FormatPAX}
		if isDir {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		} else {
			h.Typeflag, h.Size = tar.TypeReg, int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	for _, e := range []struct {
		name string
		data []byte
		dir  bool
	}{
		{layerDir + "/", nil, true},
		{layerDir + "/VERSION", []byte("1.0"), false},
		{layerDir + "/json", legacy, false},
		{layerName, layer, false},
		{cfgName, cfg, false},
		{"manifest.json", manifest, false},
		{"repositories", repositories, false},
	} {
		if err := put(e.name, e.data, e.dir); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// verifyImage reads the tarball back, lists members and checks every digest.
func verifyImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	files := map[string][]byte{}
	tr := tar.NewReader(f)
	fmt.Println("image members:")
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
		fmt.Printf("  %-90s %10d  %04o\n", h.Name, h.Size, h.Mode&0o7777)
	}
	var mf []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil || len(mf) != 1 {
		return fmt.Errorf("bad manifest.json: %v", err)
	}
	m := mf[0]
	cfgBytes, ok := files[m.Config]
	if !ok {
		return fmt.Errorf("config %s missing", m.Config)
	}
	if sum(cfgBytes)+".json" != m.Config {
		return fmt.Errorf("config digest mismatch")
	}
	var cfg struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct{ Entrypoint []string }
		Rootfs       struct {
			Type    string
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	json.Unmarshal(cfgBytes, &cfg)
	if len(m.Layers) != 1 || len(cfg.Rootfs.DiffIDs) != 1 {
		return fmt.Errorf("expected exactly one layer")
	}
	layer, ok := files[m.Layers[0]]
	if !ok {
		return fmt.Errorf("layer %s missing", m.Layers[0])
	}
	if want := "sha256:" + sum(layer); cfg.Rootfs.DiffIDs[0] != want {
		return fmt.Errorf("diff_id %s != layer digest %s", cfg.Rootfs.DiffIDs[0], want)
	}
	fmt.Println("layer members:")
	ltr := tar.NewReader(bytes.NewReader(layer))
	seen := map[string]bool{}
	for {
		h, err := ltr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		seen[h.Name] = true
		fmt.Printf("  %-40s %10d  %04o\n", h.Name, h.Size, h.Mode&0o7777)
		if h.Name == "etc/ssl/certs/ca-certificates.crt" {
			data, _ := io.ReadAll(ltr)
			n, err := checkBundle(data)
			if err != nil {
				return err
			}
			if n < 50 {
				return fmt.Errorf("CA bundle has only %d certificates", n)
			}
			fmt.Printf("  (CA bundle: %d certificates, none forbidden)\n", n)
		}
		if h.Name == "ispbill" && h.Mode&0o777 != 0o755 {
			return fmt.Errorf("/ispbill mode %o", h.Mode)
		}
		if h.Name == "tmp/" && h.Mode&0o7777 != 0o1777 {
			return fmt.Errorf("/tmp mode %o", h.Mode)
		}
	}
	for _, need := range []string{"ispbill", "etc/ssl/certs/ca-certificates.crt", "data/", "tmp/"} {
		if !seen[need] {
			return fmt.Errorf("layer lacks %s", need)
		}
	}
	if _, ok := files["repositories"]; !ok {
		return fmt.Errorf("repositories file missing")
	}
	fmt.Printf("OK: arch=%s os=%s entrypoint=%v tags=%v\n  config sha256 %s\n  layer diff_id %s\n",
		cfg.Architecture, cfg.OS, cfg.Config.Entrypoint, m.RepoTags, strings.TrimSuffix(m.Config, ".json"), cfg.Rootfs.DiffIDs[0])
	return nil
}

// forbiddenCA reports certificates that must never ship in the image
// (sandbox / interception CAs).
func forbiddenCA(c *x509.Certificate) string {
	for _, o := range c.Subject.Organization {
		if strings.Contains(strings.ToLower(o), "anthropic") {
			return "O=" + o
		}
	}
	cn := strings.ToLower(c.Subject.CommonName)
	for _, bad := range []string{"agent-proxy", "egress-gateway"} {
		if strings.Contains(cn, bad) {
			return "CN=" + c.Subject.CommonName
		}
	}
	return ""
}

// checkBundle parses every certificate in a PEM bundle and fails if any is forbidden.
func checkBundle(pemData []byte) (n int, err error) {
	for {
		var b *pem.Block
		b, pemData = pem.Decode(pemData)
		if b == nil {
			return n, nil
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		n++
		c, perr := x509.ParseCertificate(b.Bytes)
		if perr != nil {
			continue // some legacy roots do not parse in Go; they cannot match by name
		}
		if why := forbiddenCA(c); why != "" {
			return n, fmt.Errorf("forbidden CA in bundle (%s)", why)
		}
	}
}

// cleanBundle returns the system bundle minus every certificate that also
// appears (same DER bytes) in the locally installed CA files, then fails
// loudly if anything forbidden is left.
func cleanBundle(caPath, exclGlob string) ([]byte, int, error) {
	local := map[string]bool{}
	files, _ := filepath.Glob(exclGlob)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, 0, err
		}
		for {
			var blk *pem.Block
			blk, b = pem.Decode(b)
			if blk == nil {
				break
			}
			if blk.Type == "CERTIFICATE" {
				local[string(blk.Bytes)] = true
			}
		}
	}
	src, err := os.ReadFile(caPath)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	removed := 0
	for rest := src; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" && local[string(blk.Bytes)] {
			removed++
			continue
		}
		if err := pem.Encode(&out, blk); err != nil {
			return nil, 0, err
		}
	}
	if _, err := checkBundle(out.Bytes()); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), removed, nil
}
