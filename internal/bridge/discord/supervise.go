package discord

import (
	"context"
	"log/slog"
	"time"
)

// Supervise keeps the bridge connected. A dropped gateway is normal — a
// laptop sleeps, a link flaps — so a failure to connect is a wait, never a
// fatal error for the daemon: spore's local web UI must keep working when
// Discord is unreachable.
//
// Backoff is capped rather than unbounded, because the thing on the other end
// is a service that comes back, and a bridge that has backed off to an hour
// is indistinguishable from a broken one.
//
// Supervise only retries a failed Open. Once connected it waits on ctx.Done(),
// because discordgo reconnects and resumes the gateway session itself. That is
// a deliberate limitation, not an oversight — the gateway's own recovery is
// sufficient for transient drops, and polling here would add complexity without
// benefit.
func Supervise(ctx context.Context, b *Bridge, log *slog.Logger) {
	const (
		minBackoff = 2 * time.Second
		maxBackoff = 2 * time.Minute
	)
	backoff := minBackoff
	for {
		if err := b.Start(ctx); err != nil {
			log.Warn("discord bridge could not connect; retrying", "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				_ = b.Close()
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		log.Info("discord bridge connected")
		backoff = minBackoff
		// Connected. discordgo reconnects and resumes the session itself, so
		// there is nothing to poll here; wait for shutdown.
		<-ctx.Done()
		_ = b.Close()
		return
	}
}
