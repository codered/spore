package skill

import (
	"errors"
	"sync"
	"time"
)

// ttl bounds how stale a cached set may be. A skill written by hand appears
// in the index within this window without a restart; a skill written through
// skill_install appears immediately, because the tool invalidates the entry.
const ttl = 30 * time.Second

// Cache holds the loaded skills for one directory. Assembly runs on every
// turn and a directory scan per turn buys nothing, so the set is reloaded on
// the TTL and after a write.
type Cache struct {
	dir    string
	mu     sync.RWMutex
	skills []Skill
	errs   []error
	loaded time.Time
}

func NewCache(dir string) *Cache { return &Cache{dir: dir} }

func (c *Cache) Dir() string { return c.dir }

// Reload rereads the directory and returns the per-file errors so the caller
// can warn about them. On a directory-level failure the cache preserves its
// existing skills and returns the error: a transient permission problem or an
// unmounted volume must not silently blank the set. Per-file errors follow
// the degradation rule -- one broken file costs one skill, never the whole
// set.
func (c *Cache) Reload() []error {
	skills, errs := Load(c.dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	var dirErr bool
	for _, e := range errs {
		if errors.Is(e, ErrReadDir) {
			dirErr = true
			break
		}
	}
	if !dirErr {
		c.skills = skills
	}
	c.errs = errs
	c.loaded = time.Now()
	return errs
}

// Skills returns a copy: a caller that trims for a token budget must not be
// able to edit the shared set.
func (c *Cache) Skills() []Skill {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Skill, len(c.skills))
	copy(out, c.skills)
	return out
}

// Errors returns the per-file errors from the last reload. /skills reports
// them, so a skill with broken frontmatter is visible rather than merely
// absent from the index.
func (c *Cache) Errors() []error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]error, len(c.errs))
	copy(out, c.errs)
	return out
}

func (c *Cache) fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.loaded.IsZero() && time.Since(c.loaded) < ttl
}

// Caches holds one Cache per directory. Under skills.scope = "workspace"
// every session root has its own skills directory, so a single cache for the
// process would have N sessions in N directories fighting over one set.
//
// Entries are never evicted. Under the default global scope there is exactly
// one, so this costs nothing; under workspace scope it grows with the number
// of distinct session roots the daemon has ever seen, which is bounded by
// session history rather than by live sessions. internal/workspace's
// Describers had the same shape and now sweeps idle roots on a TTL; this has
// not been given the same treatment yet, and docs/backlog.md carries it.
type Caches struct {
	mu sync.Mutex
	m  map[string]*Cache
}

func NewCaches() *Caches { return &Caches{m: map[string]*Cache{}} }

func (cs *Caches) cache(dir string) *Cache {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.m[dir]
	if !ok {
		c = NewCache(dir)
		cs.m[dir] = c
	}
	return c
}

// Skills returns the skills in dir, reloading when the cached set is stale.
// An empty dir means skills are switched off for this session -- a workspace
// scope with no session root of its own -- and is not an error.
func (cs *Caches) Skills(dir string) []Skill {
	if dir == "" {
		return nil
	}
	c := cs.cache(dir)
	if !c.fresh() {
		c.Reload()
	}
	return c.Skills()
}

// Errors returns the per-file errors from dir's last load.
func (cs *Caches) Errors(dir string) []error {
	if dir == "" {
		return nil
	}
	c := cs.cache(dir)
	if !c.fresh() {
		c.Reload()
	}
	return c.Errors()
}

// Invalidate rereads dir now. skill_install calls it, which is the only way
// the set changes from inside spore; a hand-edited directory waits for the
// TTL instead.
func (cs *Caches) Invalidate(dir string) {
	if dir == "" {
		return
	}
	cs.cache(dir).Reload()
}
