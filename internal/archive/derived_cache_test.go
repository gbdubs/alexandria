package archive

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDerivedValuesAreComputedOncePerCommit(t *testing.T) {
	catalog, _ := libraryFixture(t)
	other := otherProcess(t, catalog)
	var calls atomic.Int32
	release := make(chan struct{})
	compute := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, ctx.Err()
	}
	// The request that starts the computation gives up; the computation does
	// not, since other requests are waiting for it.
	abandoned, cancel := context.WithCancel(context.Background())
	go func() { _, _ = cachedValue(abandoned, catalog, "answer", compute) }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var waiting sync.WaitGroup
	values := make([]int, 5)
	for index := range values {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			value, err := cachedValue(context.Background(), catalog, "answer", compute)
			if err != nil {
				t.Error(err)
			}
			values[index] = value
		}()
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
	close(release)
	waiting.Wait()
	for _, value := range values {
		if value != 42 {
			t.Fatalf("values = %v", values)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("computed %d times for one commit", calls.Load())
	}
	if value, _ := cachedValue(context.Background(), catalog, "answer", compute); value != 42 || calls.Load() != 1 {
		t.Fatal("an unchanged catalog recomputed")
	}
	if _, err := other.DB.Exec(`UPDATE workspaces SET title='Renamed' WHERE id='c'`); err != nil {
		t.Fatal(err)
	}
	if value, _ := cachedValue(context.Background(), catalog, "answer", compute); value != 42 || calls.Load() != 2 {
		t.Fatalf("a commit did not recompute: %d computations", calls.Load())
	}
}
