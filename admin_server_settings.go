package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	zoneinfoRoot       = "/usr/share/zoneinfo"
	hostnameFile       = "/etc/hostname"
	fstabFile          = "/etc/fstab"
	hostDeskSwapPath   = "/var/lib/hostdesk/hostdesk.swap"
	hostDeskSwapMarker = "# HostDesk managed swap"
	minimumSwapMB      = int64(64)
	maximumSwapMB      = int64(32768)
)

type swapEntry struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	SizeBytes uint64 `json:"sizeBytes"`
	UsedBytes uint64 `json:"usedBytes"`
	Priority  int    `json:"priority"`
	Managed   bool   `json:"managed"`
	Active    bool   `json:"active"`
}

type serverSettings struct {
	Hostname        string      `json:"hostname"`
	Timezone        string      `json:"timezone"`
	Timezones       []string    `json:"timezones"`
	CurrentTime     string      `json:"currentTime"`
	OperatingSystem string      `json:"operatingSystem"`
	Kernel          string      `json:"kernel"`
	Architecture    string      `json:"architecture"`
	NTPRunning      bool        `json:"ntpRunning"`
	Swaps           []swapEntry `json:"swaps"`
	SwapTotalBytes  uint64      `json:"swapTotalBytes"`
	SwapUsedBytes   uint64      `json:"swapUsedBytes"`
	ManagedSwapPath string      `json:"managedSwapPath"`
}

type serverSettingsUpdate struct {
	Hostname string `json:"hostname"`
	Timezone string `json:"timezone"`
}

type swapCreateRequest struct {
	SizeMB int64 `json:"sizeMb"`
}

func validateHostname(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return "", &apiError{http.StatusBadRequest, "主机名格式无效"}
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", &apiError{http.StatusBadRequest, "主机名格式无效"}
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return "", &apiError{http.StatusBadRequest, "主机名只能包含字母、数字、连字符和点"}
			}
		}
	}
	return value, nil
}

func availableTimezones() []string {
	var zones []string
	_ = filepath.WalkDir(zoneinfoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relative, relErr := filepath.Rel(zoneinfoRoot, path)
		if relErr != nil || relative == "." {
			return nil
		}
		if entry.IsDir() {
			if relative == "posix" || relative == "right" || relative == "SystemV" {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(relative)
		if strings.HasSuffix(base, ".tab") || strings.HasPrefix(base, "leap") || base == "tzdata.zi" || base == "localtime" || base == "posixrules" {
			return nil
		}
		zones = append(zones, filepath.ToSlash(relative))
		return nil
	})
	sort.Strings(zones)
	return zones
}

func currentTimezone() string {
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if relative, relErr := filepath.Rel(zoneinfoRoot, target); relErr == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(relative)
		}
	}
	for _, filename := range []string{"/etc/timezone", "/etc/TZ"} {
		if data, err := os.ReadFile(filename); err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return "UTC"
}

func validateTimezone(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "..") || strings.ContainsRune(value, '\x00') {
		return "", &apiError{http.StatusBadRequest, "时区无效"}
	}
	target, err := filepath.EvalSymlinks(filepath.Join(zoneinfoRoot, filepath.FromSlash(value)))
	if err != nil || !inside(zoneinfoRoot, target) || target == zoneinfoRoot {
		return "", &apiError{http.StatusBadRequest, "时区无效"}
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return "", &apiError{http.StatusBadRequest, "时区无效"}
	}
	return value, nil
}

func setTimezone(value string) error {
	target := filepath.Join(zoneinfoRoot, filepath.FromSlash(value))
	temporary := filepath.Join("/etc", ".hostdesk-localtime-"+randomToken(6))
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	return os.Rename(temporary, "/etc/localtime")
}

