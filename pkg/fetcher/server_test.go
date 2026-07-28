package fetcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMaterializer struct {
	rootsDir string

	mu       sync.Mutex
	calls    int
	delay    time.Duration
	failNext bool
}

func newFakeMaterializer(t *testing.T) *fakeMaterializer {
	return &fakeMaterializer{rootsDir: t.TempDir()}
}

func newTestMetrics() *Metrics {
	m, _, _ := newMetrics("test")
	return m
}

// newTestServer builds a Server on top of mat. Unset options keep their
// NewServer defaults.
func newTestServer(t *testing.T, mat *fakeMaterializer, options ServerOptions) *Server {
	t.Helper()
	s, err := NewServer(mat, newTestMetrics(), options)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Let the background deletions finish before the temporary directories are
	// torn down under them.
	t.Cleanup(s.waitForEvictions)
	return s
}

// fakeClock is a manually advanced clock, to exercise the eviction policy
// without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (f *fakeMaterializer) Materialize(ctx context.Context, imageRef string) (string, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failNext
	f.failNext = false
	delay := f.delay
	f.mu.Unlock()

	if fail {
		return "", fmt.Errorf("injected failure")
	}

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Like the real materializer, the path is derived from the image ref, so
	// re-materializing an image reuses the same directory.
	dir := filepath.Join(f.rootsDir, imageRef)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(imageRef), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func (f *fakeMaterializer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// acquireAndRelease leaves imageRef cached but evictable.
func acquireAndRelease(t *testing.T, s *Server, imageRef string) {
	t.Helper()
	_, release, err := s.Acquire(context.Background(), imageRef)
	if err != nil {
		t.Fatalf("acquire %s: %v", imageRef, err)
	}
	release()
}

func rootFieldsOf(t *testing.T, s *Server, imageRef string) *MaterializedRoot {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	root, ok := s.roots[imageRef]
	if !ok {
		t.Fatalf("%s is not cached", imageRef)
	}
	return root
}

// trashEntries returns the names of the leftover "trash-" directories in dir.
func trashEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), trashPrefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestCacheHit(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10})

	p1, _, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}

	p2, _, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}

	if p1 != p2 {
		t.Errorf("expected same path, got %s and %s", p1, p2)
	}
	if mat.callCount() != 1 {
		t.Errorf("expected 1 materialize call, got %d", mat.callCount())
	}
	if lc := s.LeaseCount("image-a"); lc != 2 {
		t.Errorf("expected lease count 2, got %d", lc)
	}
}

func TestCacheMiss(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10})

	path, _, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(path, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "image-a" {
		t.Errorf("expected marker 'image-a', got %q", string(data))
	}
}

func TestSingleflight(t *testing.T) {
	mat := newFakeMaterializer(t)
	mat.delay = 100 * time.Millisecond
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10, MaxConcurrentFetches: 2})

	var wg sync.WaitGroup
	paths := make([]string, 5)
	errs := make([]error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			paths[idx], _, errs[idx] = s.Acquire(context.Background(), "image-a")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	for i := 1; i < 5; i++ {
		if paths[i] != paths[0] {
			t.Errorf("goroutine %d got different path: %s vs %s", i, paths[i], paths[0])
		}
	}
	if mat.callCount() != 1 {
		t.Errorf("expected 1 materialize call, got %d", mat.callCount())
	}
	if lc := s.LeaseCount("image-a"); lc != 5 {
		t.Errorf("expected lease count 5, got %d", lc)
	}
}

func TestEviction(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 2})

	_, ra, _ := s.Acquire(context.Background(), "image-a")
	_, rb, _ := s.Acquire(context.Background(), "image-b")

	ra()
	rb()

	if _, _, err := s.Acquire(context.Background(), "image-c"); err != nil {
		t.Fatal(err)
	}

	if s.RootCount() != 2 {
		t.Errorf("expected 2 roots after eviction, got %d", s.RootCount())
	}
	if s.LeaseCount("image-c") != 1 {
		t.Errorf("expected image-c lease count 1, got %d", s.LeaseCount("image-c"))
	}
}

func TestEvictionSkipsInUseRoots(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 2})

	if _, _, err := s.Acquire(context.Background(), "image-a"); err != nil {
		t.Fatal(err)
	}
	_, rb, err := s.Acquire(context.Background(), "image-b")
	if err != nil {
		t.Fatal(err)
	}

	// Release only image-b, keep image-a in use.
	rb()

	if _, _, err := s.Acquire(context.Background(), "image-c"); err != nil {
		t.Fatal(err)
	}

	// image-b should have been evicted, image-a kept.
	if s.LeaseCount("image-a") != 1 {
		t.Errorf("in-use image-a should survive eviction, lease count=%d", s.LeaseCount("image-a"))
	}
	if s.LeaseCount("image-b") != -1 {
		t.Errorf("idle image-b should have been evicted, lease count=%d", s.LeaseCount("image-b"))
	}
}

