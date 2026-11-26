package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Ranks order the bootstrap parts (design docs/design/agentic-parity.md,
// section 2.9). Lower ranks run first. Parts of equal rank run in the order
// they were first set.
const (
	// RankCompat: the test-only Safari 16.6 approximation that deletes newer
	// APIs before the runtime installs (design 8.2).
	RankCompat  = 10
	RankRuntime = 20 // agent runtime (W3a)
	RankDialog  = 30 // dialog override (W3b)
	RankShim    = 40 // geolocation and other shims (W4)
	RankInit    = 50 // user init scripts (W4)
)

// RuntimePart is the name and owner of the agent runtime's bootstrap part
// (design 3.1 install path 2). It belongs to the page, not to a session,
// so a release keeps it.
const RuntimePart = "runtime"

// BootstrapParts joins named parts into the one script that
// Page.setBootstrapScript allows per session. Each part is wrapped in
// try/catch, so a part that throws does not stop the parts after it. A part
// with a syntax error still breaks the whole script; parts must parse.
type BootstrapParts struct {
	mu    sync.Mutex
	parts map[string]bootPart
	seq   int
}

type bootPart struct {
	rank   int
	seq    int
	source string
}

// NewBootstrapParts returns an empty set of parts.
func NewBootstrapParts() *BootstrapParts {
	return &BootstrapParts{parts: make(map[string]bootPart)}
}

// Set adds or replaces the part name. A replaced part keeps its position.
func (b *BootstrapParts) Set(name string, rank int, source string) error {
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("bootstrap part name %q: must be one non-empty line", name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.parts[name]
	if !ok || p.rank != rank {
		b.seq++
		p.seq = b.seq
	}
	p.rank, p.source = rank, source
	b.parts[name] = p
	return nil
}

// SetLatest adds or replaces the part name as the last of its rank: a
// replaced part moves to the end of the script order (last set wins).
func (b *BootstrapParts) SetLatest(name string, rank int, source string) error {
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("bootstrap part name %q: must be one non-empty line", name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.parts[name] = bootPart{rank: rank, seq: b.seq, source: source}
	return nil
}

// Remove deletes the part name and reports whether it existed.
func (b *BootstrapParts) Remove(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.parts[name]
	delete(b.parts, name)
	return ok
}

// Source returns the source of the part name and whether it exists.
func (b *BootstrapParts) Source(name string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.parts[name]
	return p.source, ok
}

// Empty reports whether there are no parts.
func (b *BootstrapParts) Empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.parts) == 0
}

// Names returns the part names in script order.
func (b *BootstrapParts) Names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.orderedLocked()
}

func (b *BootstrapParts) orderedLocked() []string {
	names := make([]string, 0, len(b.parts))
	for n := range b.parts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := b.parts[names[i]], b.parts[names[j]]
		if pi.rank != pj.rank {
			return pi.rank < pj.rank
		}
		return pi.seq < pj.seq
	})
	return names
}

// Script returns the joined bootstrap script, or "" when there are no parts.
func (b *BootstrapParts) Script() string { return b.ScriptWithout("") }

// ScriptWithout is Script without the part name, which stays in place: a
// caller sends it first and removes the part only when WebKit took it, so
// a refused send leaves the order unchanged.
func (b *BootstrapParts) ScriptWithout(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var sb strings.Builder
	for _, n := range b.orderedLocked() {
		if n == name {
			continue
		}
		fmt.Fprintf(&sb, "// iwdp bootstrap part: %s\ntry {\n%s\n} catch (e) {}\n", n, b.parts[n].source)
	}
	return sb.String()
}