func parseSwaps(data string) []swapEntry {
	var swaps []swapEntry
	for index, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if index == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		sizeKB, sizeErr := strconv.ParseUint(fields[2], 10, 64)
		usedKB, usedErr := strconv.ParseUint(fields[3], 10, 64)
		priority, priorityErr := strconv.Atoi(fields[4])
		if sizeErr != nil || usedErr != nil || priorityErr != nil {
			continue
		}
		swaps = append(swaps, swapEntry{
			Path: fields[0], Type: fields[1], SizeBytes: sizeKB * 1024, UsedBytes: usedKB * 1024,
			Priority: priority, Managed: filepath.Clean(fields[0]) == hostDeskSwapPath, Active: true,
		})
	}
	return swaps
}

func readSwaps() []swapEntry {
	data, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return nil
	}
	return parseSwaps(string(data))
}

func operatingSystemName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "Linux"
}

func collectServerSettings() serverSettings {
	settings := serverSettings{
		Timezone: currentTimezone(), Timezones: availableTimezones(), OperatingSystem: operatingSystemName(),
		Architecture: runtime.GOARCH, NTPRunning: serviceRunning("ntpd"), ManagedSwapPath: hostDeskSwapPath,
	}
	settings.Hostname, _ = os.Hostname()
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		settings.Kernel = strings.TrimSpace(string(data))
	}
	if location, err := time.LoadLocation(settings.Timezone); err == nil {
		settings.CurrentTime = time.Now().In(location).Format(time.RFC3339)
	} else {
		settings.CurrentTime = time.Now().Format(time.RFC3339)
	}
	settings.Swaps = readSwaps()
	managedActive := false
	for _, swap := range settings.Swaps {
		managedActive = managedActive || swap.Managed
	}
	if !managedActive {
		if info, err := os.Stat(hostDeskSwapPath); err == nil && info.Mode().IsRegular() {
			settings.Swaps = append(settings.Swaps, swapEntry{Path: hostDeskSwapPath, Type: "file", SizeBytes: uint64(info.Size()), Managed: true})
		}
	}
	for _, swap := range settings.Swaps {
		if swap.Active {
			settings.SwapTotalBytes += swap.SizeBytes
			settings.SwapUsedBytes += swap.UsedBytes
		}
	}
	return settings
}

func renderManagedSwapFstab(data []byte, enabled bool) []byte {
	var lines []string
	managedLine := hostDeskSwapPath + " none swap defaults 0 0"
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == hostDeskSwapMarker || trimmed == managedLine || (strings.HasPrefix(trimmed, hostDeskSwapPath+" ") && strings.Contains(trimmed, " swap ")) {
			continue
		}
		if line != "" || len(lines) > 0 {
			lines = append(lines, line)
		}
	}
	if enabled {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, hostDeskSwapMarker, managedLine)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func (a *app) handleServerSettingsGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	writeJSON(w, http.StatusOK, collectServerSettings())
}

