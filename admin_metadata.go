package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var version = "v2.0.0"

type updateStatus struct {
	CurrentVersion  string    `json:"currentVersion"`
	LatestVersion   string    `json:"latestVersion,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	ReleaseURL      string    `json:"releaseUrl,omitempty"`
	PublishedAt     time.Time `json:"publishedAt,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
	Error           string    `json:"error,omitempty"`
}

func numericVersion(value string) []int {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value = strings.SplitN(value, "-", 2)[0]
	parts := strings.Split(value, ".")
	result := make([]int, 3)
	for index := 0; index < len(parts) && index < len(result); index++ {
		result[index], _ = strconv.Atoi(parts[index])
	}
	return result
}

func versionLess(current, latest string) bool {
	if current == "" || current == "dev" || latest == "" {
		return false
	}
	currentParts := numericVersion(current)
	latestParts := numericVersion(latest)
	for index := range currentParts {
		if currentParts[index] != latestParts[index] {
			return currentParts[index] < latestParts[index]
		}
	}
	return false
}

func metadataClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func fetchPublicIPAddress() string {
	for _, endpoint := range []string{"https://api.ipify.org", "https://ipv4.icanhazip.com"} {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		request.Header.Set("User-Agent", "HostDesk/"+version)
		response, err := metadataClient(4 * time.Second).Do(request)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			continue
		}
		address := strings.TrimSpace(string(data))
		if parsed := net.ParseIP(address); parsed != nil && parsed.To4() != nil {
			return parsed.String()
		}
	}
	return ""
}

func (a *app) publicIPAddress() string {
	a.publicIPMu.Lock()
	defer a.publicIPMu.Unlock()
	if time.Now().Before(a.publicIPExpires) {
		return a.publicIP
	}
	a.publicIP = fetchPublicIPAddress()
	a.publicIPExpires = time.Now().Add(10 * time.Minute)
	return a.publicIP
}

func fetchLatestRelease() updateStatus {
	status := updateStatus{CurrentVersion: version, CheckedAt: time.Now()}
	request, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/jkjoy/hostdesk/releases/latest", nil)
	if err != nil {
		status.Error = "无法创建版本检查请求"
		return status
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "HostDesk/"+version)
	response, err := metadataClient(6 * time.Second).Do(request)
	if err != nil {
		status.Error = "无法连接 GitHub"
		return status
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status.Error = "GitHub 版本检查返回 " + response.Status
		return status
	}
	var release struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		status.Error = "GitHub 版本信息格式无效"
		return status
	}
	status.LatestVersion = release.TagName
	status.ReleaseURL = release.HTMLURL
	status.PublishedAt = release.PublishedAt
	status.UpdateAvailable = versionLess(version, release.TagName)
	return status
}

func (a *app) updateStatus(force bool) updateStatus {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	if !force && time.Now().Before(a.updateExpires) {
		return a.updateCache
	}
	a.updateCache = fetchLatestRelease()
	ttl := 30 * time.Minute
	if a.updateCache.Error != "" {
		ttl = 5 * time.Minute
	}
	a.updateExpires = time.Now().Add(ttl)
	return a.updateCache
}

func (a *app) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	writeJSON(w, http.StatusOK, a.updateStatus(r.URL.Query().Get("refresh") == "1"))
}
