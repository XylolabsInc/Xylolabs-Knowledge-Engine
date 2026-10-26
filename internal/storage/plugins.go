package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PluginAccount is one person's stored account for one plugin. Secret is the
// vault-sealed account form; this package never decrypts it.
type PluginAccount struct {
	UserKey     string
	Plugin      string
	Secret      []byte
	AccountName string
	AccountID   string
	AllowKick   bool
	Status      string // "ok" or "invalid"
	ConnectedAt time.Time
	UpdatedAt   time.Time
}

// PluginAction is a plugin call held for a human decision. Args, Attachments
// and Origin are JSON documents owned by internal/plugin.
type PluginAction struct {
	ID            string
	Kind          string // "call" or "login_kick"
	UserKey       string
	Plugin        string
	Tool          string
	Args          string
	Attachments   string
	Origin        string
	Channel       string
	Thread        string
	Preview       string
	Status        string // pending, running, done, failed, cancelled, expired
	Result        string
	PromptChannel string
	PromptMessage string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UpdatedAt     time.Time
}

const pluginActionColumns = `id, kind, user_key, plugin, tool, args, attachments, origin, channel, thread,
	preview, status, result, prompt_channel, prompt_message, created_at, expires_at, updated_at`

// UpsertPluginAccount stores or replaces a person's account for a plugin.
func (s *SQLiteStore) UpsertPluginAccount(ctx context.Context, a PluginAccount) error {
	now := time.Now().UTC()
	connectedAt := a.ConnectedAt.UTC()
	if a.ConnectedAt.IsZero() {
		connectedAt = now
	}
	status := a.Status
	if status == "" {
		status = "ok"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plugin_accounts (user_key, plugin, secret, account_name, account_id, allow_kick, status, connected_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_key, plugin) DO UPDATE SET
			secret=excluded.secret,
			account_name=excluded.account_name,
			account_id=excluded.account_id,
			allow_kick=excluded.allow_kick,
			status=excluded.status,
			connected_at=excluded.connected_at,
			updated_at=excluded.updated_at`,
		a.UserKey, a.Plugin, a.Secret, a.AccountName, a.AccountID, boolInt(a.AllowKick), status, connectedAt, now,
	)
	if err != nil {
		return fmt.Errorf("storage: upsert plugin account %s/%s: %w", a.UserKey, a.Plugin, err)
	}
	return nil
}

// GetPluginAccount returns a person's account for a plugin, or nil if there is none.
func (s *SQLiteStore) GetPluginAccount(ctx context.Context, userKey, plugin string) (*PluginAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_key, plugin, secret, account_name, account_id, allow_kick, status, connected_at, updated_at
		FROM plugin_accounts WHERE user_key = ? AND plugin = ?`, userKey, plugin)
	a, err := scanPluginAccount(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get plugin account %s/%s: %w", userKey, plugin, err)
	}
	return a, nil
}

// ListPluginAccounts returns every plugin account a person has connected.
func (s *SQLiteStore) ListPluginAccounts(ctx context.Context, userKey string) ([]PluginAccount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_key, plugin, secret, account_name, account_id, allow_kick, status, connected_at, updated_at
		FROM plugin_accounts WHERE user_key = ? ORDER BY plugin`, userKey)
	if err != nil {
		return nil, fmt.Errorf("storage: list plugin accounts %s: %w", userKey, err)
	}
	defer rows.Close()
	var accounts []PluginAccount
	for rows.Next() {
		a, err := scanPluginAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan plugin account: %w", err)
		}
		accounts = append(accounts, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list plugin accounts %s: %w", userKey, err)
	}
	return accounts, nil
}

// DeletePluginAccount removes a person's account for a plugin.
func (s *SQLiteStore) DeletePluginAccount(ctx context.Context, userKey, plugin string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM plugin_accounts WHERE user_key = ? AND plugin = ?`, userKey, plugin); err != nil {
		return fmt.Errorf("storage: delete plugin account %s/%s: %w", userKey, plugin, err)
	}
	return nil
}

// SetPluginAccountStatus marks an account "ok" or "invalid".
func (s *SQLiteStore) SetPluginAccountStatus(ctx context.Context, userKey, plugin, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plugin_accounts SET status = ?, updated_at = ? WHERE user_key = ? AND plugin = ?`,
		status, time.Now().UTC(), userKey, plugin)
	if err != nil {
		return fmt.Errorf("storage: set plugin account status %s/%s: %w", userKey, plugin, err)
	}
	return nil
}

// RecordPluginPrompt remembers that a person was asked to connect a plugin.
func (s *SQLiteStore) RecordPluginPrompt(ctx context.Context, userKey, plugin string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plugin_prompts (user_key, plugin, prompted_at) VALUES (?, ?, ?)
		ON CONFLICT(user_key, plugin) DO UPDATE SET prompted_at=excluded.prompted_at`,
		userKey, plugin, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("storage: record plugin prompt %s/%s: %w", userKey, plugin, err)
	}
	return nil
}