func (a *app) handleServerSettingsPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body serverSettingsUpdate
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	hostname, err := validateHostname(body.Hostname)
	if err != nil {
		writeError(w, err)
		return
	}
	timezone, err := validateTimezone(body.Timezone)
	if err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()

	oldHostname, _ := os.Hostname()
	oldTimezone := currentTimezone()
	hostnameSnapshot, err := captureFile(hostnameFile)
	if err != nil {
		writeError(w, err)
		return
	}
	if timezone != oldTimezone {
		if err := setTimezone(timezone); err != nil {
			writeError(w, &apiError{http.StatusInternalServerError, "设置时区失败: " + err.Error()})
			return
		}
	}
	if hostname != oldHostname {
		if err := writeAtomic(hostnameFile, []byte(hostname+"\n"), 0644); err != nil {
			if timezone != oldTimezone {
				_ = setTimezone(oldTimezone)
			}
			writeError(w, err)
			return
		}
		if _, err := runAdmin(15*time.Second, "hostname", hostname); err != nil {
			restoreFiles(hostnameSnapshot)
			_, _ = runAdmin(15*time.Second, "hostname", oldHostname)
			if timezone != oldTimezone {
				_ = setTimezone(oldTimezone)
			}
			writeError(w, &apiError{http.StatusInternalServerError, "设置主机名失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, collectServerSettings())
}

func (a *app) handleSwapCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var body swapCreateRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.SizeMB < minimumSwapMB || body.SizeMB > maximumSwapMB {
		writeError(w, &apiError{http.StatusBadRequest, fmt.Sprintf("交换空间必须在 %d MB 到 %d MB 之间", minimumSwapMB, maximumSwapMB)})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if _, err := os.Stat(hostDeskSwapPath); err == nil {
		writeError(w, &apiError{http.StatusConflict, "HostDesk 交换文件已存在，请先移除后再创建"})
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(w, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(hostDeskSwapPath), 0700); err != nil {
		writeError(w, err)
		return
	}
	placeholder, err := os.OpenFile(hostDeskSwapPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, &apiError{http.StatusConflict, "HostDesk 交换文件已存在，请先移除后再创建"})
		} else {
			writeError(w, err)
		}
		return
	}
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(hostDeskSwapPath)
		writeError(w, err)
		return
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(hostDeskSwapPath), &filesystem); err != nil {
		_ = os.Remove(hostDeskSwapPath)
		writeError(w, err)
		return
	}
	sizeBytes := uint64(body.SizeMB) << 20
	availableBytes := filesystem.Bavail * uint64(filesystem.Bsize)
	if sizeBytes+(256<<20) > availableBytes {
		_ = os.Remove(hostDeskSwapPath)
		writeError(w, &apiError{http.StatusBadRequest, "磁盘可用空间不足，请至少保留 256 MB"})
		return
	}
	fstabSnapshot, err := captureFile(fstabFile)
	if err != nil {
		_ = os.Remove(hostDeskSwapPath)
		writeError(w, err)
		return
	}
	cleanup := func() {
		_, _ = runAdmin(30*time.Second, "swapoff", hostDeskSwapPath)
		_ = os.Remove(hostDeskSwapPath)
	}
	if _, err := runAdmin(5*time.Minute, "fallocate", "-l", strconv.FormatInt(body.SizeMB, 10)+"M", hostDeskSwapPath); err != nil {
		cleanup()
		writeError(w, &apiError{http.StatusInternalServerError, "创建交换文件失败: " + err.Error()})
		return
	}
	if err := os.Chmod(hostDeskSwapPath, 0600); err != nil {
		cleanup()
		writeError(w, err)
		return
	}
	if _, err := runAdmin(time.Minute, "mkswap", hostDeskSwapPath); err != nil {
		cleanup()
		writeError(w, &apiError{http.StatusInternalServerError, "初始化交换文件失败: " + err.Error()})
		return
	}
	if _, err := runAdmin(time.Minute, "swapon", hostDeskSwapPath); err != nil {
		cleanup()
		writeError(w, &apiError{http.StatusInternalServerError, "启用交换空间失败: " + err.Error()})
		return
	}
	if err := writeAtomic(fstabFile, renderManagedSwapFstab(fstabSnapshot.data, true), fstabSnapshot.mode); err != nil {
		cleanup()
		restoreFiles(fstabSnapshot)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, collectServerSettings())
}

func (a *app) handleSwapDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	fstabSnapshot, err := captureFile(fstabFile)
	if err != nil {
		writeError(w, err)
		return
	}
	active := false
	for _, swap := range readSwaps() {
		if swap.Managed {
			active = true
			break
		}
	}
	if active {
		if _, err := runAdmin(2*time.Minute, "swapoff", hostDeskSwapPath); err != nil {
			writeError(w, &apiError{http.StatusConflict, "无法停用交换空间，可能有数据仍在使用: " + err.Error()})
			return
		}
	}
	if err := writeAtomic(fstabFile, renderManagedSwapFstab(fstabSnapshot.data, false), fstabSnapshot.mode); err != nil {
		if active {
			_, _ = runAdmin(time.Minute, "swapon", hostDeskSwapPath)
		}
		writeError(w, err)
		return
	}
	if err := os.Remove(hostDeskSwapPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		restoreFiles(fstabSnapshot)
		if active {
			_, _ = runAdmin(time.Minute, "swapon", hostDeskSwapPath)
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, collectServerSettings())
}
