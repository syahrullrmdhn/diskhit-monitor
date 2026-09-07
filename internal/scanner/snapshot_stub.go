//go:build !windows

package scanner

import (
	"context"
	"time"
)

type directSnapshotProvider struct{}

func NewSnapshotProvider(_ time.Duration) SnapshotProvider {
	return directSnapshotProvider{}
}

func (directSnapshotProvider) Create(_ context.Context, root string) (Snapshot, error) {
	return Snapshot{SourceRoot: root, ReportedRoot: root, Consistent: false}, nil
}

func (directSnapshotProvider) Delete(context.Context, Snapshot) error { return nil }
