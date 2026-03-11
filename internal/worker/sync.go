package worker

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// SyncManager orchestrates full and incremental syncs for connectors.
type SyncManager struct {
	connectors []kb.Connector
	store      kb.Storage
	logger     *slog.Logger
}

// NewSyncManager creates a sync manager.
func NewSyncManager(store kb.Storage, logger *slog.Logger) *SyncManager {
	return &SyncManager{
		store:  store,
		logger: logger.With("component", "sync-manager"),
	}
}

// AddConnector registers a connector for sync management.
func (sm *SyncManager) AddConnector(c kb.Connector) {
	sm.connectors = append(sm.connectors, c)
}

// SyncAll runs sync on all registered connectors.
func (sm *SyncManager) SyncAll() error {
	var firstErr error
	for _, c := range sm.connectors {
		sm.logger.Info("syncing source", "source", c.Name())
		if err := c.Sync(); err != nil {
			sm.logger.Warn("sync failed", "source", c.Name(), "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("sync %s: %w", c.Name(), err)
			}
		}
	}
	return firstErr
}

// SyncSource runs sync on a specific source.
func (sm *SyncManager) SyncSource(source kb.Source) error {
	for _, c := range sm.connectors {
		if c.Name() == source {
			return c.Sync()
		}
	}
	return fmt.Errorf("unknown source: %s", source)
}

// FullSync resets sync state and performs a complete backfill.
func (sm *SyncManager) FullSync(source kb.Source) error {
	// Reset sync state so connectors fetch everything
	resetState := kb.SyncState{
		Source:     source,
		LastSyncAt: time.Time{},
		Cursor:     "",
		Metadata:   map[string]string{"full_sync": "true"},
	}
	if err := sm.store.SetSyncState(resetState); err != nil {
		return fmt.Errorf("reset sync state for %s: %w", source, err)
	}

	return sm.SyncSource(source)
}

// GetConnectors returns all registered connectors.
func (sm *SyncManager) GetConnectors() []kb.Connector {
	return sm.connectors
}
