package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultConfigTable = "config_store"
	defaultAuthTable   = "auth_store"
	defaultConfigKey   = "config"
	authBatchSize      = 500
)

// PostgresStoreConfig captures configuration required to initialize a Postgres-backed store.
type PostgresStoreConfig struct {
	DSN         string
	Schema      string
	ConfigTable string
	AuthTable   string
	SpoolDir    string
}

// PostgresStore persists configuration and authentication metadata using PostgreSQL as backend
// while mirroring data to a local workspace so existing file-based workflows continue to operate.
type PostgresStore struct {
	db         *sql.DB
	cfg        PostgresStoreConfig
	spoolRoot  string
	configPath string
	authDir    string
	mu         sync.Mutex
	schemaOnce sync.Once
}

// NewPostgresStore establishes a connection to PostgreSQL and prepares the local workspace.
func NewPostgresStore(ctx context.Context, cfg PostgresStoreConfig) (*PostgresStore, error) {
	trimmedDSN := strings.TrimSpace(cfg.DSN)
	if trimmedDSN == "" {
		return nil, fmt.Errorf("postgres store: DSN is required")
	}
	cfg.DSN = trimmedDSN
	if cfg.ConfigTable == "" {
		cfg.ConfigTable = defaultConfigTable
	}
	if cfg.AuthTable == "" {
		cfg.AuthTable = defaultAuthTable
	}

	spoolRoot := strings.TrimSpace(cfg.SpoolDir)
	if spoolRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			spoolRoot = filepath.Join(cwd, "pgstore")
		} else {
			spoolRoot = filepath.Join(os.TempDir(), "pgstore")
		}
	}
	absSpool, err := filepath.Abs(spoolRoot)
	if err != nil {
		return nil, fmt.Errorf("postgres store: resolve spool directory: %w", err)
	}
	configDir := filepath.Join(absSpool, "config")
	authDir := filepath.Join(absSpool, "auths")
	if err = os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("postgres store: create config directory: %w", err)
	}
	if err = os.MkdirAll(authDir, 0o700); err != nil {
		return nil, fmt.Errorf("postgres store: create auth directory: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres store: open database connection: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres store: ping database: %w", err)
	}

	store := &PostgresStore{
		db:         db,
		cfg:        cfg,
		spoolRoot:  absSpool,
		configPath: filepath.Join(configDir, "config.yaml"),
		authDir:    authDir,
	}
	return store, nil
}

// Close releases the underlying database connection.
func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnsureSchema creates the required tables and indexes. The underlying DDL is
// executed at most once per store instance, regardless of how many times this
// method is called.
func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	var err error
	s.schemaOnce.Do(func() {
		err = s.ensureSchemaOnce(ctx)
	})
	return err
}

func (s *PostgresStore) ensureSchemaOnce(ctx context.Context) error {
	if schema := strings.TrimSpace(s.cfg.Schema); schema != "" {
		query := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdentifier(schema))
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("postgres store: create schema: %w", err)
		}
	}
	configTable := s.fullTableName(s.cfg.ConfigTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, configTable)); err != nil {
		return fmt.Errorf("postgres store: create config table: %w", err)
	}
	authTable := s.fullTableName(s.cfg.AuthTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			content JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, authTable)); err != nil {
		return fmt.Errorf("postgres store: create auth table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_auth_updated_at ON %s(updated_at)`, authTable,
	)); err != nil {
		return fmt.Errorf("postgres store: create updated_at index: %w", err)
	}
	return nil
}

// Bootstrap synchronizes configuration and auth records between PostgreSQL and the local workspace.
func (s *PostgresStore) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if err := s.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := s.syncConfigFromDatabase(ctx, exampleConfigPath); err != nil {
		return err
	}
	if err := s.syncAuthFromDatabase(ctx); err != nil {
		return err
	}
	return nil
}

// ConfigPath returns the managed configuration file path inside the spool directory.
func (s *PostgresStore) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

// AuthDir returns the local directory containing mirrored auth files.
func (s *PostgresStore) AuthDir() string {
	if s == nil {
		return ""
	}
	return s.authDir
}

