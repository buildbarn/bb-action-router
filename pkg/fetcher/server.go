package fetcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"golang.org/x/sync/singleflight"
)

// Default eviction tuning, see ServerOptions.
const (
	DefaultUseBoost    = 5 * time.Minute
	DefaultMaxUseBoost = 30 * time.Minute
)

// trashPrefix marks a directory as awaiting deletion, see discardLocked.
const trashPrefix = "trash-"

// We're using the unique address of a *int pointer as a lease token.
type leaseToken int

// MaterializedRoot is a docker image materialized into a local directory.
// A root is evictable iff its lease set is empty and its image isn't pinned.
type MaterializedRoot struct {
	Path   string
	leases map[*leaseToken]struct{}

	// virtualLastUse is a "last used" timestamp on a clock that runs ahead of
	// the real one: every acquire pushes it UseBoost further into the future,
	// capped at now+MaxUseBoost.
	virtualLastUse time.Time

	// idleSince is when this root last became evictable, i.e. when its final
	// lease was released.
	idleSince time.Time
}

// RootMaterializer pulls a docker image and materializes it into a directory.
type RootMaterializer interface {
	Materialize(ctx context.Context, imageRef string) (path string, err error)
}

// ServerOptions holds the tunables of a Server.
type ServerOptions struct {
	// MaxRoots is the maximum number of materialized roots to keep on disk.
	// Defaults to 1.
	MaxRoots int

	// MaxConcurrentFetches bounds the number of simultaneous
	// materializations. Defaults to 1.
	MaxConcurrentFetches int

	// AcquireTimeout bounds an entire Acquire call. Zero means no timeout.
	AcquireTimeout time.Duration

	// PrefetchImages are materialized by PrefetchImages() and pinned: they are
	// never evicted.
	PrefetchImages []string

	// UseBoost is how far into the future every acquire pushes a root's
	// virtualLastUse. Larger values make the cache more resistant to bursts of
	// one-shot images, at the cost of holding on to stale roots for longer.
	// Defaults to DefaultUseBoost.
	UseBoost time.Duration

	// MaxUseBoost caps virtualLastUse at now+MaxUseBoost, bounding how much
	// credit a hot root can accumulate. Setting it equal to UseBoost
	// degenerates the policy into plain LRU. Defaults to DefaultMaxUseBoost.
	MaxUseBoost time.Duration

	// Now returns the current time. Defaults to time.Now; overridden by tests.
	Now func() time.Time
}

// Server handles ACQUIRE/RELEASE requests.
type Server struct {
	mu       sync.Mutex
	roots    map[string]*MaterializedRoot
	trashSeq uint64

	materializeGroup singleflight.Group
	materializeSem   chan struct{}

	options ServerOptions

	// pinned holds the image references that are never evicted. Immutable
	// after construction, so it needs no locking.
	pinned map[string]struct{}

	reapers sync.WaitGroup

	Materializer RootMaterializer
	Metrics      *Metrics
}

