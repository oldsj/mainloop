package db

import "context"

// UpTo applies the pending migrations up to and including version, so a test can seed rows a
// later migration rewrites.
func (m *Migrator) UpTo(ctx context.Context, version int64) (int, error) {
	results, err := m.current().UpTo(ctx, version)
	return len(results), err
}
