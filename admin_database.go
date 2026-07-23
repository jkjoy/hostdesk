package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

var databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
var databaseUserPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

var allowedCharsets = map[string]bool{"utf8mb4": true, "utf8": true, "latin1": true}
var allowedPrivileges = map[string]bool{
	"ALL": true, "SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "INDEX": true, "ALTER": true, "REFERENCES": true,
	"CREATE VIEW": true, "SHOW VIEW": true, "TRIGGER": true,
}

type databaseInfo struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Tables int    `json:"tables"`
}

type databaseUserInfo struct {
	User   string `json:"user"`
	Host   string `json:"host"`
	Plugin string `json:"plugin"`
}

type databaseUserRequest struct {
	User       string   `json:"user"`
	Host       string   `json:"host"`
	Password   string   `json:"password"`
	Database   string   `json:"database"`
	Privileges []string `json:"privileges"`
}

func openMariaDB() (*sql.DB, error) {
	if !packageInstalled("mariadb") {
		return nil, &apiError{http.StatusConflict, "请先安装 MariaDB/MySQL"}
	}
	sockets := []string{"/run/mysqld/mysqld.sock", "/var/run/mysqld/mysqld.sock", "/run/mariadb/mariadb.sock"}
	var lastErr error
	for _, socket := range sockets {
		if _, err := os.Stat(socket); err != nil {
			continue
		}
		dsn := fmt.Sprintf("root@unix(%s)/?parseTime=true&interpolateParams=true&timeout=5s", socket)
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			lastErr = err
			continue
		}
		db.SetConnMaxLifetime(2 * time.Minute)
		db.SetMaxOpenConns(4)
		if err := db.Ping(); err == nil {
			return db, nil
		} else {
			lastErr = err
		}
		db.Close()
	}
	if lastErr == nil {
		lastErr = errors.New("MariaDB 服务未运行或套接字不存在")
	}
	return nil, &apiError{http.StatusServiceUnavailable, lastErr.Error()}
}

func protectedDatabase(name string) bool {
	switch strings.ToLower(name) {
	case "information_schema", "mysql", "performance_schema", "sys":
		return true
	}
	return false
}

func protectedDatabaseUser(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "root", "mariadb.sys", "mysql", "public":
		return true
	}
	return false
}

func quoteIdentifier(name string) (string, error) {
	if !databaseNamePattern.MatchString(name) {
		return "", &apiError{http.StatusBadRequest, "数据库名称只能包含字母、数字和下划线"}
	}
	return "`" + name + "`", nil
}

func validateDatabaseHost(host string) bool {
	if host == "%" || host == "localhost" {
		return true
	}
	if len(host) < 1 || len(host) > 255 || strings.ContainsAny(host, "'\"`;/\\ \t\r\n") {
		return false
	}
	return true
}

func normalizePrivileges(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"ALL"}, nil
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToUpper(strings.TrimSpace(value))
		if !allowedPrivileges[value] {
			return nil, &apiError{http.StatusBadRequest, "包含不支持的数据库权限"}
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	if seen["ALL"] {
		return []string{"ALL PRIVILEGES"}, nil
	}
	sort.Strings(result)
	return result, nil
}

func validateDatabaseUser(body *databaseUserRequest) error {
	body.User = strings.TrimSpace(body.User)
	body.Host = strings.TrimSpace(body.Host)
	if body.Host == "" {
		body.Host = "localhost"
	}
	if !databaseUserPattern.MatchString(body.User) || protectedDatabaseUser(body.User) {
		return &apiError{http.StatusBadRequest, "数据库用户名无效或受保护"}
	}
	if !validateDatabaseHost(body.Host) {
		return &apiError{http.StatusBadRequest, "数据库用户主机范围无效"}
	}
	if _, err := quoteIdentifier(body.Database); err != nil {
		return err
	}
	if len(body.Password) < 12 || len(body.Password) > 256 {
		return &apiError{http.StatusBadRequest, "数据库用户密码至少需要 12 个字符"}
	}
	_, err := normalizePrivileges(body.Privileges)
	return err
}