// NewServer constructs a Server.
func NewServer(materializer RootMaterializer, metrics *Metrics, options ServerOptions) (*Server, error) {
	if options.MaxRoots < 1 {
		options.MaxRoots = 1
	}
	if options.MaxConcurrentFetches < 1 {
		options.MaxConcurrentFetches = 1
	}
	if options.UseBoost <= 0 {
		options.UseBoost = DefaultUseBoost
	}
	if options.MaxUseBoost <= 0 {
		options.MaxUseBoost = DefaultMaxUseBoost
	}
	if options.MaxUseBoost < options.UseBoost {
		options.MaxUseBoost = options.UseBoost
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	// Pinned roots take up their slot permanently, so leave at least half of
	// the cache to the images the runners actually ask for.
	pinned := make(map[string]struct{}, len(options.PrefetchImages))
	for _, image := range options.PrefetchImages {
		pinned[image] = struct{}{}
	}
	if 2*len(pinned) >= options.MaxRoots {
		return nil, fmt.Errorf("%d prefetched images would be pinned, which is more than half of the %d available root slots", len(pinned), options.MaxRoots)
	}

	return &Server{
		roots:          make(map[string]*MaterializedRoot),
		materializeSem: make(chan struct{}, options.MaxConcurrentFetches),
		options:        options,
		pinned:         pinned,
		Materializer:   materializer,
		Metrics:        metrics,
	}, nil
}

// Acquire returns a path to a materialized root for imageRef and a release
// func. The caller must invoke release() when done. Calling release() more
// than once is a no-op.
func (s *Server) Acquire(ctx context.Context, imageRef string) (string, func(), error) {
	if s.options.AcquireTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.options.AcquireTimeout)
		defer cancel()
	}

	token := new(leaseToken)

	if root := s.acquireCachedRoot(imageRef, token); root != nil {
		s.Metrics.AcquireTotal.Add(s.Metrics.Ctx, 1, metric.WithAttributes(attribute.String("status", "hit")))
		return root.Path, s.releaseFunc(root, token), nil
	}

	result, err, _ := s.materializeGroup.Do(imageRef, func() (interface{}, error) {
		return s.fetchRefSingle(ctx, imageRef, token)
	})
	if err != nil {
		s.Metrics.AcquireTotal.Add(s.Metrics.Ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return "", nil, err
	}

	root := result.(*MaterializedRoot)
	s.attachLease(root, token)
	s.Metrics.AcquireTotal.Add(s.Metrics.Ctx, 1, metric.WithAttributes(attribute.String("status", "miss")))
	return root.Path, s.releaseFunc(root, token), nil
}

func (s *Server) releaseFunc(root *MaterializedRoot, token *leaseToken) func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := root.leases[token]; !ok {
			return
		}
		delete(root.leases, token)
		if len(root.leases) == 0 {
			root.idleSince = s.options.Now()
		}
	}
}

// PrefetchImages materializes each of the configured prefetch images, see
// ServerOptions.PrefetchImages.
func (s *Server) PrefetchImages(ctx context.Context, perImageTimeout time.Duration) {
	for _, image := range s.options.PrefetchImages {
		log.Printf("Prefetching image %s", image)
		start := time.Now()

		callCtx := ctx
		var cancel context.CancelFunc
		if perImageTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, perImageTimeout)
		}
		_, release, err := s.Acquire(callCtx, image)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			log.Printf("Failed to prefetch image %s: %v", image, err)
			continue
		}
		release()
		log.Printf("Prefetched image %s in %.2fs", image, time.Since(start).Seconds())
	}
}

func (s *Server) acquireCachedRoot(imageRef string, token *leaseToken) *MaterializedRoot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if root, ok := s.roots[imageRef]; ok {
		root.leases[token] = struct{}{}
		s.touchLocked(root)
		return root
	}
	return nil
}

func (s *Server) attachLease(root *MaterializedRoot, token *leaseToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root.leases[token] = struct{}{}
	s.touchLocked(root)
}

// touchLocked records a use of root by pushing its virtualLastUse UseBoost
// further into the future, capped at now+MaxUseBoost. Caller must hold s.mu.
func (s *Server) touchLocked(root *MaterializedRoot) {
	now := s.options.Now()
	if root.virtualLastUse.Before(now) {
		// The real clock caught up: accumulated credit has decayed.
		root.virtualLastUse = now
	}
	root.virtualLastUse = root.virtualLastUse.Add(s.options.UseBoost)
	if ceiling := now.Add(s.options.MaxUseBoost); root.virtualLastUse.After(ceiling) {
		root.virtualLastUse = ceiling
	}
}

