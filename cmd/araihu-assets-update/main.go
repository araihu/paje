package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/araihu/paje/internal/araihuassets"
	"io"
	"os"
	"path/filepath"
)

func main() {
	repo := flag.String("repo", ".", "repository root")
	archive := flag.String("archive", "", "verified release archive")
	assetsRepository := flag.String("assets-repository", "", "replacement assets repository")
	assetsRevision := flag.String("assets-revision", "", "replacement assets revision")
	release := flag.String("release", "", "replacement release")
	releaseURL := flag.String("release-url", "", "replacement release URL")
	releaseSHA256 := flag.String("release-sha256", "", "replacement release archive SHA-256")
	releaseJSONSHA256 := flag.String("release-json-sha256", "", "replacement release.json SHA-256")
	flag.Parse()
	if *archive == "" {
		fmt.Fprintln(os.Stderr, "-archive required")
		os.Exit(2)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(*repo, araihuassets.DefaultManifestPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var manifest araihuassets.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	identity := &araihuassets.ReleaseIdentity{AssetsRepository: manifest.AssetsRepository, AssetsRevision: manifest.AssetsRevision, Release: manifest.Release, ReleaseURL: manifest.ReleaseURL, ReleaseSHA256: manifest.ReleaseSHA256, ReleaseJSONSHA256: manifest.ReleaseJSONSHA256}
	provided := []string{*assetsRepository, *assetsRevision, *release, *releaseURL, *releaseSHA256, *releaseJSONSHA256}
	count := 0
	for _, value := range provided {
		if value != "" {
			count++
		}
	}
	if count != 0 && count != len(provided) {
		fmt.Fprintln(os.Stderr, "all release identity flags required")
		os.Exit(2)
	}
	if count == len(provided) {
		*identity = araihuassets.ReleaseIdentity{AssetsRepository: *assetsRepository, AssetsRevision: *assetsRevision, Release: *release, ReleaseURL: *releaseURL, ReleaseSHA256: *releaseSHA256, ReleaseJSONSHA256: *releaseJSONSHA256}
	}
	if got, err := fileSHA256(*archive); err != nil || got != identity.ReleaseSHA256 {
		fmt.Fprintln(os.Stderr, "release archive SHA-256 mismatch")
		os.Exit(1)
	}
	d, err := os.MkdirTemp("", "paje-assets-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(d)
	if err = extract(*archive, d); err == nil {
		_, err = araihuassets.Update(araihuassets.Options{RepoRoot: *repo, ReleaseRoot: d, Identity: identity})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func extract(name, out string) error {
	f, e := os.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	g, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer g.Close()
	t := tar.NewReader(g)
	for {
		h, e := t.Next()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		p := filepath.Join(out, h.Name)
		if !filepath.IsLocal(h.Name) || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		w, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if e != nil {
			return e
		}
		_, e = io.Copy(w, t)
		if x := w.Close(); e == nil {
			e = x
		}
		if e != nil {
			return e
		}
	}
}
