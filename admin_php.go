package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type phpSettings struct {
	UploadMaxFilesize string `json:"uploadMaxFilesize"`
	PostMaxSize       string `json:"postMaxSize"`
	MemoryLimit       string `json:"memoryLimit"`
	MaxExecutionTime  int    `json:"maxExecutionTime"`
	DisplayErrors     bool   `json:"displayErrors"`
}

type phpExtension struct {
	Name      string `json:"name"`
	Package   string `json:"package"`
	Installed bool   `json:"installed"`
}

var phpExtensionPackages = map[string]string{
	"bcmath": "bcmath", "bz2": "bz2", "calendar": "calendar", "ctype": "ctype", "curl": "curl", "dom": "dom",
	"exif": "exif", "fileinfo": "fileinfo", "ftp": "ftp", "gd": "gd", "gettext": "gettext",
	"gmp": "gmp", "iconv": "iconv", "intl": "intl", "ldap": "ldap", "mbstring": "mbstring",
	"memcached": "pecl-memcached", "mysqli": "mysqli", "pdo_mysql": "pdo_mysql", "pdo_sqlite": "pdo_sqlite",
	"pgsql": "pgsql", "phar": "phar", "redis": "pecl-redis", "simplexml": "simplexml", "soap": "soap",
	"sockets": "sockets", "sodium": "sodium", "sqlite3": "sqlite3", "xdebug": "pecl-xdebug",
	"xml": "xml", "xmlreader": "xmlreader", "xmlwriter": "xmlwriter", "xsl": "xsl", "zip": "zip",
}

func phpIniPath() string { return filepath.Join("/etc", phpPrefix(), "php.ini") }

func parsePHPIni(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return values, scanner.Err()
}

func phpBool(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "on" || value == "yes" || value == "true"
}

func currentPHPSettings() (phpSettings, error) {
	values, err := parsePHPIni(phpIniPath())
	if err != nil {
		return phpSettings{}, err
	}
	maxTime, _ := strconv.Atoi(values["max_execution_time"])
	return phpSettings{
		UploadMaxFilesize: values["upload_max_filesize"],
		PostMaxSize:       values["post_max_size"],
		MemoryLimit:       values["memory_limit"],
		MaxExecutionTime:  maxTime,
		DisplayErrors:     phpBool(values["display_errors"]),
	}, nil
}

func validatePHPSettings(settings phpSettings) error {
	for _, value := range []string{settings.UploadMaxFilesize, settings.PostMaxSize} {
		if !sizePattern.MatchString(strings.ToLower(value)) {
			return &apiError{http.StatusBadRequest, "PHP 文件大小格式无效，例如 64m"}
		}
	}
	memory := strings.ToLower(settings.MemoryLimit)
	if memory != "-1" && !sizePattern.MatchString(memory) {
		return &apiError{http.StatusBadRequest, "PHP 内存限制格式无效"}
	}
	if settings.MaxExecutionTime < 0 || settings.MaxExecutionTime > 3600 {
		return &apiError{http.StatusBadRequest, "最大执行时间必须在 0 到 3600 秒之间"}
	}
	return nil
}

func replaceINIValues(filename string, values map[string]string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	remaining := make(map[string]string, len(values))
	for key, value := range values {
		remaining[key] = value
	}
	patterns := make(map[string]*regexp.Regexp, len(values))
	for key := range values {
		patterns[key] = regexp.MustCompile(`^\s*;?\s*` + regexp.QuoteMeta(key) + `\s*=`)
	}
	for index, line := range lines {
		for key, pattern := range patterns {
			if pattern.MatchString(line) {
				lines[index] = key + " = " + values[key]
				delete(remaining, key)
				break
			}
		}
	}
	if len(remaining) > 0 {
		keys := make([]string, 0, len(remaining))
		for key := range remaining {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines = append(lines, "", "; Added by HostDesk")
		for _, key := range keys {
			lines = append(lines, key+" = "+remaining[key])
		}
	}
	backup := filename + ".hostdesk.bak"
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(backup, data, 0600); err != nil {
			return err
		}
	}
	return writeAtomic(filename, []byte(strings.Join(lines, "\n")), 0644)
}

