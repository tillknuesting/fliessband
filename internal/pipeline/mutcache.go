package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tillknuesting/fliessband/internal/mutate"
)

// mutantCache stores mutant outcomes across runs, keyed by everything a
// mutant's test run depends on: the files of its package and of every
// in-module package its test binary links, the versions of outside modules,
// go.mod and go.sum, the go test flags and the timeout. A rerun of an
// unchanged spec, or a version whose LLM code changed but whose template
// packages did not, skips most mutants. Any change to those inputs is a miss.
type mutantCache struct {
	dir  string
	salt string // flags, timeout and module files shared by every key

	mu      sync.Mutex
	pkgKeys map[string]string                   // package dir -> key
	entries map[string]map[string]mutate.Status // package key -> mutant -> status
	dirty   map[string]bool
	hits    int
	stored  int
}

// cacheDir is where mutant outcomes are kept: FLIESSBAND_CACHE, else the
// user's cache directory, else the data directory.
func (m *Manager) cacheDir() string {
	if d := os.Getenv("FLIESSBAND_CACHE"); d != "" {
		return filepath.Join(d, "mutants")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "fliessband", "mutants")
	}
	return filepath.Join(m.dataDir, "cache", "mutants")
}

// newMutantCache computes a key for each package that has mutants. It
// returns nil (no cache) when the package graph cannot be listed.
func (x *executor) newMutantCache(ctx context.Context, ms []mutate.Mutant, args []string, timeout time.Duration) *mutantCache {
	root := x.r.ServiceDir()
	mod, err1 := os.ReadFile(filepath.Join(root, "go.mod"))
	sum, _ := os.ReadFile(filepath.Join(root, "go.sum"))
	if err1 != nil {
		return nil
	}
	salt := sha256.New()
	_, _ = fmt.Fprintf(salt, "v1\n%s\n%s\n", strings.Join(args, " "), timeout)
	salt.Write(mod)
	salt.Write(sum)
	c := &mutantCache{dir: x.m.cacheDir(), salt: hex.EncodeToString(salt.Sum(nil)), pkgKeys: map[string]string{},
		entries: map[string]map[string]mutate.Status{}, dirty: map[string]bool{}}
	dirs := map[string]bool{}
	for _, m := range ms {
		dirs[filepath.ToSlash(filepath.Dir(m.File))] = true
	}
	for d := range dirs {
		key, err := packageKey(ctx, x.m.childEnv(false), root, d, c.salt)
		if err != nil {
			return nil
		}
		c.pkgKeys[d] = key
	}
	return c
}

// packageKey hashes what the test binary of the package in dir is built from.
func packageKey(ctx context.Context, env []string, root, dir, salt string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-test", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{if .Module.Main}}@main{{end}}{{end}}\t{{.Standard}}", "./"+dir)
	cmd.Dir, cmd.Env = root, env
	isolate(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n%s\n", salt, dir)
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	slices.Sort(lines)
	seen := map[string]bool{}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) < 4 || f[3] == "true" {
			continue // the standard library comes with the toolchain, pinned in go.mod
		}
		if !strings.HasSuffix(f[2], "@main") {
			_, _ = fmt.Fprintf(h, "dep %s %s\n", f[0], f[2]) // an outside module: its version pins its content
			continue
		}
		if seen[f[1]] {
			continue
		}
		seen[f[1]] = true
		if err := hashTree(h, root, f[1]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree hashes the files under dir (embedded files such as SQL
// migrations included), skipping hidden and underscore directories.
func hashTree(h io.Writer, root, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // files of the run's own service
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		sum := sha256.Sum256(b)
		_, _ = fmt.Fprintf(h, "file %s %x\n", filepath.ToSlash(rel), sum)
		return nil
	})
}

func mutantID(m mutate.Mutant) string {
	return fmt.Sprintf("%s:%d:%d:%s:%q:%q", m.File, m.Line, m.Col, m.Operator, m.Original, m.Mutated)
}

func (c *mutantCache) load(key string) map[string]mutate.Status {
	if e, ok := c.entries[key]; ok {
		return e
	}
	e := map[string]mutate.Status{}
	if b, err := os.ReadFile(filepath.Join(c.dir, key+".json")); err == nil {
		_ = json.Unmarshal(b, &e)
	}
	c.entries[key] = e
	return e
}

// Get implements mutate.Cache.
func (c *mutantCache) Get(m mutate.Mutant) (mutate.Status, bool) {
	key, ok := c.pkgKeys[filepath.ToSlash(filepath.Dir(m.File))]
	if !ok {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.load(key)[mutantID(m)]
	if ok {
		c.hits++
	}
	return st, ok
}

// Put implements mutate.Cache.
func (c *mutantCache) Put(m mutate.Mutant, st mutate.Status) {
	key, ok := c.pkgKeys[filepath.ToSlash(filepath.Dir(m.File))]
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load(key)[mutantID(m)] = st
	c.dirty[key] = true
	c.stored++
}

// flush writes the changed packages' entries. Errors only lose the cache.
func (c *mutantCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirty) == 0 {
		return
	}
	if os.MkdirAll(c.dir, 0o750) != nil {
		return
	}
	for key := range c.dirty {
		b, err := json.Marshal(c.entries[key])
		if err != nil {
			continue
		}
		tmp := filepath.Join(c.dir, key+".tmp")
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, filepath.Join(c.dir, key+".json"))
		}
	}
	c.dirty = map[string]bool{}
}
