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

// idleTTL is how long a directory's cache is kept without use. It bounds the
// map by live use rather than by session history: under skills.scope =
// "workspace" every session root gets an entry, and a daemon that has
// served a thousand roots should not still hold a thousand caches.
const idleTTL = time.Hour

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

// cacheEntry pairs a directory's cache with the last time it was asked for.
// The timestamp is what the sweep reads, so it is touched on every hit, not
// only on insert -- a directory in steady use must never look idle.
type cacheEntry struct {
	cache *Cache
	used  time.Time
}

// Caches holds one Cache per directory. Under skills.scope = "workspace"
// every session root has its own skills directory, so a single cache for the
// process would have N sessions in N directories fighting over one set.
//
// Entries idle beyond idleTTL are swept on the next miss, so the map is
// bounded by the roots in live use rather than by every root the daemon has
// ever served. internal/workspace's Describers bounds itself the same way.
type Caches struct {
	mu  sync.Mutex
	m   map[string]*cacheEntry
	now func() time.Time
}

func NewCaches() *Caches {
	return &Caches{m: map[string]*cacheEntry{}, now: time.Now}
}

func (cs *Caches) cache(dir string) *Cache {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := cs.now()
	if e, ok := cs.m[dir]; ok {
		e.used = now
		return e.cache
	}

	// Miss: sweep entries idle beyond the TTL before inserting the new one.
	// Evicting an entry another goroutine is already holding is harmless --
	// it keeps its *Cache and finishes, and the next caller simply reloads.
	for k, e := range cs.m {
		if now.Sub(e.used) > idleTTL {
			delete(cs.m, k)
		}
	}

	c := NewCache(dir)
	cs.m[dir] = &cacheEntry{cache: c, used: now}
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