// WorkDir exposes the root spool directory used for mirroring.
func (s *PostgresStore) WorkDir() string {
	if s == nil {
		return ""
	}
	return s.spoolRoot
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the Postgres-backed store controls its own workspace.
func (s *PostgresStore) SetBaseDir(string) {}

// Save persists authentication metadata to disk and PostgreSQL.
func (s *PostgresStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("postgres store: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("postgres store: create auth directory: %w", err)
	}

	switch {
	case auth.Storage != nil:
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["disabled"] = auth.Disabled
		if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
			setter.SetMetadata(auth.Metadata)
		}
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		auth.Metadata["disabled"] = auth.Disabled
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("postgres store: marshal metadata: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
		} else if errRead != nil && !errors.Is(errRead, fs.ErrNotExist) {
			return "", fmt.Errorf("postgres store: read existing metadata: %w", errRead)
		}
		tmp := path + ".tmp"
		if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("postgres store: write temp auth file: %w", errWrite)
		}
		if errRename := os.Rename(tmp, path); errRename != nil {
			return "", fmt.Errorf("postgres store: rename auth file: %w", errRename)
		}
	default:
		return "", fmt.Errorf("postgres store: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourcePostgres

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	relID, err := s.relativeAuthID(path)
	if err != nil {
		return "", err
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", fmt.Errorf("postgres store: read auth file for upsert: %w", errRead)
	}
	if err = s.upsertAuthRecord(ctx, relID, data); err != nil {
		return "", err
	}
	return path, nil
}

// scanAuthRow converts a single database row into an Auth value. It returns
// nil when the row should be skipped (e.g. invalid path or unparseable JSON).
func (s *PostgresStore) scanAuthRow(id, payload string, createdAt, updatedAt time.Time) *cliproxyauth.Auth {
	path, errPath := s.absoluteAuthPath(id)
	if errPath != nil {
		log.WithError(errPath).Warnf("postgres store: skipping auth %s outside spool", id)
		return nil
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal([]byte(payload), &metadata); err != nil {
		log.WithError(err).Warnf("postgres store: skipping auth %s with invalid json", id)
		return nil
	}
	provider := strings.TrimSpace(valueAsString(metadata["type"]))
	if provider == "" {
		provider = "unknown"
	}
	attr := map[string]string{
		cliproxyauth.AttributePath:          path,
		cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourcePostgres,
	}
	if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
		attr["email"] = email
	}
	auth := &cliproxyauth.Auth{
		ID:               normalizeAuthID(id),
		Provider:         provider,
		FileName:         normalizeAuthID(id),
		Label:            labelFor(metadata),
		Status:           cliproxyauth.StatusActive,
		Attributes:       attr,
		Metadata:         metadata,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
	if disabled, ok := metadata["disabled"].(bool); ok && disabled {
		auth.Disabled = true
		auth.Status = cliproxyauth.StatusDisabled
	}
	return auth
}

// List enumerates all auth records stored in PostgreSQL.
func (s *PostgresStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, created_at, updated_at FROM %s ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id        string
			payload   string
			createdAt time.Time
			updatedAt time.Time
		)
		if err = rows.Scan(&id, &payload, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		if a := s.scanAuthRow(id, payload, createdAt, updatedAt); a != nil {
			auths = append(auths, a)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// ListSince returns auth records whose updated_at is after the given time.
// The caller must ensure the idx_auth_updated_at index exists (created during
// EnsureSchema).
func (s *PostgresStore) ListSince(ctx context.Context, after time.Time) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf(
		"SELECT id, content, created_at, updated_at FROM %s WHERE updated_at > $1 ORDER BY updated_at",
		s.fullTableName(s.cfg.AuthTable),
	)
	rows, err := s.db.QueryContext(ctx, query, after)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth since: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id        string
			payload   string
			createdAt time.Time
			updatedAt time.Time
		)
		if err = rows.Scan(&id, &payload, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		if a := s.scanAuthRow(id, payload, createdAt, updatedAt); a != nil {
			auths = append(auths, a)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// Delete removes an auth file and the corresponding database record.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("postgres store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("postgres store: delete auth file: %w", err)
	}
	relID, err := s.relativeAuthID(path)
	if err != nil {
		return err
	}
	return s.deleteAuthRecord(ctx, relID)
}

// PersistAuthFiles stores the provided auth file changes in PostgreSQL using
// batched upserts.
func (s *PostgresStore) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var upserts []authRecord
	var deletes []string
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		relID, err := s.relativeAuthID(trimmed)
		if err != nil {
			abs := trimmed
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(s.authDir, trimmed)
			}
			relID, err = s.relativeAuthID(abs)
			if err != nil {
				log.WithError(err).Warnf("postgres store: ignoring auth path %s", trimmed)
				continue
			}
			trimmed = abs
		}
		data, errRead := os.ReadFile(trimmed)
		if errRead != nil {
			if errors.Is(errRead, fs.ErrNotExist) {
				deletes = append(deletes, relID)
			} else {
				return fmt.Errorf("postgres store: read auth file: %w", errRead)
			}
			continue
		}
		if len(data) == 0 {
			deletes = append(deletes, relID)
			continue
		}
		upserts = append(upserts, authRecord{id: relID, data: data})
	}

	for _, id := range deletes {
		if err := s.deleteAuthRecord(ctx, id); err != nil {
			return err
		}
	}
	return s.batchPersistAuth(ctx, upserts)
}

