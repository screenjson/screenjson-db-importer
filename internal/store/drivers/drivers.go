// Package drivers opens a storage driver by its configured name.
package drivers

import (
	"context"
	"fmt"

	"github.com/screenjson/screenjson-db-importer/internal/config"
	"github.com/screenjson/screenjson-db-importer/internal/store"
	"github.com/screenjson/screenjson-db-importer/internal/store/chroma"
	"github.com/screenjson/screenjson-db-importer/internal/store/elastic"
	"github.com/screenjson/screenjson-db-importer/internal/store/memory"
	"github.com/screenjson/screenjson-db-importer/internal/store/mongo"
	"github.com/screenjson/screenjson-db-importer/internal/store/pinecone"
	"github.com/screenjson/screenjson-db-importer/internal/store/postgres"
	"github.com/screenjson/screenjson-db-importer/internal/store/weaviate"
)

// Open connects the driver storage.driver names.
func Open(ctx context.Context, c *config.Config) (store.Driver, error) {
	s := c.Storage
	switch s.Driver {
	case "memory":
		return memory.New(), nil
	case "postgres":
		return postgres.Open(ctx, s.URL, c.TransactionsOn())
	case "mongo":
		return mongo.Open(ctx, s.URL, s.Database, s.EnvelopeField, c.TransactionsOn())
	case "elastic":
		return elastic.Open(ctx, s.URL, s.EnvelopeField, s.Elastic.Refresh)
	case "chroma":
		return chroma.Open(ctx, s.URL, s.Chroma.Tenant, s.Chroma.Database)
	case "weaviate":
		return weaviate.Open(ctx, s.URL, s.Weaviate.ClassPrefix)
	case "pinecone":
		return pinecone.Open(ctx, s.URL, s.Pinecone.APIKey, s.Pinecone.Namespace)
	}
	return nil, fmt.Errorf("storage: unknown driver %q", s.Driver)
}