func phpExtensions() []phpExtension {
	prefix := phpPrefix() + "-"
	names := make([]string, 0, len(phpExtensionPackages))
	for name := range phpExtensionPackages {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]phpExtension, 0, len(names))
	for _, name := range names {
		pkg := prefix + phpExtensionPackages[name]
		result = append(result, phpExtension{Name: name, Package: pkg, Installed: packageInstalled(pkg)})
	}
	return result
}

func (a *app) handlePHPGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	prefix := phpPrefix()
	installed := packageInstalled(prefix)
	settings := phpSettings{}
	if installed {
		settings, _ = currentPHPSettings()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installed":  installed,
		"prefix":     prefix,
		"version":    commandVersion("php", "-v"),
		"fpmRunning": serviceRunning(phpService(prefix)),
		"settings":   settings,
		"extensions": phpExtensions(),
	})
}

func (a *app) handlePHPSettingsPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !packageInstalled(phpPrefix()) {
		writeError(w, &apiError{http.StatusConflict, "请先安装 PHP"})
		return
	}
	var settings phpSettings
	if err := decodeJSON(w, r, &settings); err != nil {
		writeError(w, err)
		return
	}
	settings.UploadMaxFilesize = strings.ToLower(strings.TrimSpace(settings.UploadMaxFilesize))
	settings.PostMaxSize = strings.ToLower(strings.TrimSpace(settings.PostMaxSize))
	settings.MemoryLimit = strings.ToLower(strings.TrimSpace(settings.MemoryLimit))
	if err := validatePHPSettings(settings); err != nil {
		writeError(w, err)
		return
	}
	values := map[string]string{
		"upload_max_filesize": settings.UploadMaxFilesize,
		"post_max_size":       settings.PostMaxSize,
		"memory_limit":        settings.MemoryLimit,
		"max_execution_time":  strconv.Itoa(settings.MaxExecutionTime),
		"display_errors":      map[bool]string{true: "On", false: "Off"}[settings.DisplayErrors],
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if err := replaceINIValues(phpIniPath(), values); err != nil {
		writeError(w, err)
		return
	}
	service := phpService(phpPrefix())
	if serviceRunning(service) {
		if _, err := runAdmin(time.Minute, "rc-service", service, "restart"); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func extensionPackage(name string) (string, error) {
	suffix, ok := phpExtensionPackages[name]
	if !ok {
		return "", &apiError{http.StatusBadRequest, "不支持该 PHP 扩展"}
	}
	return phpPrefix() + "-" + suffix, nil
}

func (a *app) changePHPExtension(w http.ResponseWriter, r *http.Request, install bool) {
	if a.authorize(w, r, true) == nil {
		return
	}
	pkg, err := extensionPackage(r.PathValue("extension"))
	if err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	action := "add"
	args := []string{"add", "--no-cache", pkg}
	if !install {
		action = "del"
		args = []string{"del", pkg}
	}
	output, err := runAdmin(10*time.Minute, "apk", args...)
	if err == nil {
		service := phpService(phpPrefix())
		if serviceRunning(service) {
			_, err = runAdmin(time.Minute, "rc-service", service, "restart")
		}
	}
	if err != nil {
		writeError(w, &apiError{http.StatusInternalServerError, fmt.Sprintf("扩展%s失败：%s", action, err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "package": pkg, "output": output})
}

func (a *app) handlePHPExtensionInstall(w http.ResponseWriter, r *http.Request) {
	a.changePHPExtension(w, r, true)
}

func (a *app) handlePHPExtensionRemove(w http.ResponseWriter, r *http.Request) {
	a.changePHPExtension(w, r, false)
}
