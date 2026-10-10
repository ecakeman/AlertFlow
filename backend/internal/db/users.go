package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned when a lookup matches no rows.
var ErrNotFound = errors.New("not found")

// User is an application account.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	PasswordHash string `json:"-"`
	CreatedAt    int64  `json:"createdAt"`
}

// CountUsers returns the number of accounts (used to detect first-run setup).
func (db *DB) CountUsers() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a new account and returns it.
func (db *DB) CreateUser(username, passwordHash, role string) (*User, error) {
	now := time.Now().Unix()
	var id int64
	err := db.QueryRow(
		`INSERT INTO users (username, password_hash, role, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		username, passwordHash, role, now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, Role: role, PasswordHash: passwordHash, CreatedAt: now}, nil
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByUsername looks up an account by username.
func (db *DB) GetUserByUsername(username string) (*User, error) {
	return scanUser(db.QueryRow(
		`SELECT id, username, password_hash, role, created_at FROM users WHERE username = $1`, username))
}

// GetUserByID looks up an account by id.
func (db *DB) GetUserByID(id int64) (*User, error) {
	return scanUser(db.QueryRow(
		`SELECT id, username, password_hash, role, created_at FROM users WHERE id = $1`, id))
}

// ListUsers returns all accounts, oldest first.
func (db *DB) ListUsers() ([]User, error) {
	rows, err := db.Query(`SELECT id, username, password_hash, role, created_at FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateUser sets the role and (if non-empty) the password hash of an account.
func (db *DB) UpdateUser(id int64, role, passwordHash string) error {
	if passwordHash != "" {
		_, err := db.Exec(`UPDATE users SET role = $1, password_hash = $2 WHERE id = $3`, role, passwordHash, id)
		return err
	}
	_, err := db.Exec(`UPDATE users SET role = $1 WHERE id = $2`, role, id)
	return err
}

// DeleteUser removes an account (cascading its sessions).
func (db *DB) DeleteUser(id int64) error {
	_, err := db.Exec(`DELETE FROM users WHERE id = $1`, id)
	return err
}