func (a *app) handleDatabasesList(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	db, err := openMariaDB()
	if err != nil {
		writeError(w, err)
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT s.SCHEMA_NAME,
        COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0), COUNT(t.TABLE_NAME)
        FROM information_schema.SCHEMATA s
        LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
        GROUP BY s.SCHEMA_NAME ORDER BY s.SCHEMA_NAME`)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	databases := make([]databaseInfo, 0)
	for rows.Next() {
		var item databaseInfo
		if err := rows.Scan(&item.Name, &item.Size, &item.Tables); err == nil && !protectedDatabase(item.Name) {
			databases = append(databases, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": databases, "running": true})
}

func (a *app) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body struct {
		Name    string `json:"name"`
		Charset string `json:"charset"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	identifier, err := quoteIdentifier(body.Name)
	body.Charset = strings.ToLower(strings.TrimSpace(body.Charset))
	if body.Charset == "" {
		body.Charset = "utf8mb4"
	}
	if err != nil || !allowedCharsets[body.Charset] || protectedDatabase(body.Name) {
		writeError(w, &apiError{http.StatusBadRequest, "数据库名称或字符集无效"})
		return
	}
	db, err := openMariaDB()
	if err == nil {
		defer db.Close()
		_, err = db.Exec("CREATE DATABASE " + identifier + " CHARACTER SET " + body.Charset + " COLLATE " + body.Charset + "_general_ci")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (a *app) handleDatabaseDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	name := r.PathValue("database")
	identifier, err := quoteIdentifier(name)
	if err != nil || protectedDatabase(name) {
		writeError(w, &apiError{http.StatusBadRequest, "不能删除该数据库"})
		return
	}
	db, err := openMariaDB()
	if err == nil {
		defer db.Close()
		_, err = db.Exec("DROP DATABASE " + identifier)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleDatabaseUsersList(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	db, err := openMariaDB()
	if err != nil {
		writeError(w, err)
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT User, Host, plugin FROM mysql.user WHERE User <> '' AND UPPER(User) NOT IN ('ROOT','MARIADB.SYS','MYSQL','PUBLIC') ORDER BY User, Host`)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	users := make([]databaseUserInfo, 0)
	for rows.Next() {
		var item databaseUserInfo
		if err := rows.Scan(&item.User, &item.Host, &item.Plugin); err == nil {
			users = append(users, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func grantDatabaseUser(db *sql.DB, body databaseUserRequest, revoke bool) error {
	identifier, err := quoteIdentifier(body.Database)
	if err != nil {
		return err
	}
	privileges, err := normalizePrivileges(body.Privileges)
	if err != nil {
		return err
	}
	if revoke {
		if _, err := db.Exec("REVOKE ALL PRIVILEGES, GRANT OPTION FROM ?@?", body.User, body.Host); err != nil {
			return err
		}
	}
	_, err = db.Exec("GRANT "+strings.Join(privileges, ", ")+" ON "+identifier+".* TO ?@?", body.User, body.Host)
	return err
}

func (a *app) handleDatabaseUserCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body databaseUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if err := validateDatabaseUser(&body); err != nil {
		writeError(w, err)
		return
	}
	db, err := openMariaDB()
	if err == nil {
		defer db.Close()
		_, err = db.Exec("CREATE USER ?@? IDENTIFIED BY ?", body.User, body.Host, body.Password)
	}
	if err == nil {
		err = grantDatabaseUser(db, body, false)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (a *app) handleDatabaseUserUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body databaseUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	body.User = r.PathValue("user")
	if err := validateDatabaseUser(&body); err != nil {
		writeError(w, err)
		return
	}
	db, err := openMariaDB()
	if err == nil {
		defer db.Close()
		_, err = db.Exec("ALTER USER ?@? IDENTIFIED BY ?", body.User, body.Host, body.Password)
	}
	if err == nil {
		err = grantDatabaseUser(db, body, true)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleDatabaseUserDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	user := r.PathValue("user")
	host := r.URL.Query().Get("host")
	if !databaseUserPattern.MatchString(user) || protectedDatabaseUser(user) || !validateDatabaseHost(host) {
		writeError(w, &apiError{http.StatusBadRequest, "数据库用户无效或受保护"})
		return
	}
	db, err := openMariaDB()
	if err == nil {
		defer db.Close()
		_, err = db.Exec("DROP USER ?@?", user, host)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
