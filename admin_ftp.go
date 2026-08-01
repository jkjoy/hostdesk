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
	ftpGroup         = "hostdesk-ftp"
	ftpRoot          = "/srv/ftp"
	vsftpdConfig     = "/etc/vsftpd/vsftpd.conf"
	vsftpdPAMConfig  = "/etc/pam.d/vsftpd"
	vsftpdUserConfig = "/etc/vsftpd/users"
)

var ftpUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{2,31}$`)

type ftpUserView struct {
	Username      string    `json:"username"`
	Home          string    `json:"home"`
	SiteID        string    `json:"siteId"`
	SiteDomain    string    `json:"siteDomain"`
	CreatedAt     time.Time `json:"createdAt"`
	SystemPresent bool      `json:"systemPresent"`
}

type ftpSiteOption struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Root   string `json:"root"`
}

func renderVSFTPDConfig() string {
	return `listen=YES
listen_ipv6=NO
anonymous_enable=NO
local_enable=YES
write_enable=YES
local_umask=002
dirmessage_enable=YES
xferlog_enable=YES
connect_from_port_20=YES
chroot_local_user=YES
allow_writeable_chroot=YES
check_shell=NO
seccomp_sandbox=NO
secure_chroot_dir=/var/empty
pam_service_name=vsftpd
user_sub_token=$USER
local_root=/srv/ftp/$USER
user_config_dir=/etc/vsftpd/users
pasv_enable=YES
pasv_min_port=40000
pasv_max_port=40100
`
}

func renderVSFTPDPAMConfig() string {
	return `#%PAM-1.0
