package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/config"
	"github.com/screenjson/screenjson-db-importer/internal/docs"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
	"github.com/screenjson/screenjson-db-importer/internal/state"
	"github.com/screenjson/screenjson-db-importer/internal/store"
	"github.com/screenjson/screenjson-db-importer/internal/store/drivers"
)

const leaseCollection = "screenjson_server_lease"
const leaseID = "00000000-0000-4000-8000-000000000001"
const leaseTTL = 30 * time.Second

type serverLease struct {
	Instance  string    `json:"instance"`
	Heartbeat time.Time `json:"heartbeat"`
}
type environment struct {
	Driver  store.Driver
	State   *state.Store
	Manager *docs.Manager
}

func openOffline(ctx context.Context, c *config.Config, log *slog.Logger) (*environment, error) {
	e := &environment{}
	st, err := state.Open(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w; stop the server before importing", err)
	}
	e.State = st
	ok := false
	defer func() {
		if !ok {
			e.Close()
		}
	}()
	l := c.Storage.Layout.Layout
	if err = l.Validate(); err != nil {
		return nil, fmt.Errorf("validate storage layout: %w", err)
	}
	fp := l.Fingerprint(c.Storage.EnvelopeField)
	stored, _, err := st.Layout()
	newLayout := errors.Is(err, state.ErrNotFound)
	if err != nil && !newLayout {
		return nil, fmt.Errorf("read stored layout: %w", err)
	}
	if !newLayout && stored != fp {
		return nil, fmt.Errorf("configured layout fingerprint %.12s differs from stored layout %.12s", fp, stored)
	}
	d, err := drivers.Open(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("connect storage driver %s: %w", c.Storage.Driver, err)
	}
	e.Driver = d
	if err = d.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping storage: %w", err)
	}
	cols := append(l.Collections(), store.CollectionSpec{Name: leaseCollection, Level: "system"})
	if err = d.Ensure(ctx, cols); err != nil {
		return nil, fmt.Errorf("prepare storage: %w", err)
	}
	recs, err := d.Get(ctx, leaseCollection, []string{leaseID})
	if err != nil {
		return nil, fmt.Errorf("read server lease: %w", err)
	}
	if len(recs) > 0 {
		var lease serverLease
		if json.Unmarshal(recs[0].Node, &lease) == nil && lease.Instance != "" && time.Since(lease.Heartbeat) < leaseTTL {
			return nil, fmt.Errorf("server instance %s has a fresh heartbeat; stop it before importing", lease.Instance)
		}
	}
	if newLayout {
		if err = st.SetLayout(fp, l.Normalise(c.Storage.EnvelopeField)); err != nil {
			return nil, fmt.Errorf("record storage layout: %w", err)
		}
	}
	raw := schema.Patched()
	set, err := schema.Compile(raw)
	if err != nil {
		return nil, err
	}
	order, err := schema.BuildOrder(raw)
	if err != nil {
		return nil, err
	}
	pt, err := paths.NewTable()
	if err != nil {
		return nil, err
	}
	mgr, err := docs.New(docs.Options{Driver: d, Layout: l, Fingerprint: fp, State: st, Schema: set, Order: order, Paths: pt,
		Log: log, MaxDocuments: c.Cache.MaxDocuments, MaxBytes: int64(c.Cache.MaxBytes), QueueLength: c.Queue.Length,
		DefaultTTL: c.Checkouts.DefaultTTL.D(), MaxTTL: c.Checkouts.MaxTTL.D(), Numbering: c.Numbering.Default,
		Transactions: c.TransactionsOn(), RequireIfMatch: c.HTTP.RequireIfMatch})
	if err != nil {
		return nil, err
	}
	e.Manager = mgr
	ok = true
	return e, nil
}

func (e *environment) Close() {
	if e.Manager != nil {
		e.Manager.Close()
	}
	if e.Driver != nil {
		_ = e.Driver.Close()
	}
	if e.State != nil {
		_ = e.State.Close()
	}
}
