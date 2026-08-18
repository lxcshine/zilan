package container

import (
	"context"
	"log"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ResourceCleaner is a resource cleaner that can be used to clean up resources
type ResourceCleaner struct {
	mu       sync.Mutex
	cleanups []types.CleanupFunc
}

// NewResourceCleaner creates a new resource cleaner
func NewResourceCleaner() interfaces.ResourceCleaner {
	return &ResourceCleaner{
		cleanups: make([]types.CleanupFunc, 0),
	}
}

// Register registers a cleanup function
// Note: the cleanup function will be executed in reverse order (the last registered will be executed first)
func (c *ResourceCleaner) Register(cleanup types.CleanupFunc) {
	if cleanup == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanups = append(c.cleanups, cleanup)
}

// RegisterWithName registers a cleanup function with a name, for logging tracking
func (c *ResourceCleaner) RegisterWithName(name string, cleanup types.CleanupFunc) {
	if cleanup == nil {
		return
	}

	wrappedCleanup := func() error {
		log.Printf("Cleaning up resource: %s", name)
		err := cleanup()
		if err != nil {
			log.Printf("Error cleaning up resource %s: %v", name, err)
		} else {
			log.Printf("Successfully cleaned up resource: %s", name)
		}
		return err
	}

	c.Register(wrappedCleanup)
}

// Cleanup executes all cleanup functions
// Even if a cleanup function fails, other cleanup functions will still be executed
func (c *ResourceCleaner) Cleanup(ctx context.Context) (errs []error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Execute cleanup functions in reverse order (the last registered will be executed first)
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		select {
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return errs
		default:
			if err := c.cleanups[i](); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errs
}

// Reset clears all registered cleanup functions
func (c *ResourceCleaner) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanups = make([]types.CleanupFunc, 0)
}
