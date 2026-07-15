package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"

	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

type Commander interface {
	RunCommand(name string, args ...string) error
	StartCommand(name string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error)
	Stat(path string) (os.FileInfo, error)
	MkdirAll(path string, perm os.FileMode) error
	Remove(path string) error
	Create(path string) (*os.File, error)
	Open(path string) (*os.File, error)
	ReadFile(path string) ([]byte, error)
}

type RealCommander struct{}

func (c *RealCommander) RunCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func (c *RealCommander) StartCommand(name string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd, nil
}

func (c *RealCommander) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (c *RealCommander) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (c *RealCommander) Remove(path string) error {
	return os.Remove(path)
}

func (c *RealCommander) Create(path string) (*os.File, error) {
	return os.Create(path)
}

func (c *RealCommander) Open(path string) (*os.File, error) {
	return os.Open(path)
}

func (c *RealCommander) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

type SQLStore struct {
	db *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

func (s *SQLStore) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

func (s *SQLStore) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *SQLStore) BeginTx(ctx context.Context, opts *sql.TxOptions) (types.Tx, error) {
	return s.db.BeginTx(ctx, opts)
}
