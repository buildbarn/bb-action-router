package fetcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMaterializer struct {
	mu       sync.Mutex
	calls    int
	delay    time.Duration
	failNext bool
}

func newTestMetrics() *Metrics {
	m, _, _ := newMetrics("test")
	return m
}

func newTestServer(mat *fakeMaterializer, maxRoots, maxConcurrent int) *Server {
	return NewServer(mat, newTestMetrics(), maxRoots, maxConcurrent, 0)
}

func (f *fakeMaterializer) Materialize(ctx context.Context, imageRef string) (string, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failNext
	f.failNext = false
	f.mu.Unlock()

	if fail {
		return "", fmt.Errorf("injected failure")
	}

	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	dir, err := os.MkdirTemp("", "root-*")
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dir, "marker"), []byte(imageRef), 0o644)
	return dir, nil
}

func (f *fakeMaterializer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestCacheHit(t *testing.T) {
	mat := &fakeMaterializer{}
	s := newTestServer(mat, 10, 1)

	p1, _, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(p1)

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
	mat := &fakeMaterializer{}
	s := newTestServer(mat, 10, 1)

	path, _, err := s.Acquire(context.Background(), "image-a")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(path)

	data, err := os.ReadFile(filepath.Join(path, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "image-a" {
		t.Errorf("expected marker 'image-a', got %q", string(data))
	}
}

func TestSingleflight(t *testing.T) {
	mat := &fakeMaterializer{delay: 100 * time.Millisecond}
	s := newTestServer(mat, 10, 2)

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
	defer os.RemoveAll(paths[0])

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
	mat := &fakeMaterializer{}
	s := newTestServer(mat, 2, 1)

	pa, ra, _ := s.Acquire(context.Background(), "image-a")
	defer os.RemoveAll(pa)
	pb, rb, _ := s.Acquire(context.Background(), "image-b")
	defer os.RemoveAll(pb)

	ra()
	rb()

	pc, _, _ := s.Acquire(context.Background(), "image-c")
	defer os.RemoveAll(pc)

	if s.RootCount() != 2 {
		t.Errorf("expected 2 roots after eviction, got %d", s.RootCount())
	}
	if s.LeaseCount("image-c") != 1 {
		t.Errorf("expected image-c lease count 1, got %d", s.LeaseCount("image-c"))
	}
}

func TestEvictionSkipsInUseRoots(t *testing.T) {
	mat := &fakeMaterializer{}
	s := newTestServer(mat, 2, 1)

	pa, _, _ := s.Acquire(context.Background(), "image-a")
	defer os.RemoveAll(pa)
	pb, rb, _ := s.Acquire(context.Background(), "image-b")
	defer os.RemoveAll(pb)

	// Release only image-b, keep image-a in use.
	rb()

	pc, _, _ := s.Acquire(context.Background(), "image-c")
	defer os.RemoveAll(pc)

	// image-b should have been evicted, image-a kept.
	if s.LeaseCount("image-a") != 1 {
		t.Errorf("in-use image-a should survive eviction, lease count=%d", s.LeaseCount("image-a"))
	}
	if s.LeaseCount("image-b") != -1 {
		t.Errorf("idle image-b should have been evicted, lease count=%d", s.LeaseCount("image-b"))
	}
}

func TestRelease(t *testing.T) {
	mat := &fakeMaterializer{}
	s := newTestServer(mat, 10, 1)

	path, release, _ := s.Acquire(context.Background(), "image-a")
	defer os.RemoveAll(path)

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

func TestAcquireTimeout(t *testing.T) {
	mat := &fakeMaterializer{delay: 5 * time.Second}
	s := NewServer(mat, newTestMetrics(), 10, 1, 50*time.Millisecond)

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

	mat := &fakeMaterializer{delay: 100 * time.Millisecond}
	origMat := mat

	wrapper := &concurrencyTrackingMaterializer{
		inner:      origMat,
		running:    &running,
		maxRunning: &maxRunning,
	}

	s := NewServer(wrapper, newTestMetrics(), 10, 1, 0)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path, _, err := s.Acquire(context.Background(), fmt.Sprintf("image-%d", idx))
			if err != nil {
				t.Errorf("acquire %d: %v", idx, err)
				return
			}
			defer os.RemoveAll(path)
		}(i)
	}
	wg.Wait()

	if maxRunning.Load() > 1 {
		t.Errorf("expected max 1 concurrent materialization, got %d", maxRunning.Load())
	}
}

func TestMaterializeError(t *testing.T) {
	mat := &fakeMaterializer{failNext: true}
	s := newTestServer(mat, 10, 1)

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
