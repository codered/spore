package agent

import "context"

// MaybeCompact summarises old messages when the session outgrows its budget.
// Implemented in Task 9.
func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error { return nil }