// authRecord pairs an auth identifier with its serialized content.
type authRecord struct {
	id   string
	data []byte
}

// batchPersistAuth upserts auth records in batches to stay under PostgreSQL
// parameter limits.
func (s *PostgresStore) batchPersistAuth(ctx context.Context, records []authRecord) error {
	if len(records) == 0 {
		return nil
	}
	tableName := s.fullTableName(s.cfg.AuthTable)
	for i := 0; i < len(records); i += authBatchSize {
		end := min(i+authBatchSize, len(records))
		batch := records[i:end]
		numCols := 2
		valueStrings := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*numCols)
		for idx, r := range batch {
			base := idx * numCols
			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, NOW(), NOW())", base+1, base+2))
			args = append(args, r.id, json.RawMessage(r.data))
		}
		query := fmt.Sprintf(`
			INSERT INTO %s (id, content, created_at, updated_at)
			VALUES %s
			ON CONFLICT (id)
			DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
		`, tableName, strings.Join(valueStrings, ", "))
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("postgres store: batch persist auth: %w", err)
		}
	}
	return nil
}

// PersistConfig mirrors the local configuration file to PostgreSQL.
func (s *PostgresStore) PersistConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteConfigRecord(ctx)
		}
		return fmt.Errorf("postgres store: read config file: %w", err)
	}
	return s.persistConfig(ctx, data)
}

// syncConfigFromDatabase writes the database-stored config to disk or seeds the database from template.
func (s *PostgresStore) syncConfigFromDatabase(ctx context.Context, exampleConfigPath string) error {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var content string
	err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, errStat := os.Stat(s.configPath); errors.Is(errStat, fs.ErrNotExist) {
			if exampleConfigPath != "" {
				if errCopy := misc.CopyConfigTemplate(exampleConfigPath, s.configPath); errCopy != nil {
					return fmt.Errorf("postgres store: copy example config: %w", errCopy)
				}
			} else {
				if errCreate := os.MkdirAll(filepath.Dir(s.configPath), 0o700); errCreate != nil {
					return fmt.Errorf("postgres store: prepare config directory: %w", errCreate)
				}
				if errWrite := os.WriteFile(s.configPath, []byte{}, 0o600); errWrite != nil {
					return fmt.Errorf("postgres store: create empty config: %w", errWrite)
				}
			}
		}
		data, errRead := os.ReadFile(s.configPath)
		if errRead != nil {
			return fmt.Errorf("postgres store: read local config: %w", errRead)
		}
		if errPersist := s.persistConfig(ctx, data); errPersist != nil {
			return errPersist
		}
	case err != nil:
		return fmt.Errorf("postgres store: load config from database: %w", err)
	default:
		if err = os.MkdirAll(filepath.Dir(s.configPath), 0o700); err != nil {
			return fmt.Errorf("postgres store: prepare config directory: %w", err)
		}
		normalized := normalizeLineEndings(content)
		if err = os.WriteFile(s.configPath, []byte(normalized), 0o600); err != nil {
			return fmt.Errorf("postgres store: write config to spool: %w", err)
		}
	}
	return nil
}

