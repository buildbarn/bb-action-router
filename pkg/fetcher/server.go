package fetcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"golang.org/x/sync/singleflight"
)

// We're using the unique address of a *int pointer as a lease token.
type leaseToken int

// MaterializedRoot is a docker image materialized into a local directory.
// A root is evictable iff its lease set is empty.
type MaterializedRoot struct {
	Path   string
	leases map[*leaseToken]struct{}
}

// RootMaterializer pulls a docker image and materializes it into a directory.
type RootMaterializer interface {
	Materialize(ctx context.Context, imageRef string) (path string, err error)
}

// Server handles ACQUIRE/RELEASE requests.
type Server struct {
	mu    sync.Mutex
	roots map[string]*MaterializedRoot

	materializeGroup singleflight.Group
	materializeSem   chan struct{}

	MaxRoots       int
	AcquireTimeout time.Duration

	Materializer RootMaterializer
	Metrics      *Metrics
}

// NewServer constructs a Server.
func NewServer(materializer RootMaterializer, metrics *Metrics, maxRoots, maxConcurrentFetches int, acquireTimeout time.Duration) *Server {
	return &Server{
		roots:          make(map[string]*MaterializedRoot),
		materializeSem: make(chan struct{}, maxConcurrentFetches),
		MaxRoots:       maxRoots,
		AcquireTimeout: acquireTimeout,
		Materializer:   materializer,
		Metrics:        metrics,
	}
}

// Acquire returns a path to a materialized root for imageRef and a release
// func. The caller must invoke release() when done. Calling release() more
// than once is a no-op.
func (s *Server) Acquire(ctx context.Context, imageRef string) (string, func(), error) {
	if s.AcquireTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.AcquireTimeout)
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
		delete(root.leases, token)
	}
}

// PrefetchImages materializes each image in the list.
func (s *Server) PrefetchImages(ctx context.Context, images []string, perImageTimeout time.Duration) {
	for _, image := range images {
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
		return root
	}
	return nil
}

func (s *Server) attachLease(root *MaterializedRoot, token *leaseToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root.leases[token] = struct{}{}
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
		os.RemoveAll(rootPath)
		return existing, nil
	}

	if len(s.roots) >= s.MaxRoots {
		if !s.evictOneIdleLocked() {
			os.RemoveAll(rootPath)
			// Should only happen if MaxRoots < number of workers.
			return nil, fmt.Errorf("can't fetch %s, all %d slots are in use", imageRef, s.MaxRoots)
		}
	}

	root := &MaterializedRoot{
		Path:   rootPath,
		leases: map[*leaseToken]struct{}{leaderToken: {}},
	}
	s.roots[imageRef] = root
	s.Metrics.RootsGauge.Add(s.Metrics.Ctx, 1)
	return root, nil
}

// evictOneIdleLocked removes one cached root with no live leases and returns
// true. Caller must hold s.mu.
func (s *Server) evictOneIdleLocked() bool {
	for ref, root := range s.roots {
		if len(root.leases) == 0 {
			log.Printf("Evicting unused root %s: %s", ref, root.Path)
			os.RemoveAll(root.Path)
			delete(s.roots, ref)
			s.Metrics.RootsGauge.Add(s.Metrics.Ctx, -1)
			return true
		}
	}
	return false
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
