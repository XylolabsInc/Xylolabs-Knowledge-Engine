package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/slack-go/slack"
)

// slackAuthDelays spaces the auth.test retries at startup: about two and a
// half minutes in all. The bot needs its user ID before it can run, and the
// service can start while DNS is still down (unattended-upgrades restarts
// systemd-resolved and this unit together), so one failed call must not leave
// the bot off until the next restart.
var slackAuthDelays = []time.Duration{
	2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second,
	30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
}

// slackAuthAttemptTimeout bounds one auth.test call.
const slackAuthAttemptTimeout = 15 * time.Second

// permanentSlackAuthErrors are auth.test errors that retrying cannot fix.
var permanentSlackAuthErrors = map[string]bool{
	"invalid_auth":           true,
	"not_authed":             true,
	"account_inactive":       true,
	"token_revoked":          true,
	"token_expired":          true,
	"no_permission":          true,
	"missing_scope":          true,
	"not_allowed_token_type": true,
}

func isPermanentSlackAuthError(err error) bool {
	var se slack.SlackErrorResponse
	return errors.As(err, &se) && permanentSlackAuthErrors[se.Err]
}

// authTestWithRetry calls test until it succeeds, fails with an error that
// retrying cannot fix, runs out of delays, or ctx is done.
func authTestWithRetry(ctx context.Context, test func(context.Context) (*slack.AuthTestResponse, error), delays []time.Duration, logger *slog.Logger) (*slack.AuthTestResponse, error) {
	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, slackAuthAttemptTimeout)
		resp, err := test(attemptCtx)
		cancel()
		if err == nil {
			if attempt > 0 {
				logger.Info("slack auth.test succeeded after retry", "attempts", attempt+1)
			}
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isPermanentSlackAuthError(err) || attempt >= len(delays) {
			return nil, err
		}
		logger.Warn("slack auth.test failed, retrying", "attempt", attempt+1, "retry_in", delays[attempt], "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
}
