package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "v2.1.9"

type updateStatus struct {
	CurrentVersion  string    `json:"currentVersion"`
	LatestVersion   string    `json:"latestVersion,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	ReleaseURL      string    `json:"releaseUrl,omitempty"`
	PublishedAt     time.Time `json:"publishedAt,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
	Error           string    `json:"error,omitempty"`
	AssetURL        string    `json:"-"`
	ChecksumsURL    string    `json:"-"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
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
		TagName     string         `json:"tag_name"`
		HTMLURL     string         `json:"html_url"`
		PublishedAt time.Time      `json:"published_at"`
		Assets      []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		status.Error = "GitHub 版本信息格式无效"
		return status
	}
	status.LatestVersion = release.TagName
	status.ReleaseURL = release.HTMLURL
	status.PublishedAt = release.PublishedAt
	status.UpdateAvailable = versionLess(version, release.TagName)
	assetName, _ := updateAssetName(runtime.GOARCH)
	for _, asset := range release.Assets {
		switch asset.Name {
		case assetName:
			status.AssetURL = asset.BrowserDownloadURL
		case "checksums.txt":
			status.ChecksumsURL = asset.BrowserDownloadURL
		}
	}
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

func updateAssetName(goarch string) (string, error) {
	suffix := goarch
	if goarch == "arm" {
		suffix = "armv7"
	}
	switch suffix {
	case "386", "amd64", "arm64", "armv7":
		return "hostdesk-linux-" + suffix, nil
	default:
		return "", fmt.Errorf("不支持当前 CPU 架构 %s", goarch)
	}
}

func downloadUpdateAsset(rawURL string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(rawURL, "https://github.com/jkjoy/hostdesk/releases/download/") {
		return nil, errors.New("Release 下载地址无效")
	}
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "HostDesk/"+version)
	client := metadataClient(5 * time.Minute)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载返回 %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("下载文件超过大小限制")
	}
	return data, nil
}

func checksumForAsset(data []byte, assetName string) ([sha256.Size]byte, error) {
	var expected [sha256.Size]byte
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return expected, errors.New("Release 校验值格式无效")
		}
		copy(expected[:], decoded)
		return expected, nil
	}
	return expected, errors.New("checksums.txt 缺少当前架构")
}

func verifyUpdateAsset(binary, checksums []byte, assetName string) error {
	expected, err := checksumForAsset(checksums, assetName)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(binary)
	if actual != expected {
		return errors.New("Release 二进制校验失败")
	}
	return nil
}

func installUpdateBinary(binary []byte) (string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("当前程序文件无效")
	}
	previous, err := os.ReadFile(executable)
	if err != nil {
		return "", "", err
	}
	backup := executable + ".previous"
	if err := writeAtomic(backup, previous, info.Mode().Perm()); err != nil {
		return "", "", fmt.Errorf("备份当前版本失败：%w", err)
	}
	if err := writeAtomic(executable, binary, info.Mode().Perm()); err != nil {
		return "", "", fmt.Errorf("替换程序失败：%w", err)
	}
	return executable, backup, nil
}

func restoreUpdateBinary(executable, backup string) {
	data, err := os.ReadFile(backup)
	if err != nil {
		return
	}
	info, err := os.Stat(backup)
	if err != nil {
		return
	}
	_ = writeAtomic(executable, data, info.Mode().Perm())
}

func (a *app) scheduleUpdateRestart(executable, backup string) error {
	const script = `sleep 2
if rc-service hostdesk restart; then
  sleep 2
  if rc-service hostdesk status; then exit 0; fi
fi
cp -p "$2" "$1.new" && mv -f "$1.new" "$1"
rc-service hostdesk restart
`
	command := exec.Command("/bin/sh", "-c", script, "hostdesk-update", executable, backup)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logFile, err := os.OpenFile(filepath.Join(a.dataDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		return err
	}
	logFile.Close()
	return command.Process.Release()
}

func (a *app) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !a.updateInstallMu.TryLock() {
		writeError(w, &apiError{http.StatusConflict, "HostDesk 正在更新"})
		return
	}
	defer a.updateInstallMu.Unlock()
	status := a.updateStatus(true)
	if status.Error != "" {
		writeError(w, &apiError{http.StatusBadGateway, status.Error})
		return
	}
	if !status.UpdateAvailable {
		writeError(w, &apiError{http.StatusConflict, "当前已经是最新版本"})
		return
	}
	assetName, err := updateAssetName(runtime.GOARCH)
	if err != nil {
		writeError(w, &apiError{http.StatusConflict, err.Error()})
		return
	}
	if status.AssetURL == "" || status.ChecksumsURL == "" {
		writeError(w, &apiError{http.StatusBadGateway, "Release 资产尚未就绪"})
		return
	}
	checksums, err := downloadUpdateAsset(status.ChecksumsURL, 1<<20)
	if err != nil {
		writeError(w, &apiError{http.StatusBadGateway, "下载校验文件失败：" + err.Error()})
		return
	}
	binary, err := downloadUpdateAsset(status.AssetURL, 64<<20)
	if err != nil {
		writeError(w, &apiError{http.StatusBadGateway, "下载更新失败：" + err.Error()})
		return
	}
	if err := verifyUpdateAsset(binary, checksums, assetName); err != nil {
		writeError(w, &apiError{http.StatusBadGateway, err.Error()})
		return
	}
	executable, backup, err := installUpdateBinary(binary)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.scheduleUpdateRestart(executable, backup); err != nil {
		restoreUpdateBinary(executable, backup)
		writeError(w, fmt.Errorf("安排服务重启失败：%w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": status.LatestVersion, "restarting": true})
}
