package storage

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

func TestPluginAccounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetPluginAccount(ctx, "slack:U1", "daouoffice")
	if err != nil || got != nil {
		t.Fatalf("GetPluginAccount(missing) = %v, %v; want nil, nil", got, err)
	}

	acct := PluginAccount{
		UserKey: "slack:U1", Plugin: "daouoffice", Secret: []byte{1, 2, 3},
		AccountName: "홍길동 (개발본부)", AccountID: "hong", AllowKick: true,
	}
	if err := store.UpsertPluginAccount(ctx, acct); err != nil {
		t.Fatalf("UpsertPluginAccount() error = %v", err)
	}
	got, err = store.GetPluginAccount(ctx, "slack:U1", "daouoffice")
	if err != nil || got == nil {
		t.Fatalf("GetPluginAccount() = %v, %v", got, err)
	}
	if !bytes.Equal(got.Secret, acct.Secret) || got.AccountName != acct.AccountName ||
		got.AccountID != "hong" || !got.AllowKick || got.Status != "ok" || got.ConnectedAt.IsZero() {
		t.Errorf("GetPluginAccount() = %+v", got)
	}

	acct.Secret = []byte{9}
	acct.AllowKick = false
	if err := store.UpsertPluginAccount(ctx, acct); err != nil {
		t.Fatalf("UpsertPluginAccount(replace) error = %v", err)
	}
	if err := store.SetPluginAccountStatus(ctx, "slack:U1", "daouoffice", "invalid"); err != nil {
		t.Fatalf("SetPluginAccountStatus() error = %v", err)
	}
	got, _ = store.GetPluginAccount(ctx, "slack:U1", "daouoffice")
	if !bytes.Equal(got.Secret, []byte{9}) || got.AllowKick || got.Status != "invalid" {
		t.Errorf("after replace and invalidate = %+v", got)
	}

	if err := store.UpsertPluginAccount(ctx, PluginAccount{UserKey: "slack:U1", Plugin: "erp", Secret: []byte{4}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPluginAccount(ctx, PluginAccount{UserKey: "slack:U2", Plugin: "erp", Secret: []byte{5}}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListPluginAccounts(ctx, "slack:U1")
	if err != nil {
		t.Fatalf("ListPluginAccounts() error = %v", err)
	}
	if len(list) != 2 || list[0].Plugin != "daouoffice" || list[1].Plugin != "erp" {
		t.Errorf("ListPluginAccounts() = %+v", list)
	}

	if err := store.DeletePluginAccount(ctx, "slack:U1", "daouoffice"); err != nil {
		t.Fatalf("DeletePluginAccount() error = %v", err)
	}
	if got, _ := store.GetPluginAccount(ctx, "slack:U1", "daouoffice"); got != nil {
		t.Errorf("account still present after delete: %+v", got)
	}
}

func TestPluginPrompts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	has, err := store.HasPluginPrompt(ctx, "slack:U1", "daouoffice")
	if err != nil || has {
		t.Fatalf("HasPluginPrompt(before) = %v, %v", has, err)
	}
	for range 2 { // recording twice is not an error
		if err := store.RecordPluginPrompt(ctx, "slack:U1", "daouoffice"); err != nil {
			t.Fatalf("RecordPluginPrompt() error = %v", err)
		}
	}
	if has, _ := store.HasPluginPrompt(ctx, "slack:U1", "daouoffice"); !has {
		t.Error("HasPluginPrompt(after) = false")
	}
	if has, _ := store.HasPluginPrompt(ctx, "slack:U2", "daouoffice"); has {
		t.Error("HasPluginPrompt(other user) = true")
	}
}

func newTestAction(id string, expires time.Time) PluginAction {
	return PluginAction{
		ID: id, Kind: "call", UserKey: "slack:U1", Plugin: "daouoffice", Tool: "submit_purchase",
		Args: `{"key":"p1"}`, Channel: "C1", Thread: "100.1", Preview: "미리보기", Status: "pending",
		ExpiresAt: expires,
	}
}

func TestPluginActionLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.CreatePluginAction(ctx, newTestAction("a1", now.Add(30*time.Minute))); err != nil {
		t.Fatalf("CreatePluginAction() error = %v", err)
	}
	got, err := store.GetPluginAction(ctx, "a1")
	if err != nil || got == nil {
		t.Fatalf("GetPluginAction() = %v, %v", got, err)
	}
	if got.Attachments != "[]" || got.Origin != "{}" || got.Args != `{"key":"p1"}` || got.Status != "pending" {
		t.Errorf("GetPluginAction() = %+v", got)
	}
	if missing, err := store.GetPluginAction(ctx, "nope"); err != nil || missing != nil {
		t.Errorf("GetPluginAction(missing) = %v, %v", missing, err)
	}

	if err := store.SetPluginActionPrompt(ctx, "a1", "C1", "200.2"); err != nil {
		t.Fatalf("SetPluginActionPrompt() error = %v", err)
	}

	pending, err := store.PendingPluginActionInThread(ctx, "slack:U1", "C1", "100.1", now)
	if err != nil || pending == nil || pending.ID != "a1" || pending.PromptMessage != "200.2" {
		t.Fatalf("PendingPluginActionInThread() = %+v, %v", pending, err)
	}
	for _, tc := range []struct{ name, user, channel, thread string }{
		{"other user", "slack:U2", "C1", "100.1"},
		{"other channel", "slack:U1", "C2", "100.1"},
		{"other thread", "slack:U1", "C1", "100.9"},
	} {
		t.Run(tc.name+" sees nothing", func(t *testing.T) {
			got, err := store.PendingPluginActionInThread(ctx, tc.user, tc.channel, tc.thread, now)
			if err != nil || got != nil {
				t.Errorf("got %+v, %v", got, err)
			}
		})
	}

	ok, err := store.TransitionPluginAction(ctx, "a1", "pending", "running", "")
	if err != nil || !ok {
		t.Fatalf("Transition pending->running = %v, %v", ok, err)
	}
	ok, err = store.TransitionPluginAction(ctx, "a1", "pending", "running", "")
	if err != nil || ok {
		t.Fatalf("second Transition pending->running = %v, %v; want false", ok, err)
	}
	if ok, _ := store.TransitionPluginAction(ctx, "a1", "running", "done", `{"text":"ok"}`); !ok {
		t.Fatal("Transition running->done failed")
	}
	got, _ = store.GetPluginAction(ctx, "a1")
	if got.Status != "done" || got.Result != `{"text":"ok"}` {
		t.Errorf("after done = %+v", got)
	}
	if p, _ := store.PendingPluginActionInThread(ctx, "slack:U1", "C1", "100.1", now); p != nil {
		t.Errorf("done action still pending in thread: %+v", p)
	}
}

