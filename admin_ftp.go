package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ftpGroup     = "hostdesk-ftp"
	ftpRoot      = "/srv/ftp"
	vsftpdConfig = "/etc/vsftpd/vsftpd.conf"
)

var ftpUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{2,31}$`)

type ftpUserView struct {
	Username      string    `json:"username"`
	Home          string    `json:"home"`
	CreatedAt     time.Time `json:"createdAt"`
	SystemPresent bool      `json:"systemPresent"`
}

func renderVSFTPDConfig() string {
	return `listen=YES
listen_ipv6=NO
anonymous_enable=NO
local_enable=YES
write_enable=YES
local_umask=022
dirmessage_enable=YES
xferlog_enable=YES
connect_from_port_20=YES
chroot_local_user=YES
allow_writeable_chroot=YES
check_shell=NO
secure_chroot_dir=/var/empty
pam_service_name=vsftpd
user_sub_token=$USER
local_root=/srv/ftp/$USER
pasv_enable=YES
pasv_min_port=40000
pasv_max_port=40100
`
}

func ensureVSFTPDConfig() error {
	if err := os.MkdirAll(ftpRoot, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll("/var/empty", 0555); err != nil {
		return err
	}
	return writeAtomic(vsftpdConfig, []byte(renderVSFTPDConfig()), 0600)
}

func systemUserExists(username string) bool {
	return exec.Command("id", "-u", username).Run() == nil
}

func systemGroupExists(group string) bool {
	data, err := os.ReadFile("/etc/group")
	if err != nil {
		return false
	}
	prefix := group + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func validateFTPPassword(password string) error {
	if len(password) < 12 || len(password) > 1024 {
		return &apiError{Status: http.StatusBadRequest, Message: "FTP 密码需要 12-1024 个字符"}
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return &apiError{Status: http.StatusBadRequest, Message: "FTP 密码不能包含换行或空字符"}
	}
	return nil
}

func (a *app) ftpUsers() ([]ftpUserView, error) {
	rows, err := a.db.Query("SELECT username, home, created_at FROM ftp_users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []ftpUserView{}
	for rows.Next() {
		var user ftpUserView
		var created string
		if err := rows.Scan(&user.Username, &user.Home, &created); err != nil {
			return nil, err
		}
		user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		user.SystemPresent = systemUserExists(user.Username)
		users = append(users, user)
	}
	return users, rows.Err()
}

func (a *app) ftpUser(username string) (ftpUserView, error) {
	var user ftpUserView
	var created string
	err := a.db.QueryRow("SELECT username, home, created_at FROM ftp_users WHERE username = ?", username).
		Scan(&user.Username, &user.Home, &created)
	if err != nil {
		return user, err
	}
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	user.SystemPresent = systemUserExists(user.Username)
	return user, nil
}

func (a *app) handleFTPGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	definition := components()["ftp"]
	users, err := a.ftpUsers()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       componentStatus("ftp", definition),
		"users":        users,
		"root":         ftpRoot,
		"passivePorts": "40000-40100",
	})
}

func (a *app) handleFTPUserCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	request.Username = strings.TrimSpace(request.Username)
	if !ftpUsernamePattern.MatchString(request.Username) {
		writeError(w, &apiError{Status: http.StatusBadRequest, Message: "FTP 用户名需为 3-32 位小写字母、数字、下划线或横线"})
		return
	}
	if err := validateFTPPassword(request.Password); err != nil {
		writeError(w, err)
		return
	}
	if !packageInstalled("vsftpd") {
		writeError(w, &apiError{Status: http.StatusConflict, Message: "请先安装 FTP 服务"})
		return
	}

	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if systemUserExists(request.Username) {
		writeError(w, &apiError{Status: http.StatusConflict, Message: "系统中已存在同名用户"})
		return
	}
	if err := ensureVSFTPDConfig(); err != nil {
		writeError(w, err)
		return
	}
	if !systemGroupExists(ftpGroup) {
		if _, err := runAdmin(time.Minute, "addgroup", "-S", ftpGroup); err != nil {
			writeError(w, err)
			return
		}
	}
	home := filepath.Join(ftpRoot, request.Username)
	if _, err := runAdmin(time.Minute, "adduser", "-D", "-h", home, "-s", "/sbin/nologin", "-G", ftpGroup, request.Username); err != nil {
		writeError(w, err)
		return
	}
	if _, err := runAdminInput(time.Minute, request.Username+":"+request.Password+"\n", "chpasswd"); err != nil {
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	if err := os.Chmod(home, 0750); err != nil {
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	now := time.Now().UTC()
	_, err := a.db.Exec("INSERT INTO ftp_users (username, home, created_at, updated_at) VALUES (?, ?, ?, ?)", request.Username, home, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	user, _ := a.ftpUser(request.Username)
	writeJSON(w, http.StatusCreated, user)
}

func (a *app) handleFTPUserUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	username := r.PathValue("username")
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := validateFTPPassword(request.Password); err != nil {
		writeError(w, err)
		return
	}
	user, err := a.ftpUser(username)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, &apiError{Status: http.StatusNotFound, Message: "FTP 用户不存在"})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if !user.SystemPresent {
		writeError(w, &apiError{Status: http.StatusConflict, Message: "FTP 系统用户已不存在"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if _, err := runAdminInput(time.Minute, username+":"+request.Password+"\n", "chpasswd"); err != nil {
		writeError(w, err)
		return
	}
	_, _ = a.db.Exec("UPDATE ftp_users SET updated_at = ? WHERE username = ?", time.Now().UTC().Format(time.RFC3339Nano), username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleFTPUserDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	username := r.PathValue("username")
	user, err := a.ftpUser(username)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, &apiError{Status: http.StatusNotFound, Message: "FTP 用户不存在"})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if user.SystemPresent {
		if _, err := runAdmin(time.Minute, "deluser", username); err != nil {
			writeError(w, err)
			return
		}
	}
	if _, err := a.db.Exec("DELETE FROM ftp_users WHERE username = ?", username); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "homeRetained": user.Home})
}
