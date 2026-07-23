package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

const (
	settingACMEEmail       = "acme.default_email"
	settingCloudflareToken = "dns.cloudflare.token"
)

type dnsSettingsView struct {
	DefaultEmail         string `json:"defaultEmail"`
	CloudflareConfigured bool   `json:"cloudflareConfigured"`
}

type dnsSettingsRequest struct {
	DefaultEmail    string `json:"defaultEmail"`
	CloudflareToken string `json:"cloudflareToken"`
	ClearCloudflare bool   `json:"clearCloudflare"`
}

type dnsProviderCredentials struct {
	Token string
}

func (a *app) appSetting(key string) (string, error) {
	var value string
	err := a.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (a *app) encryptedAppSetting(key string) (string, error) {
	value, err := a.appSetting(key)
	if err != nil || value == "" {
		return "", err
	}
	return a.decryptCredential(tokenString(value))
}

func validateDefaultEmail(value string) error {
	if value == "" {
		return nil
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return &apiError{Status: http.StatusBadRequest, Message: "默认证书邮箱格式无效"}
	}
	return nil
}

func (a *app) dnsSettingsView() (dnsSettingsView, error) {
	view := dnsSettingsView{}
	var err error
	view.DefaultEmail, err = a.appSetting(settingACMEEmail)
	if err != nil {
		return view, err
	}
	cloudflare, err := a.appSetting(settingCloudflareToken)
	if err != nil {
		return view, err
	}
	view.CloudflareConfigured = cloudflare != ""
	return view, nil
}

func (a *app) saveDNSSettings(request dnsSettingsRequest) error {
	request.DefaultEmail = strings.TrimSpace(request.DefaultEmail)
	request.CloudflareToken = strings.TrimSpace(request.CloudflareToken)
	if err := validateDefaultEmail(request.DefaultEmail); err != nil {
		return err
	}
	if request.CloudflareToken != "" && (len(request.CloudflareToken) < 20 || len(request.CloudflareToken) > 512) {
		return &apiError{Status: http.StatusBadRequest, Message: "Cloudflare API Token 长度无效"}
	}
	if request.ClearCloudflare && request.CloudflareToken != "" {
		return &apiError{Status: http.StatusBadRequest, Message: "Cloudflare 凭据不能同时更新和清除"}
	}

	updates := map[string]string{settingACMEEmail: request.DefaultEmail}
	for key, value := range map[string]string{settingCloudflareToken: request.CloudflareToken} {
		if value == "" {
			continue
		}
		encrypted, err := a.encryptCredential(value)
		if err != nil {
			return err
		}
		updates[key] = string(encrypted)
	}
	deletes := []string{}
	if request.ClearCloudflare {
		deletes = append(deletes, settingCloudflareToken)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, value := range updates {
		if _, err := tx.Exec(`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now); err != nil {
			return err
		}
	}
	for _, key := range deletes {
		if _, err := tx.Exec("DELETE FROM app_settings WHERE key = ?", key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) dnsProviderCredentials(provider string, legacy tokenString) (dnsProviderCredentials, error) {
	var credentials dnsProviderCredentials
	var err error
	switch provider {
	case "cloudflare":
		credentials.Token, err = a.encryptedAppSetting(settingCloudflareToken)
		if err == nil && credentials.Token == "" && legacy != "" {
			credentials.Token, err = a.decryptCredential(legacy)
		}
	default:
		err = &apiError{Status: http.StatusBadRequest, Message: "DNS 服务商无效"}
	}
	if err != nil {
		return credentials, err
	}
	if credentials.Token == "" {
		return credentials, &apiError{Status: http.StatusConflict, Message: "请先保存所选 DNS 服务商的 API 凭据"}
	}
	return credentials, nil
}

func (a *app) handleDNSSettingsGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	view, err := a.dnsSettingsView()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *app) handleDNSSettingsPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var request dnsSettingsRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := a.saveDNSSettings(request); err != nil {
		writeError(w, err)
		return
	}
	view, err := a.dnsSettingsView()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