// HasPluginPrompt reports whether a person was already asked to connect a plugin.
func (s *SQLiteStore) HasPluginPrompt(ctx context.Context, userKey, plugin string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_prompts WHERE user_key = ? AND plugin = ?`, userKey, plugin).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("storage: check plugin prompt %s/%s: %w", userKey, plugin, err)
	}
	return n > 0, nil
}

// CreatePluginAction stores a new action.
func (s *SQLiteStore) CreatePluginAction(ctx context.Context, a PluginAction) error {
	now := time.Now().UTC()
	createdAt := a.CreatedAt.UTC()
	if a.CreatedAt.IsZero() {
		createdAt = now
	}
	attachments := a.Attachments
	if attachments == "" {
		attachments = "[]"
	}
	origin := a.Origin
	if origin == "" {
		origin = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plugin_actions (`+pluginActionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Kind, a.UserKey, a.Plugin, a.Tool, a.Args, attachments, origin, a.Channel, a.Thread,
		a.Preview, a.Status, a.Result, a.PromptChannel, a.PromptMessage, createdAt, a.ExpiresAt.UTC(), now,
	)
	if err != nil {
		return fmt.Errorf("storage: create plugin action %s: %w", a.ID, err)
	}
	return nil
}

// GetPluginAction returns an action by ID, or nil if there is none.
func (s *SQLiteStore) GetPluginAction(ctx context.Context, id string) (*PluginAction, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pluginActionColumns+` FROM plugin_actions WHERE id = ?`, id)
	a, err := scanPluginAction(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get plugin action %s: %w", id, err)
	}
	return a, nil
}

// TransitionPluginAction moves an action from one status to another in a
// single conditional UPDATE, so two concurrent callers cannot both win. It
// reports whether this call made the change. A non-empty result replaces the
// stored one.
func (s *SQLiteStore) TransitionPluginAction(ctx context.Context, id, from, to, result string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE plugin_actions
		SET status = ?, result = CASE WHEN ? = '' THEN result ELSE ? END, updated_at = ?
		WHERE id = ? AND status = ?`,
		to, result, result, time.Now().UTC(), id, from)
	if err != nil {
		return false, fmt.Errorf("storage: transition plugin action %s %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: transition plugin action %s: rows affected: %w", id, err)
	}
	return n == 1, nil
}

// SetPluginActionPrompt records where the bot posted an action's buttons.
func (s *SQLiteStore) SetPluginActionPrompt(ctx context.Context, id, channel, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plugin_actions SET prompt_channel = ?, prompt_message = ?, updated_at = ? WHERE id = ?`,
		channel, message, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("storage: set plugin action prompt %s: %w", id, err)
	}
	return nil
}

// PendingPluginActionInThread returns the newest unexpired pending action a
// person has in a thread, or nil if there is none.
func (s *SQLiteStore) PendingPluginActionInThread(ctx context.Context, userKey, channel, thread string, now time.Time) (*PluginAction, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+pluginActionColumns+` FROM plugin_actions
		WHERE user_key = ? AND channel = ? AND thread = ? AND status = 'pending' AND expires_at > ?
		ORDER BY created_at DESC LIMIT 1`,
		userKey, channel, thread, now.UTC())
	a, err := scanPluginAction(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: pending plugin action %s in %s/%s: %w", userKey, channel, thread, err)
	}
	return a, nil
}

// ExpirePluginActions marks every pending action whose expiry is at or before
// now as expired and returns them.
func (s *SQLiteStore) ExpirePluginActions(ctx context.Context, now time.Time) ([]PluginAction, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE plugin_actions SET status = 'expired', updated_at = ?
		WHERE status = 'pending' AND expires_at <= ?
		RETURNING `+pluginActionColumns,
		time.Now().UTC(), now.UTC())
	if err != nil {
		return nil, fmt.Errorf("storage: expire plugin actions: %w", err)
	}
	defer rows.Close()
	var actions []PluginAction
	for rows.Next() {
		a, err := scanPluginAction(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan expired plugin action: %w", err)
		}
		actions = append(actions, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: expire plugin actions: %w", err)
	}
	return actions, nil
}

// FailStalePluginActions marks every action still running since before as
// failed with result, and returns them. A running action that old outlived
// its call timeout, which means the bot stopped while it ran.
func (s *SQLiteStore) FailStalePluginActions(ctx context.Context, before time.Time, result string) ([]PluginAction, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE plugin_actions SET status = 'failed', result = ?, updated_at = ?
		WHERE status = 'running' AND updated_at <= ?
		RETURNING `+pluginActionColumns,
		result, time.Now().UTC(), before.UTC())
	if err != nil {
		return nil, fmt.Errorf("storage: fail stale plugin actions: %w", err)
	}
	defer rows.Close()
	var actions []PluginAction
	for rows.Next() {
		a, err := scanPluginAction(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan stale plugin action: %w", err)
		}
		actions = append(actions, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: fail stale plugin actions: %w", err)
	}
	return actions, nil
}

func scanPluginAccount(row scanner) (*PluginAccount, error) {
	var a PluginAccount
	var allowKick int
	if err := row.Scan(&a.UserKey, &a.Plugin, &a.Secret, &a.AccountName, &a.AccountID,
		&allowKick, &a.Status, &a.ConnectedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.AllowKick = allowKick == 1
	return &a, nil
}

func scanPluginAction(row scanner) (*PluginAction, error) {
	var a PluginAction
	if err := row.Scan(&a.ID, &a.Kind, &a.UserKey, &a.Plugin, &a.Tool, &a.Args, &a.Attachments, &a.Origin,
		&a.Channel, &a.Thread, &a.Preview, &a.Status, &a.Result, &a.PromptChannel, &a.PromptMessage,
		&a.CreatedAt, &a.ExpiresAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
