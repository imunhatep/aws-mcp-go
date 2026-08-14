package mcpserver

import (
	"context"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/cache"
	"github.com/imunhatep/awslib/cache/handlers"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// DefaultCacheTTL is the default time-to-live for cached AWS resource listings.
const DefaultCacheTTL = 6 * time.Hour

// NewCache builds a DataCache backed by an in-memory (bigcache) handler and,
// when cacheDir is non-empty, a file handler. Both handlers use the same ttl so
// cached resource listings expire consistently. The per-account/region cache
// namespace is applied automatically by each repository's WithCache.
func NewCache(ctx context.Context, ttl time.Duration, cacheDir string) (*cache.DataCache, error) {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}

	bigCache, err := bigcache.New(ctx, bigcache.DefaultConfig(ttl))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	dataCache := cache.NewDataCache().WithHandlers(handlers.NewInMemory(bigCache))

	if cacheDir != "" {
		inFile, err := handlers.NewInFile(cacheDir, ttl)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		dataCache = dataCache.WithHandlers(inFile)
	}

	log.Info().
		Dur("ttl", ttl).
		Str("cacheDir", cacheDir).
		Msg("[mcpserver.NewCache] resource cache initialised")

	return dataCache, nil
}