// A repeatedly acquired root must outrank a one-shot image, so that a burst of
// single-use images cannot flush the hot set.
func TestEvictionPrefersLeastUsedRoot(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{
		MaxRoots:    2,
		UseBoost:    5 * time.Minute,
		MaxUseBoost: 30 * time.Minute,
		Now:         clock.Now,
	})

	acquireAndRelease(t, s, "hot")
	acquireAndRelease(t, s, "hot")
	acquireAndRelease(t, s, "cold")

	if _, _, err := s.Acquire(context.Background(), "new"); err != nil {
		t.Fatal(err)
	}

	if s.LeaseCount("hot") != 0 {
		t.Errorf("twice-used hot root should survive eviction, lease count=%d", s.LeaseCount("hot"))
	}
	if s.LeaseCount("cold") != -1 {
		t.Errorf("once-used cold root should have been evicted, lease count=%d", s.LeaseCount("cold"))
	}
}

// Use credit has to decay, otherwise a root that was hot hours ago keeps its
// slot forever.
func TestEvictionCreditDecays(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{
		MaxRoots:    2,
		UseBoost:    5 * time.Minute,
		MaxUseBoost: 30 * time.Minute,
		Now:         clock.Now,
	})

	// "formerly-hot" ends up with 15 minutes of credit.
	for i := 0; i < 3; i++ {
		acquireAndRelease(t, s, "formerly-hot")
	}

	// By the time "recent" is acquired, that credit has run out.
	clock.advance(20 * time.Minute)
	acquireAndRelease(t, s, "recent")

	if _, _, err := s.Acquire(context.Background(), "new"); err != nil {
		t.Fatal(err)
	}

	if s.LeaseCount("formerly-hot") != -1 {
		t.Errorf("stale root should have been evicted, lease count=%d", s.LeaseCount("formerly-hot"))
	}
	if s.LeaseCount("recent") != 0 {
		t.Errorf("recently used root should survive eviction, lease count=%d", s.LeaseCount("recent"))
	}
}

func TestMaxUseBoostCapsCredit(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{
		MaxRoots:    10,
		UseBoost:    5 * time.Minute,
		MaxUseBoost: 10 * time.Minute,
		Now:         clock.Now,
	})

	for i := 0; i < 20; i++ {
		acquireAndRelease(t, s, "image-a")
	}

	want := clock.Now().Add(10 * time.Minute)
	if got := rootFieldsOf(t, s, "image-a").virtualLastUse; !got.Equal(want) {
		t.Errorf("expected use credit to be capped at %s, got %s", want, got)
	}
}

// A MaxUseBoost equal to UseBoost means no root can build up an advantage.
func TestUseBoostEqualToMaxUseBoostIsLRU(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{
		MaxRoots:    2,
		UseBoost:    5 * time.Minute,
		MaxUseBoost: 5 * time.Minute,
		Now:         clock.Now,
	})

	acquireAndRelease(t, s, "many-uses")
	acquireAndRelease(t, s, "many-uses")
	acquireAndRelease(t, s, "many-uses")
	clock.advance(time.Minute)
	acquireAndRelease(t, s, "one-use")

	if _, _, err := s.Acquire(context.Background(), "new"); err != nil {
		t.Fatal(err)
	}

	if s.LeaseCount("many-uses") != -1 {
		t.Errorf("least recently used root should have been evicted, lease count=%d", s.LeaseCount("many-uses"))
	}
	if s.LeaseCount("one-use") != 0 {
		t.Errorf("most recently used root should survive eviction, lease count=%d", s.LeaseCount("one-use"))
	}
}

func TestPrefetchedRootsArePinned(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{
		MaxRoots:       3,
		PrefetchImages: []string{"pinned"},
		Now:            clock.Now,
	})

	s.PrefetchImages(context.Background(), 0)
	if s.LeaseCount("pinned") != 0 {
		t.Fatalf("expected prefetched root to be cached and idle, lease count=%d", s.LeaseCount("pinned"))
	}

	// The pinned root stays the least recently used one throughout, so plain
	// LRU would drop it first.
	for i := 0; i < 5; i++ {
		clock.advance(time.Hour)
		acquireAndRelease(t, s, fmt.Sprintf("image-%d", i))
	}

	if s.LeaseCount("pinned") != 0 {
		t.Errorf("pinned root should never be evicted, lease count=%d", s.LeaseCount("pinned"))
	}
	if s.RootCount() != 3 {
		t.Errorf("expected 3 roots, got %d", s.RootCount())
	}
}

func TestPinningMoreThanHalfTheCacheIsRejected(t *testing.T) {
	for _, test := range []struct {
		name           string
		maxRoots       int
		prefetchImages []string
		wantErr        bool
	}{
		{name: "no prefetch", maxRoots: 1, wantErr: false},
		{name: "quarter of the cache", maxRoots: 4, prefetchImages: []string{"a"}, wantErr: false},
		{name: "duplicates are counted once", maxRoots: 3, prefetchImages: []string{"a", "a"}, wantErr: false},
		{name: "exactly half the cache", maxRoots: 2, prefetchImages: []string{"a"}, wantErr: true},
		{name: "the whole cache", maxRoots: 2, prefetchImages: []string{"a", "b"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewServer(newFakeMaterializer(t), newTestMetrics(), ServerOptions{
				MaxRoots:       test.maxRoots,
				PrefetchImages: test.prefetchImages,
			})
			if test.wantErr && err == nil {
				t.Error("expected NewServer to reject the configuration")
			}
			if !test.wantErr && err != nil {
				t.Errorf("expected NewServer to accept the configuration, got: %v", err)
			}
		})
	}
}