// syncAuthFromDatabase populates the local auth directory from PostgreSQL data.
func (s *PostgresStore) syncAuthFromDatabase(ctx context.Context) error {
	query := fmt.Sprintf("SELECT id, content FROM %s", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("postgres store: load auth from database: %w", err)
	}
	defer rows.Close()

	if err = os.RemoveAll(s.authDir); err != nil {
		return fmt.Errorf("postgres store: reset auth directory: %w", err)
	}
	if err = os.MkdirAll(s.authDir, 0o700); err != nil {
		return fmt.Errorf("postgres store: recreate auth directory: %w", err)
	}

	for rows.Next() {
		var (
			id      string
			payload string
		)
		if err = rows.Scan(&id, &payload); err != nil {
			return fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		path, errPath := s.absoluteAuthPath(id)
		if errPath != nil {
			log.WithError(errPath).Warnf("postgres store: skipping auth %s outside spool", id)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("postgres store: create auth subdir: %w", err)
		}
		if err = os.WriteFile(path, []byte(payload), 0o600); err != nil {
			return fmt.Errorf("postgres store: write auth file: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return nil
}

func (s *PostgresStore) upsertAuthRecord(ctx context.Context, relID string, data []byte) error {
	if len(data) == 0 {
		return s.deleteAuthRecord(ctx, relID)
	}
	return s.persistAuth(ctx, relID, data)
}

func (s *PostgresStore) persistAuth(ctx context.Context, relID string, data []byte) error {
	jsonPayload := json.RawMessage(data)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (id)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID, jsonPayload); err != nil {
		return fmt.Errorf("postgres store: upsert auth record: %w", err)
	}
	return nil
}

func (s *PostgresStore) deleteAuthRecord(ctx context.Context, relID string) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID); err != nil {
		return fmt.Errorf("postgres store: delete auth record: %w", err)
	}
	return nil
}

func (s *PostgresStore) persistConfig(ctx context.Context, data []byte) error {
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (id)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.fullTableName(s.cfg.ConfigTable))
	normalized := normalizeLineEndings(string(data))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey, normalized); err != nil {
		return fmt.Errorf("postgres store: upsert config: %w", err)
	}
	return nil
}

func (s *PostgresStore) deleteConfigRecord(ctx context.Context) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey); err != nil {
		return fmt.Errorf("postgres store: delete config: %w", err)
	}
	return nil
}

func (s *PostgresStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		return filepath.Join(s.authDir, fileName), nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("postgres store: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(auth.ID)), nil
}

func (s *PostgresStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(id)), nil
}

func (s *PostgresStore) relativeAuthID(path string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("postgres store: store not initialized")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.authDir, path)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.authDir, clean)
	if err != nil {
		return "", fmt.Errorf("postgres store: compute relative path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("postgres store: path %s outside managed directory", path)
	}
	return filepath.ToSlash(rel), nil
}

func (s *PostgresStore) absoluteAuthPath(id string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("postgres store: store not initialized")
	}
	clean := filepath.Clean(filepath.FromSlash(id))
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("postgres store: invalid auth identifier %s", id)
	}
	path := filepath.Join(s.authDir, clean)
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("postgres store: resolved auth path escapes auth directory")
	}
	return path, nil
}

func (s *PostgresStore) fullTableName(name string) string {
	if strings.TrimSpace(s.cfg.Schema) == "" {
		return quoteIdentifier(name)
	}
	return quoteIdentifier(s.cfg.Schema) + "." + quoteIdentifier(name)
}

func quoteIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "\"", "\"\"")
	return "\"" + replaced + "\""
}

func valueAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v := strings.TrimSpace(valueAsString(metadata["label"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["email"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["project_id"])); v != "" {
		return v
	}
	return ""
}

func normalizeAuthID(id string) string {
	return filepath.ToSlash(filepath.Clean(id))
}

func normalizeLineEndings(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}