func TestTransitionPluginActionConcurrent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreatePluginAction(ctx, newTestAction("a1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 8 {
		wg.Go(func() {
			ok, err := store.TransitionPluginAction(ctx, "a1", "pending", "running", "")
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d callers won the transition, want 1", wins)
	}
}

func TestExpirePluginActions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, a := range []PluginAction{
		newTestAction("old", now.Add(-time.Minute)),
		newTestAction("fresh", now.Add(time.Hour)),
	} {
		if err := store.CreatePluginAction(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	done := newTestAction("old-done", now.Add(-time.Minute))
	done.Status = "done"
	if err := store.CreatePluginAction(ctx, done); err != nil {
		t.Fatal(err)
	}

	if p, _ := store.PendingPluginActionInThread(ctx, "slack:U1", "C1", "100.1", now); p == nil || p.ID != "fresh" {
		t.Fatalf("PendingPluginActionInThread() = %+v; want the unexpired action", p)
	}

	expired, err := store.ExpirePluginActions(ctx, now)
	if err != nil {
		t.Fatalf("ExpirePluginActions() error = %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "old" || expired[0].Status != "expired" {
		t.Fatalf("ExpirePluginActions() = %+v; want only the old pending action", expired)
	}
	if again, _ := store.ExpirePluginActions(ctx, now); len(again) != 0 {
		t.Errorf("second ExpirePluginActions() = %+v; want none", again)
	}
	for id, want := range map[string]string{"old": "expired", "fresh": "pending", "old-done": "done"} {
		got, _ := store.GetPluginAction(ctx, id)
		if got.Status != want {
			t.Errorf("%s status = %s, want %s", id, got.Status, want)
		}
	}
}
