package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAuthTestWithRetryRecoversFromTransientErrors(t *testing.T) {
	calls := 0
	test := func(context.Context) (*slack.AuthTestResponse, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("dial tcp: lookup slack.com: connection refused")
		}
		return &slack.AuthTestResponse{UserID: "UBOT"}, nil
	}
	resp, err := authTestWithRetry(context.Background(), test, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}, quietLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.UserID != "UBOT" || calls != 3 {
		t.Fatalf("got user %q after %d calls, want UBOT after 3", resp.UserID, calls)
	}
}

func TestAuthTestWithRetryStopsOnPermanentError(t *testing.T) {
	calls := 0
	test := func(context.Context) (*slack.AuthTestResponse, error) {
		calls++
		return nil, slack.SlackErrorResponse{Err: "invalid_auth"}
	}
	_, err := authTestWithRetry(context.Background(), test, []time.Duration{time.Millisecond, time.Millisecond}, quietLogger())
	if err == nil || calls != 1 {
		t.Fatalf("got err %v after %d calls, want an error after 1", err, calls)
	}
}

func TestAuthTestWithRetryGivesUpAfterDelays(t *testing.T) {
	calls := 0
	test := func(context.Context) (*slack.AuthTestResponse, error) {
		calls++
		return nil, errors.New("network down")
	}
	_, err := authTestWithRetry(context.Background(), test, []time.Duration{time.Millisecond, time.Millisecond}, quietLogger())
	if err == nil || calls != 3 {
		t.Fatalf("got err %v after %d calls, want an error after 3", err, calls)
	}
}

func TestAuthTestWithRetryStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	test := func(context.Context) (*slack.AuthTestResponse, error) {
		calls++
		cancel()
		return nil, errors.New("network down")
	}
	_, err := authTestWithRetry(ctx, test, []time.Duration{time.Hour}, quietLogger())
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("got err %v after %d calls, want context.Canceled after 1", err, calls)
	}
}