func (s *Server) fetchRefSingle(ctx context.Context, imageRef string, leaderToken *leaseToken) (*MaterializedRoot, error) {
	s.materializeSem <- struct{}{}
	defer func() { <-s.materializeSem }()

	log.Printf("Fetching image %s", imageRef)
	rootPath, err := s.Materializer.Materialize(ctx, imageRef)
	if err != nil {
		return nil, err
	}
	log.Printf("Materialized %s at %s", imageRef, rootPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	// This would require two concurrent Acquire calls and one of them to pause
	// before the materializeGroupCall for so long that the other one completes.
	// In this scenario we'd have two subsequent fetchRefSingle calls with the same ref.
	// It's very unlikely but also an easy check to add, so why not.
	if existing, ok := s.roots[imageRef]; ok {
		log.Printf("Discarding redundant materialization of %s at %s", imageRef, rootPath)
		if existing.Path != rootPath {
			s.discardLocked(rootPath)
		}
		return existing, nil
	}

	if len(s.roots) >= s.options.MaxRoots {
		if !s.evictOneIdleLocked() {
			s.discardLocked(rootPath)
			// Should only happen if MaxRoots < number of workers.
			return nil, fmt.Errorf("can't fetch %s, all %d slots are in use", imageRef, s.options.MaxRoots)
		}
	}

	now := s.options.Now()
	root := &MaterializedRoot{
		Path:           rootPath,
		leases:         map[*leaseToken]struct{}{leaderToken: {}},
		virtualLastUse: now,
		idleSince:      now,
	}
	s.roots[imageRef] = root
	s.Metrics.RootsGauge.Add(s.Metrics.Ctx, 1)
	return root, nil
}

// evictOneIdleLocked evicts the unpinned, unleased root with the smallest
// virtualLastUse and returns true, or returns false if there is none. Ties are
// broken on the image reference, so the choice is deterministic. Caller must
// hold s.mu.
func (s *Server) evictOneIdleLocked() bool {
	var victimRef string
	var victim *MaterializedRoot
	for ref, root := range s.roots {
		if len(root.leases) > 0 {
			continue
		}
		if _, ok := s.pinned[ref]; ok {
			continue
		}
		if victim == nil ||
			root.virtualLastUse.Before(victim.virtualLastUse) ||
			(root.virtualLastUse.Equal(victim.virtualLastUse) && ref < victimRef) {
			victimRef, victim = ref, root
		}
	}
	if victim == nil {
		return false
	}

	now := s.options.Now()
	idleFor := now.Sub(victim.idleSince)
	log.Printf("Evicting root %s (%s): idle for %s, %s of use credit left",
		victimRef, victim.Path, idleFor, victim.virtualLastUse.Sub(now))
	s.Metrics.EvictionIdleDuration.Record(s.Metrics.Ctx, idleFor.Seconds())

	delete(s.roots, victimRef)
	s.Metrics.RootsGauge.Add(s.Metrics.Ctx, -1)
	s.discardLocked(victim.Path)
	return true
}

// discardLocked renames path to a "trash-<seq>-<name>" sibling and deletes it on
// a background goroutine. This is done for speed and correctness (names are derived
// from the image digest).
//
// Caller must hold s.mu.
func (s *Server) discardLocked(path string) {
	s.trashSeq++
	dir, name := filepath.Split(path)
	target := filepath.Join(dir, fmt.Sprintf("%s%d-%s", trashPrefix, s.trashSeq, name))
	if err := os.Rename(path, target); err != nil {
		log.Printf("Failed to rename %s to %s: %v; deleting it in place", path, target, err)
		target = path
	}

	s.reapers.Add(1)
	go func() {
		defer s.reapers.Done()
		if err := os.RemoveAll(target); err != nil {
			log.Printf("Failed to remove %s: %v", target, err)
		}
	}()
}

// waitForEvictions blocks until every discarded root has been deleted from disk
// (for testing).
func (s *Server) waitForEvictions() {
	s.reapers.Wait()
}

// RootCount returns the number of cached roots (for testing).
func (s *Server) RootCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.roots)
}

// LeaseCount returns the number of live leases for a cached root, or -1 if
// not found (for testing).
func (s *Server) LeaseCount(imageRef string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if root, ok := s.roots[imageRef]; ok {
		return len(root.leases)
	}
	return -1
}
