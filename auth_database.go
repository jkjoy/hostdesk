package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const authDatabaseName = "hostdesk.db"

var administratorUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

type authFile struct {
	Username string `json:"username"`
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`
}

type administratorRecord struct {
	Username string
	Salt     string
	Hash     string
}

func openAuthDatabase(dataDir string) (*sql.DB, error) {
	databasePath := filepath.Join(dataDir, authDatabaseName)
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建认证数据库: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(databasePath, 0600); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE IF NOT EXISTS administrators (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL UNIQUE,
			salt TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS login_attempts (
			scope TEXT NOT NULL,
			identity TEXT NOT NULL,
			failures INTEGER NOT NULL,
			first_failed_at INTEGER NOT NULL,
			locked_until INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (scope, identity)
		)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ftp_users (
			username TEXT PRIMARY KEY,
			home TEXT NOT NULL,
			site_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化认证数据库: %w", err)
		}
	}
	if err := migrateFTPUsers(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateLegacyAdministrator(db, filepath.Join(dataDir, "config.json")); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrateFTPUsers(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(ftp_users)")
	if err != nil {
		return fmt.Errorf("检查 FTP 用户表: %w", err)
	}
	defer rows.Close()
	hasSiteID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "site_id" {
			hasSiteID = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasSiteID {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE ftp_users ADD COLUMN site_id TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("迁移 FTP 用户网站绑定: %w", err)
	}
	return nil
}

func migrateLegacyAdministrator(db *sql.DB, configPath string) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM administrators").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取旧管理员配置: %w", err)
	}
	var legacy authFile
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("解析旧管理员配置: %w", err)
	}
	if strings.TrimSpace(legacy.Username) == "" || legacy.Salt == "" || legacy.Hash == "" {
		return errors.New("旧管理员配置不完整")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`INSERT OR IGNORE INTO administrators (id, username, salt, password_hash, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?)`,
		legacy.Username, legacy.Salt, legacy.Hash, now, now)
	if err != nil {
		return fmt.Errorf("迁移旧管理员配置: %w", err)
	}
	return nil
}

func (a *app) administrator() (administratorRecord, error) {
	var administrator administratorRecord
	err := a.db.QueryRow("SELECT username, salt, password_hash FROM administrators WHERE id = 1").
		Scan(&administrator.Username, &administrator.Salt, &administrator.Hash)
	return administrator, err
}

func (a *app) setupRequired() (bool, error) {
	var exists int
	err := a.db.QueryRow("SELECT EXISTS(SELECT 1 FROM administrators WHERE id = 1)").Scan(&exists)
	return exists == 0, err
}

func (a *app) newSession(username string, now time.Time) (string, *sessionInfo) {
	id := randomToken(32)
	session := &sessionInfo{CSRF: randomToken(24), Expires: now.Add(sessionTTL), User: username}
	return id, session
}

func (a *app) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name: "hostdesk_session", Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode,
	})
}

func (a *app) handleSetupState(w http.ResponseWriter, _ *http.Request) {
	required, err := a.setupRequired()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"required": required})
}

func (a *app) handleSetup(w http.ResponseWriter, r *http.Request) {
	required, err := a.setupRequired()
	if err != nil {
		writeError(w, err)
		return
	}
	if !required {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "管理员账号已经初始化"})
		return
	}
	clearDeadline := limitRequestReadTime(w, authRequestTimeout)
	defer clearDeadline()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if !administratorUsernamePattern.MatchString(body.Username) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号需为 3-32 位字母、数字、点、横线或下划线"})
		return
	}
	if len(body.Password) < 12 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码至少需要 12 个字符"})
		return
	}
	if len(body.Password) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码过长"})
		return
	}
	salt := randomToken(16)
	hash, err := derivePassword(body.Password, salt)
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	timestamp := now.UTC().Format(time.RFC3339Nano)
	result, err := a.db.Exec(`INSERT OR IGNORE INTO administrators (id, username, salt, password_hash, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?)`,
		body.Username, salt, hash, timestamp, timestamp)
	if err != nil {
		writeError(w, err)
		return
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		writeError(w, err)
		return
	}
	if inserted != 1 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "管理员账号已经初始化"})
		return
	}
	id, session := a.newSession(body.Username, now)
	a.mu.Lock()
	a.sessions[id] = session
	a.mu.Unlock()
	a.setSessionCookie(w, id)
	log.Printf("security administrator_initialized ip=%q", requestClientIP(r))
	writeJSON(w, http.StatusCreated, map[string]string{"csrf": session.CSRF, "user": session.User, "fileRoot": a.rootReal})
}

type loginProtectionIdentity struct {
	Scope    string
	Identity string
}

func requestClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote != nil && remote.IsLoopback() {
		if forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); forwarded != nil {
			return forwarded.String()
		}
	}
	if remote == nil {
		return "unknown"
	}
	return remote.String()
}

func loginProtectionIdentities(ip, username string, validUser bool) []loginProtectionIdentity {
	identities := []loginProtectionIdentity{{Scope: "ip", Identity: ip}}
	if validUser {
		identities = append(identities, loginProtectionIdentity{Scope: "account", Identity: strings.ToLower(username)})
	}
	return identities
}

func (a *app) loginLockRemaining(identities []loginProtectionIdentity, now time.Time) (time.Duration, error) {
	var longest time.Duration
	for _, identity := range identities {
		var lockedUntil int64
		err := a.db.QueryRow("SELECT locked_until FROM login_attempts WHERE scope = ? AND identity = ?", identity.Scope, identity.Identity).Scan(&lockedUntil)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if remaining := time.Unix(lockedUntil, 0).Sub(now); remaining > longest {
			longest = remaining
		}
	}
	return longest, nil
}

func loginLockDuration(scope string, failures int) time.Duration {
	if scope == "account" {
		switch {
		case failures >= 50:
			return 4 * time.Hour
		case failures >= 30:
			return time.Hour
		case failures >= 20:
			return 15 * time.Minute
		}
		return 0
	}
	switch {
	case failures >= 20:
		return time.Hour
	case failures >= 12:
		return 15 * time.Minute
	case failures >= 8:
		return 5 * time.Minute
	case failures >= 5:
		return time.Minute
	default:
		return 0
	}
}

func (a *app) recordLoginFailure(identities []loginProtectionIdentity, now time.Time) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowUnix := now.Unix()
	for _, identity := range identities {
		var failures int
		var firstFailed int64
		err := tx.QueryRow("SELECT failures, first_failed_at FROM login_attempts WHERE scope = ? AND identity = ?", identity.Scope, identity.Identity).Scan(&failures, &firstFailed)
		if errors.Is(err, sql.ErrNoRows) || nowUnix-firstFailed > int64((24*time.Hour)/time.Second) {
			failures = 0
			firstFailed = nowUnix
		} else if err != nil {
			return err
		}
		failures++
		lockedUntil := now.Add(loginLockDuration(identity.Scope, failures)).Unix()
		if _, err := tx.Exec(`INSERT INTO login_attempts (scope, identity, failures, first_failed_at, locked_until, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(scope, identity) DO UPDATE SET failures = excluded.failures, first_failed_at = excluded.first_failed_at,
			locked_until = excluded.locked_until, updated_at = excluded.updated_at`,
			identity.Scope, identity.Identity, failures, firstFailed, lockedUntil, nowUnix); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM login_attempts WHERE updated_at < ?", now.Add(-30*24*time.Hour).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) clearLoginFailures(identities []loginProtectionIdentity) error {
	for _, identity := range identities {
		if _, err := a.db.Exec("DELETE FROM login_attempts WHERE scope = ? AND identity = ?", identity.Scope, identity.Identity); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	administrator, err := a.administrator()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": administrator.Username})
}

func (a *app) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Username        string `json:"username"`
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if !administratorUsernamePattern.MatchString(body.Username) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号需为 3-32 位字母、数字、点、横线或下划线"})
		return
	}
	if len(body.CurrentPassword) == 0 || len(body.CurrentPassword) > 1024 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "当前密码错误"})
		return
	}
	if body.NewPassword != "" && (len(body.NewPassword) < 12 || len(body.NewPassword) > 1024) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码需要 12-1024 个字符"})
		return
	}
	administrator, err := a.administrator()
	if err != nil {
		writeError(w, err)
		return
	}
	candidate, err := derivePassword(body.CurrentPassword, administrator.Salt)
	if err != nil || subtle.ConstantTimeCompare([]byte(candidate), []byte(administrator.Hash)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "当前密码错误"})
		return
	}
	if body.Username == administrator.Username && body.NewPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户名和密码均未修改"})
		return
	}
	salt, hash := administrator.Salt, administrator.Hash
	if body.NewPassword != "" {
		salt = randomToken(16)
		hash, err = derivePassword(body.NewPassword, salt)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	result, err := a.db.Exec(`UPDATE administrators SET username = ?, salt = ?, password_hash = ?, updated_at = ?
		WHERE id = 1 AND password_hash = ?`, body.Username, salt, hash, time.Now().UTC().Format(time.RFC3339Nano), administrator.Hash)
	if err != nil {
		writeError(w, err)
		return
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "账号已发生变化，请重新登录"})
		return
	}
	id, session := a.newSession(body.Username, time.Now())
	a.mu.Lock()
	a.sessions = map[string]*sessionInfo{id: session}
	a.mu.Unlock()
	a.setSessionCookie(w, id)
	_ = a.clearLoginFailures([]loginProtectionIdentity{{Scope: "account", Identity: strings.ToLower(administrator.Username)}, {Scope: "account", Identity: strings.ToLower(body.Username)}})
	writeJSON(w, http.StatusOK, map[string]string{"csrf": session.CSRF, "user": session.User, "fileRoot": a.rootReal})
}