func TestEvictionDeletesRootDirectory(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 1})

	path, release, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}
	release()

	if _, _, err := s.Acquire(context.Background(), "image-b"); err != nil {
		t.Fatal(err)
	}

	// The rename happens synchronously, so the path is immediately free for a
	// re-materialization of image-a.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone right after eviction, stat returned %v", path, err)
	}

	s.waitForEvictions()
	if leftovers := trashEntries(t, mat.rootsDir); len(leftovers) != 0 {
		t.Errorf("expected the evicted root to be deleted, found %v", leftovers)
	}
}

func TestAcquireFailsWhenAllSlotsAreInUse(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 1})

	if _, _, err := s.Acquire(context.Background(), "image-a"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Acquire(context.Background(), "image-b"); err == nil {
		t.Fatal("expected acquire to fail with all slots in use")
	}
	if s.RootCount() != 1 {
		t.Errorf("expected 1 root, got %d", s.RootCount())
	}

	// The materialization that could not be cached must not be left behind.
	s.waitForEvictions()
	if _, err := os.Stat(filepath.Join(mat.rootsDir, "image-b")); !os.IsNotExist(err) {
		t.Errorf("expected the discarded root to be deleted, stat returned %v", err)
	}
	if leftovers := trashEntries(t, mat.rootsDir); len(leftovers) != 0 {
		t.Errorf("expected no leftover trash directories, found %v", leftovers)
	}
}

func TestRelease(t *testing.T) {
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10})

	_, release, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}

	if lc := s.LeaseCount("image-a"); lc != 1 {
		t.Fatalf("expected lease count 1, got %d", lc)
	}
	release()
	if lc := s.LeaseCount("image-a"); lc != 0 {
		t.Errorf("expected lease count 0 after release, got %d", lc)
	}
	// Root should still be cached.
	if s.RootCount() != 1 {
		t.Errorf("root should remain cached after release, got %d roots", s.RootCount())
	}
}

// A second release() must not restart the root's idle clock, or
// EvictionIdleDuration would under-report.
func TestRepeatedReleaseKeepsIdleSince(t *testing.T) {
	clock := newFakeClock()
	mat := newFakeMaterializer(t)
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10, Now: clock.Now})

	_, release, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	release()

	want := clock.Now()
	clock.advance(time.Hour)
	release()

	if got := rootFieldsOf(t, s, "image-a").idleSince; !got.Equal(want) {
		t.Errorf("expected idleSince to stay at %s, got %s", want, got)
	}
}

func TestAcquireTimeout(t *testing.T) {
	mat := newFakeMaterializer(t)
	mat.delay = 5 * time.Second
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10, AcquireTimeout: 50 * time.Millisecond})

	_, _, err := s.Acquire(context.Background(), "image-a")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !os.IsTimeout(err) && err != context.DeadlineExceeded {
		t.Errorf("expected deadline exceeded, got: %v", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	var running atomic.Int32
	var maxRunning atomic.Int32

	mat := newFakeMaterializer(t)
	mat.delay = 100 * time.Millisecond

	wrapper := &concurrencyTrackingMaterializer{
		inner:      mat,
		running:    &running,
		maxRunning: &maxRunning,
	}

	s, err := NewServer(wrapper, newTestMetrics(), ServerOptions{MaxRoots: 10})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if _, _, err := s.Acquire(context.Background(), fmt.Sprintf("image-%d", idx)); err != nil {
				t.Errorf("acquire %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if maxRunning.Load() > 1 {
		t.Errorf("expected max 1 concurrent materialization, got %d", maxRunning.Load())
	}
}

func TestMaterializeError(t *testing.T) {
	mat := newFakeMaterializer(t)
	mat.failNext = true
	s := newTestServer(t, mat, ServerOptions{MaxRoots: 10})

	_, _, err := s.Acquire(context.Background(), "image-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if s.RootCount() != 0 {
		t.Errorf("failed materialize should not cache, got %d roots", s.RootCount())
	}
}

type concurrencyTrackingMaterializer struct {
	inner      RootMaterializer
	running    *atomic.Int32
	maxRunning *atomic.Int32
}

func (c *concurrencyTrackingMaterializer) Materialize(ctx context.Context, imageRef string) (string, error) {
	n := c.running.Add(1)
	for {
		old := c.maxRunning.Load()
		if n <= old || c.maxRunning.CompareAndSwap(old, n) {
			break
		}
	}
	defer c.running.Add(-1)
	return c.inner.Materialize(ctx, imageRef)
}
