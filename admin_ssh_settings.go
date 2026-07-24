package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	settingSSHHost     = "ssh.host"
	settingSSHPort     = "ssh.port"
	settingSSHUsername = "ssh.username"
	settingSSHPassword = "ssh.password"
)

type sshSettingsRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type sshSettingsView struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	PasswordConfigured bool   `json:"passwordConfigured"`
}

type sshSavedCredentials struct {
	sshSettingsView
	Password string
}

func (a *app) validateSSHIdentity(host string, port int, username string) error {
	host = strings.TrimSpace(host)
	username = strings.TrimSpace(username)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n") || (!a.allowedSSH["*"] && !a.allowedSSH[host]) {
		return &apiError{Status: http.StatusBadRequest, Message: "SSH 主机不在允许范围内"}
	}
	if port < 1 || port > 65535 {
		return &apiError{Status: http.StatusBadRequest, Message: "SSH 端口无效"}
	}
	if username == "" || len(username) > 64 || strings.ContainsAny(username, "\r\n") {
		return &apiError{Status: http.StatusBadRequest, Message: "SSH 用户名无效"}
	}
	return nil
}

func (a *app) sshSettings() (sshSettingsView, error) {
	view := sshSettingsView{Port: 22}
	var err error
	if view.Host, err = a.appSetting(settingSSHHost); err != nil {
		return view, err
	}
	port, err := a.appSetting(settingSSHPort)
	if err != nil {
		return view, err
	}
	if port != "" {
		if parsed, parseErr := strconv.Atoi(port); parseErr == nil {
			view.Port = parsed
		}
	}
	if view.Username, err = a.appSetting(settingSSHUsername); err != nil {
		return view, err
	}
	password, err := a.appSetting(settingSSHPassword)
	if err != nil {
		return view, err
	}
	view.PasswordConfigured = password != ""
	return view, nil
}

func (a *app) savedSSHCredentials() (sshSavedCredentials, error) {
	view, err := a.sshSettings()
	if err != nil {
		return sshSavedCredentials{}, err
	}
	credentials := sshSavedCredentials{sshSettingsView: view}
	if view.PasswordConfigured {
		credentials.Password, err = a.encryptedAppSetting(settingSSHPassword)
	}
	return credentials, err
}

func (a *app) saveSSHSettings(request sshSettingsRequest) (sshSettingsView, error) {
	request.Host = strings.TrimSpace(request.Host)
	request.Username = strings.TrimSpace(request.Username)
	if err := a.validateSSHIdentity(request.Host, request.Port, request.Username); err != nil {
		return sshSettingsView{}, err
	}
	if len(request.Password) > 4096 {
		return sshSettingsView{}, &apiError{Status: http.StatusBadRequest, Message: "SSH 密码过长"}
	}
	updates := map[string]string{
		settingSSHHost: request.Host, settingSSHPort: strconv.Itoa(request.Port), settingSSHUsername: request.Username,
	}
	if request.Password != "" {
		encrypted, err := a.encryptCredential(request.Password)
		if err != nil {
			return sshSettingsView{}, err
		}
		updates[settingSSHPassword] = string(encrypted)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return sshSettingsView{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, value := range updates {
		if _, err := tx.Exec(`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now); err != nil {
			return sshSettingsView{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return sshSettingsView{}, err
	}
	return a.sshSettings()
}

func (a *app) clearSSHSettings() error {
	_, err := a.db.Exec("DELETE FROM app_settings WHERE key IN (?, ?, ?, ?)", settingSSHHost, settingSSHPort, settingSSHUsername, settingSSHPassword)
	return err
}

func (a *app) handleSSHSettingsGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	view, err := a.sshSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *app) handleSSHSettingsPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var request sshSettingsRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	view, err := a.saveSSHSettings(request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *app) handleSSHSettingsDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if err := a.clearSSHSettings(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sshSettingsView{Port: 22})
}
