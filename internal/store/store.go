package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrUnauthorized      = errors.New("unauthorized")
	ErrForbidden         = errors.New("forbidden")
	ErrNotFound          = errors.New("not found")
	ErrConflict          = errors.New("conflict")
	ErrLeaseLost         = errors.New("gateway lease lost")
	ErrPayloadConflict   = errors.New("client message ID already has a different payload")
	ErrHistoryExpired    = errors.New("message history expired")
	ErrInvalidAttachment = errors.New("attachment is unavailable for sending")
	ErrRateLimited       = errors.New("tenant message rate exceeded")
)

type Store struct {
	db *sql.DB
}

func Open(dsn string) (*Store, error) {
	db, err := openDB(dsn, false)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(16)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func openDB(dsn string, multiStatements bool) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("set MESSAGING_MYSQL_DSN")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid MySQL DSN")
	}
	if config.DBName == "" {
		return nil, errors.New("MySQL database name is required")
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = multiStatements
	config.InterpolateParams = false
	config.ClientFoundRows = false // Quota updates distinguish a changed row from a no-op.
	config.AllowAllFiles = false
	config.AllowCleartextPasswords = false
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 10 * time.Second
	config.WriteTimeout = 10 * time.Second
	if config.Params == nil {
		config.Params = make(map[string]string)
	}
	config.Params["time_zone"] = "'+00:00'"
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, errors.New("invalid MySQL connection settings")
	}
	return sql.OpenDB(connector), nil
}

func (s *Store) Ready(ctx context.Context) error {
	var version uint
	var dirty bool
	if err := s.db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != SchemaVersion || dirty {
		return fmt.Errorf("expected clean schema version %d", SchemaVersion)
	}
	return nil
}

func conflict(err error) error {
	var serverErr *mysql.MySQLError
	if errors.As(err, &serverErr) && serverErr.Number == 1062 {
		return ErrConflict
	}
	return err
}
