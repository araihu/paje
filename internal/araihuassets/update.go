// Package araihuassets applies a verified, already extracted Arai Hu Assets
// release. Network access and archive extraction deliberately live outside it.
package araihuassets

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const DefaultManifestPath = "araihu-assets.json"

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var revisionRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var releaseRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type Manifest struct {
	SchemaVersion     int       `json:"schemaVersion"`
	AssetsRepository  string    `json:"assetsRepository"`
	AssetsRevision    string    `json:"assetsRevision"`
	Release           string    `json:"release"`
	ReleaseURL        string    `json:"releaseUrl"`
	ReleaseSHA256     string    `json:"releaseSha256"`
	ReleaseJSONSHA256 string    `json:"releaseJsonSha256"`
	Mappings          []Mapping `json:"mappings"`
}
type Mapping struct {
	Source        string `json:"source"`
	Destination   string `json:"destination"`
	CanonicalName string `json:"canonicalName"`
	Namespace     string `json:"namespace"`
	Product       string `json:"product"`
	Artwork       string `json:"artwork"`
	Appearance    string `json:"appearance"`
	Surface       string `json:"surface"`
	Framing       string `json:"framing"`
	Format        string `json:"format"`
}
type ReleaseIdentity struct{ AssetsRepository, AssetsRevision, Release, ReleaseURL, ReleaseSHA256, ReleaseJSONSHA256 string }
type Options struct {
	RepoRoot, ReleaseRoot, ManifestPath string
	Identity                            *ReleaseIdentity
	BeforeReplace                       func(int, string) error
}
type Result struct{ Changed []string }
type ApplyError struct {
	FailedPath                   string
	AppliedPaths, RollbackErrors []string
	Cause                        error
}

func (e *ApplyError) Error() string          { return fmt.Sprintf("replace %q: %v", e.FailedPath, e.Cause) }
func (e *ApplyError) Unwrap() error          { return e.Cause }
func (e *ApplyError) RollbackComplete() bool { return len(e.RollbackErrors) == 0 }

type releaseFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type releaseDocument struct {
	SchemaVersion int           `json:"schemaVersion"`
	Release       string        `json:"release"`
	CatalogSHA256 string        `json:"catalogSha256"`
	Files         []releaseFile `json:"files"`
}
type catalog struct {
	Release string         `json:"release"`
	Assets  []catalogAsset `json:"assets"`
}
type catalogAsset struct {
	CanonicalName string `json:"canonicalName"`
	Path          string `json:"path"`
	Namespace     string `json:"namespace"`
	Product       string `json:"product"`
	Artwork       string `json:"artwork"`
	Appearance    string `json:"appearance"`
	Surface       string `json:"surface"`
	Framing       string `json:"framing"`
	Format        string `json:"format"`
	SHA256        string `json:"sha256"`
}
type write struct {
	path      string
	next, old []byte
	existed   bool
	staged    string
}