auth requisite pam_succeed_if.so user ingroup hostdesk-ftp
auth include base-auth
account include base-account
session include base-session-noninteractive
`
}

func ensureVSFTPDConfig() error {
	if err := os.MkdirAll(ftpRoot, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(vsftpdUserConfig, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll("/var/empty", 0555); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(vsftpdPAMConfig), 0755); err != nil {
		return err
	}
	if err := writeAtomic(vsftpdPAMConfig, []byte(renderVSFTPDPAMConfig()), 0644); err != nil {
		return err
	}
	config := []byte(renderVSFTPDConfig())
	current, err := os.ReadFile(vsftpdConfig)
	if err == nil && string(current) == string(config) {
		return nil
	}
	if err := writeAtomic(vsftpdConfig, config, 0600); err != nil {
		return err
	}
	if serviceRunning("vsftpd") {
		_, err := runAdmin(time.Minute, "rc-service", "vsftpd", "restart")
		return err
	}
	return nil
}

func availableFTPSites(sites []siteDefinition) []ftpSiteOption {
	options := make([]ftpSiteOption, 0, len(sites))
	for _, site := range sites {
		root := filepath.Clean(site.Root)
		if (site.Type != "static" && site.Type != "php") || !filepath.IsAbs(root) || !inside(webRootDir, root) || root == webRootDir || strings.ContainsAny(root, "\r\n\x00") {
			continue
		}
		options = append(options, ftpSiteOption{ID: site.ID, Domain: site.Domain, Root: root})
	}
	return options
}

func resolveFTPSite(sites []siteDefinition, siteID string) (ftpSiteOption, error) {
	siteID = strings.TrimSpace(siteID)
	for _, site := range availableFTPSites(sites) {
		if site.ID == siteID {
			return site, nil
		}
	}
	return ftpSiteOption{}, &apiError{Status: http.StatusBadRequest, Message: "请选择有效的网站目录"}
}

func ftpUserConfigPath(username string) string {
	return filepath.Join(vsftpdUserConfig, username)
}

func writeFTPUserConfig(username, root string) error {
	return writeAtomic(ftpUserConfigPath(username), []byte("local_root="+root+"\n"), 0600)
}

func ensureFTPSiteAccess(root string) error {
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	if _, err := runAdmin(time.Minute, "chgrp", "-R", ftpGroup, root); err != nil {
		return err
	}
	if _, err := runAdmin(time.Minute, "chmod", "-R", "g+rwX", root); err != nil {
		return err
	}
	_, err := runAdmin(time.Minute, "chmod", "g+s", root)
	return err
}

func (a *app) migrateLegacyFTPBindings() error {
	users, err := a.ftpUsers()
	if err != nil {
		return err
	}
	legacy := make([]ftpUserView, 0, len(users))
	for _, user := range users {
		if user.SiteID == "" && user.SystemPresent {
			legacy = append(legacy, user)
		}
	}
	if len(legacy) == 0 || !packageInstalled("vsftpd") {
		return nil
	}
	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	options := availableFTPSites(sites)
	if len(options) != 1 {
		return nil
	}
	site := options[0]
	if !systemGroupExists(ftpGroup) {
		if _, err := runAdmin(time.Minute, "addgroup", "-S", ftpGroup); err != nil {
			return err
		}
	}
	if err := ensureVSFTPDConfig(); err != nil {
		return err
	}
	if err := ensureFTPSiteAccess(site.Root); err != nil {
		return err
	}
	for _, user := range legacy {
		snapshot, err := captureFile(ftpUserConfigPath(user.Username))
		if err != nil {
			return err
		}
		if err := writeFTPUserConfig(user.Username, site.Root); err != nil {
			return err
		}
		_, err = a.db.Exec("UPDATE ftp_users SET home = ?, site_id = ?, updated_at = ? WHERE username = ?", site.Root, site.ID, time.Now().UTC().Format(time.RFC3339Nano), user.Username)
		if err != nil {
			restoreFiles(snapshot)
			return err
		}
	}
	return nil
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
	rows, err := a.db.Query("SELECT username, home, site_id, created_at FROM ftp_users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []ftpUserView{}
	for rows.Next() {
		var user ftpUserView
		var created string
		if err := rows.Scan(&user.Username, &user.Home, &user.SiteID, &created); err != nil {
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
	err := a.db.QueryRow("SELECT username, home, site_id, created_at FROM ftp_users WHERE username = ?", username).
		Scan(&user.Username, &user.Home, &user.SiteID, &created)
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
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	options := availableFTPSites(sites)
	domains := make(map[string]string, len(options))
	for _, site := range options {
		domains[site.ID] = site.Domain
	}
	for index := range users {
		users[index].SiteDomain = domains[users[index].SiteID]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       componentStatus("ftp", definition),
		"users":        users,
		"sites":        options,
		"root":         webRootDir,
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
		SiteID   string `json:"siteId"`
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
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	site, err := resolveFTPSite(sites, request.SiteID)
	if err != nil {
		writeError(w, err)
		return
	}
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
	if err := ensureFTPSiteAccess(site.Root); err != nil {
		writeError(w, err)
		return
	}
	accountHome := filepath.Join(ftpRoot, request.Username)
	if _, err := runAdmin(time.Minute, "adduser", "-D", "-h", accountHome, "-s", "/sbin/nologin", "-G", ftpGroup, request.Username); err != nil {
		writeError(w, err)
		return
	}
	if _, err := runAdminInput(time.Minute, request.Username+":"+request.Password+"\n", "chpasswd"); err != nil {
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	if err := writeFTPUserConfig(request.Username, site.Root); err != nil {
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	now := time.Now().UTC()
	_, err = a.db.Exec("INSERT INTO ftp_users (username, home, site_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)", request.Username, site.Root, site.ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		_ = os.Remove(ftpUserConfigPath(request.Username))
		_, _ = runAdmin(time.Minute, "deluser", request.Username)
		writeError(w, err)
		return
	}
	user, _ := a.ftpUser(request.Username)
	user.SiteDomain = site.Domain
	writeJSON(w, http.StatusCreated, user)
}

func (a *app) handleFTPUserUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	username := r.PathValue("username")
	var request struct {
		Password string `json:"password"`
		SiteID   string `json:"siteId"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if request.Password != "" {
		if err := validateFTPPassword(request.Password); err != nil {
			writeError(w, err)
			return
		}
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	site, err := resolveFTPSite(sites, request.SiteID)
	if err != nil {
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
	if err := ensureFTPSiteAccess(site.Root); err != nil {
		writeError(w, err)
		return
	}
	if request.Password != "" {
		if _, err := runAdminInput(time.Minute, username+":"+request.Password+"\n", "chpasswd"); err != nil {
			writeError(w, err)
			return
		}
	}
	configSnapshot, err := captureFile(ftpUserConfigPath(username))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := writeFTPUserConfig(username, site.Root); err != nil {
		writeError(w, err)
		return
	}
	_, err = a.db.Exec("UPDATE ftp_users SET home = ?, site_id = ?, updated_at = ? WHERE username = ?", site.Root, site.ID, time.Now().UTC().Format(time.RFC3339Nano), username)
	if err != nil {
		restoreFiles(configSnapshot)
		writeError(w, err)
		return
	}
	updated, _ := a.ftpUser(username)
	updated.SiteDomain = site.Domain
	writeJSON(w, http.StatusOK, updated)
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
	if err := os.Remove(ftpUserConfigPath(username)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, err)
		return
	}
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