// Update verifies release.json, catalog roles and every selected file before
// staging replacements. Fallbacks replace first; manifest identity commits last.
func Update(o Options) (Result, error) {
	if err := rootOK(o.RepoRoot); err != nil {
		return Result{}, err
	}
	if err := rootOK(o.ReleaseRoot); err != nil {
		return Result{}, err
	}
	manifestPath := o.ManifestPath
	if manifestPath == "" {
		manifestPath = DefaultManifestPath
	}
	if err := safe(manifestPath); err != nil {
		return Result{}, err
	}
	mb, err := readRegular(o.RepoRoot, manifestPath)
	if err != nil {
		return Result{}, err
	}
	var m Manifest
	if err = json.Unmarshal(mb, &m); err != nil {
		return Result{}, err
	}
	if o.Identity != nil {
		m.AssetsRepository = o.Identity.AssetsRepository
		m.AssetsRevision = o.Identity.AssetsRevision
		m.Release = o.Identity.Release
		m.ReleaseURL = o.Identity.ReleaseURL
		m.ReleaseSHA256 = o.Identity.ReleaseSHA256
		m.ReleaseJSONSHA256 = o.Identity.ReleaseJSONSHA256
	}
	if err = validate(m); err != nil {
		return Result{}, err
	}
	rb, err := readRegular(o.ReleaseRoot, "release.json")
	if err != nil {
		return Result{}, err
	}
	if hash(rb) != m.ReleaseJSONSHA256 {
		return Result{}, fmt.Errorf("release.json SHA-256 mismatch")
	}
	var r releaseDocument
	if err = json.Unmarshal(rb, &r); err != nil {
		return Result{}, err
	}
	if r.SchemaVersion != 1 || r.Release != m.Release {
		return Result{}, errors.New("release.json release mismatch")
	}
	inv := map[string]releaseFile{}
	for _, f := range r.Files {
		if err := safe(f.Path); err != nil {
			return Result{}, err
		}
		if !sha256RE.MatchString(f.SHA256) || f.Size < 0 {
			return Result{}, errors.New("invalid release file")
		}
		if _, ok := inv[f.Path]; ok {
			return Result{}, errors.New("release file collision")
		}
		inv[f.Path] = f
	}
	cb, err := readRegular(o.ReleaseRoot, "catalog.json")
	if err != nil {
		return Result{}, err
	}
	if hash(cb) != r.CatalogSHA256 {
		return Result{}, errors.New("catalog.json SHA-256 mismatch")
	}
	var c catalog
	if err = json.Unmarshal(cb, &c); err != nil {
		return Result{}, err
	}
	if c.Release != m.Release {
		return Result{}, errors.New("catalog release mismatch")
	}
	writes := []write{}
	for _, x := range m.Mappings {
		f, ok := inv[x.Source]
		if !ok {
			return Result{}, fmt.Errorf("source %q missing", x.Source)
		}
		if x.CanonicalName != "" {
			if err := catalogOK(c, x, f); err != nil {
				return Result{}, err
			}
		}
		b, e := readRegular(o.ReleaseRoot, x.Source)
		if e != nil {
			return Result{}, e
		}
		if int64(len(b)) != f.Size || hash(b) != f.SHA256 {
			return Result{}, fmt.Errorf("source %q SHA-256 mismatch", x.Source)
		}
		old, e := readMaybe(o.RepoRoot, x.Destination)
		if e != nil {
			return Result{}, e
		}
		if !bytes.Equal(old, b) {
			writes = append(writes, write{path: x.Destination, next: b, old: old, existed: old != nil})
		}
	}
	next, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Result{}, err
	}
	next = append(next, '\n')
	if !bytes.Equal(mb, next) {
		writes = append(writes, write{path: manifestPath, next: next, old: mb, existed: true})
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].path < writes[j].path })
	for i := range writes {
		if writes[i].path == manifestPath {
			writes = append(append(writes[:i], writes[i+1:]...), writes[i])
			break
		}
	}
	for i := range writes {
		if err := stage(o.RepoRoot, &writes[i]); err != nil {
			return Result{}, err
		}
	}
	applied := []string{}
	for i := range writes {
		if o.BeforeReplace != nil {
			if err := o.BeforeReplace(i, writes[i].path); err != nil {
				return Result{}, rollback(o.RepoRoot, writes, i, applied, err)
			}
		}
		if err := os.Rename(writes[i].staged, filepath.Join(o.RepoRoot, writes[i].path)); err != nil {
			return Result{}, rollback(o.RepoRoot, writes, i, applied, err)
		}
		applied = append(applied, writes[i].path)
	}
	return Result{Changed: applied}, nil
}
func validate(m Manifest) error {
	if m.SchemaVersion != 1 || m.AssetsRepository != "araihu/assets" || !revisionRE.MatchString(m.AssetsRevision) || !releaseRE.MatchString(m.Release) || !sha256RE.MatchString(m.ReleaseSHA256) || !sha256RE.MatchString(m.ReleaseJSONSHA256) {
		return errors.New("invalid manifest identity")
	}
	want := fmt.Sprintf("https://github.com/araihu/assets/releases/download/%s/araihu-assets-%s.tar.gz", m.Release, m.Release)
	if m.ReleaseURL != want {
		return errors.New("invalid release URL")
	}
	seen := map[string]bool{}
	for _, x := range m.Mappings {
		if err := safe(x.Source); err != nil {
			return err
		}
		if err := safe(x.Destination); err != nil {
			return err
		}
		k := strings.ToLower(x.Destination)
		if seen[k] {
			return errors.New("destination collision")
		}
		seen[k] = true
		roles := []string{x.Namespace, x.Product, x.Artwork, x.Appearance, x.Surface, x.Framing, x.Format}
		if x.CanonicalName == "" {
			for _, role := range roles {
				if role != "" {
					return errors.New("catalog roles require canonical name")
				}
			}
			continue
		}
		if x.Namespace == "" || x.Product == "" || x.Artwork == "" || x.Appearance == "" || x.Surface == "" || x.Framing == "" || x.Format == "" {
			return errors.New("incomplete catalog mapping")
		}
	}
	return nil
}
func catalogOK(c catalog, m Mapping, f releaseFile) error {
	for _, a := range c.Assets {
		if a.CanonicalName == m.CanonicalName {
			if a.Path != m.Source || a.Namespace != m.Namespace || a.Product != m.Product || a.Artwork != m.Artwork || a.Appearance != m.Appearance || a.Surface != m.Surface || a.Framing != m.Framing || a.Format != m.Format || a.SHA256 != f.SHA256 {
				return fmt.Errorf("catalog role mismatch for %q", m.CanonicalName)
			}
			return nil
		}
	}
	return fmt.Errorf("catalog canonicalName %q not found", m.CanonicalName)
}
func safe(p string) error {
	if p == "" || filepath.IsAbs(p) || filepath.Clean(p) != p || strings.Contains(p, "\\") || p == "." || strings.HasPrefix(p, "..") {
		return fmt.Errorf("unsafe path %q", p)
	}
	return nil
}
func rootOK(root string) error {
	i, e := os.Lstat(root)
	if e != nil {
		return e
	}
	if i.Mode()&os.ModeSymlink != 0 || !i.IsDir() {
		return errors.New("root must be non-symlink directory")
	}
	return nil
}
func readRegular(root, p string) ([]byte, error) {
	if err := components(root, p); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, p))
}
func readMaybe(root, p string) ([]byte, error) {
	if err := components(root, p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, p))
}
func components(root, p string) error {
	if err := safe(p); err != nil {
		return err
	}
	cur := root
	parts := strings.Split(p, string(filepath.Separator))
	for _, x := range parts {
		cur = filepath.Join(cur, x)
		i, e := os.Lstat(cur)
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symbolic link", x)
		}
	}
	return nil
}
func stage(root string, w *write) error {
	dst := filepath.Join(root, w.path)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	r := make([]byte, 8)
	if _, err := rand.Read(r); err != nil {
		return err
	}
	w.staged = dst + fmt.Sprintf(".araihu-assets-%x.tmp", r)
	f, err := os.OpenFile(w.staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	if _, err = f.Write(w.next); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}
func rollback(root string, w []write, failed int, applied []string, cause error) error {
	e := &ApplyError{FailedPath: w[failed].path, AppliedPaths: applied, Cause: cause}
	for i := failed - 1; i >= 0; i-- {
		dst := filepath.Join(root, w[i].path)
		if w[i].existed {
			v := write{path: w[i].path, next: w[i].old}
			if x := stage(root, &v); x != nil {
				e.RollbackErrors = append(e.RollbackErrors, x.Error())
			} else if x := os.Rename(v.staged, dst); x != nil {
				e.RollbackErrors = append(e.RollbackErrors, x.Error())
			}
		} else if x := os.Remove(dst); x != nil && !errors.Is(x, os.ErrNotExist) {
			e.RollbackErrors = append(e.RollbackErrors, x.Error())
		}
	}
	for i := failed; i < len(w); i++ {
		_ = os.Remove(w[i].staged)
	}
	return e
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

var _ = io.EOF
