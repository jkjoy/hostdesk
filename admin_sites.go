package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	nginxHTTPDir            = "/etc/nginx/http.d"
	webRootDir              = "/var/www"
	maxSiteNginxConfigBytes = 256 << 10
)

var (
	domainPattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	sizePattern   = regexp.MustCompile(`^[1-9][0-9]*(?:k|m|g)?$`)
)

type nginxSettings struct {
	ClientMaxBodySize string `json:"clientMaxBodySize"`
	KeepaliveTimeout  int    `json:"keepaliveTimeout"`
	Gzip              bool   `json:"gzip"`
	ServerTokens      bool   `json:"serverTokens"`
}

type siteDefinition struct {
	ID           string   `json:"id"`
	Domain       string   `json:"domain"`
	Aliases      []string `json:"aliases"`
	Type         string   `json:"type"`
	Root         string   `json:"root"`
	Upstream     string   `json:"upstream"`
	RewriteMode  string   `json:"rewriteMode,omitempty"`
	RewriteRules string   `json:"rewriteRules,omitempty"`
	Enabled      bool     `json:"enabled"`
	SSL          bool     `json:"ssl"`
	Certificate  string   `json:"certificate"`
	PrivateKey   string   `json:"privateKey"`
	NginxConfig  string   `json:"nginxConfig,omitempty"`
	CreatedAt    string   `json:"createdAt"`
}

type fileSnapshot struct {
	filename string
	data     []byte
	mode     os.FileMode
	exists   bool
}

func captureFile(filename string) (fileSnapshot, error) {
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{filename: filename}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{filename: filename, data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func restoreFile(snapshot fileSnapshot) error {
	if snapshot.exists {
		return writeAtomic(snapshot.filename, snapshot.data, snapshot.mode)
	}
	if err := os.Remove(snapshot.filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func restoreFiles(snapshots ...fileSnapshot) {
	for _, snapshot := range snapshots {
		_ = restoreFile(snapshot)
	}
}

func defaultNginxSettings() nginxSettings {
	return nginxSettings{ClientMaxBodySize: "64m", KeepaliveTimeout: 65, Gzip: true, ServerTokens: false}
}

func ensureNginxLayout() error {
	if err := os.MkdirAll(nginxHTTPDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(webRootDir, 0755); err != nil {
		return err
	}
	return nil
}

func writeAtomic(filename string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(filename), ".hostdesk-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(data)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tempName, filename)
	}
	return err
}

func (a *app) settingsPath() string { return filepath.Join(a.dataDir, "nginx-settings.json") }
func (a *app) sitesPath() string    { return filepath.Join(a.dataDir, "sites.json") }

func (a *app) loadNginxSettings() nginxSettings {
	settings := defaultNginxSettings()
	data, err := os.ReadFile(a.settingsPath())
	if err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	return settings
}

func validateNginxSettings(settings nginxSettings) error {
	settings.ClientMaxBodySize = strings.ToLower(strings.TrimSpace(settings.ClientMaxBodySize))
	if !sizePattern.MatchString(settings.ClientMaxBodySize) {
		return &apiError{http.StatusBadRequest, "上传限制格式无效，例如 64m"}
	}
	if settings.KeepaliveTimeout < 5 || settings.KeepaliveTimeout > 600 {
		return &apiError{http.StatusBadRequest, "Keepalive 必须在 5 到 600 秒之间"}
	}
	return nil
}

func renderSiteSettings(settings nginxSettings) string {
	gzip := "off"
	if settings.Gzip {
		gzip = "on"
	}
	tokens := "off"
	if settings.ServerTokens {
		tokens = "on"
	}
	return fmt.Sprintf(`    client_max_body_size %s;
    keepalive_timeout %d;
    server_tokens %s;
    gzip %s;
    gzip_types text/plain text/css application/json application/javascript application/xml image/svg+xml;
`, settings.ClientMaxBodySize, settings.KeepaliveTimeout, tokens, gzip)
}

func nginxTest() error {
	output, err := exec.Command("nginx", "-t").CombinedOutput()
	if err != nil {
		return &apiError{http.StatusBadRequest, strings.TrimSpace(string(output))}
	}
	return nil
}

func nginxReloadIfRunning() error {
	if !serviceRunning("nginx") {
		return nil
	}
	_, err := runAdmin(time.Minute, "rc-service", "nginx", "reload")
	return err
}

func (a *app) handleNginxSettingsGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	writeJSON(w, http.StatusOK, a.loadNginxSettings())
}

func (a *app) handleNginxSettingsPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !packageInstalled("nginx") {
		writeError(w, &apiError{http.StatusConflict, "请先安装 Nginx"})
		return
	}
	var settings nginxSettings
	if err := decodeJSON(w, r, &settings); err != nil {
		writeError(w, err)
		return
	}
	settings.ClientMaxBodySize = strings.ToLower(strings.TrimSpace(settings.ClientMaxBodySize))
	if err := validateNginxSettings(settings); err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if err := ensureNginxLayout(); err != nil {
		writeError(w, err)
		return
	}
	configPath := filepath.Join(nginxHTTPDir, "00-hostdesk-global.conf")
	configSnapshot, err := captureFile(configPath)
	if err != nil {
		writeError(w, err)
		return
	}
	settingsSnapshot, err := captureFile(a.settingsPath())
	if err != nil {
		writeError(w, err)
		return
	}
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	snapshots := []fileSnapshot{configSnapshot, settingsSnapshot}
	for _, site := range sites {
		snapshot, snapshotErr := captureFile(siteConfigPath(site))
		if snapshotErr != nil {
			writeError(w, snapshotErr)
			return
		}
		snapshots = append(snapshots, snapshot)
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err == nil {
		err = writeAtomic(configPath, []byte("# HostDesk settings are applied in each managed server block.\n"), 0644)
	}
	if err == nil {
		for _, site := range sites {
			if err = writeAtomic(siteConfigPath(site), []byte(renderSiteConfig(site, settings)), 0644); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = nginxTest()
	}
	if err == nil {
		err = writeAtomic(a.settingsPath(), append(encoded, '\n'), 0600)
	}
	if err == nil {
		err = nginxReloadIfRunning()
	}
	if err != nil {
		restoreFiles(snapshots...)
		_ = nginxReloadIfRunning()
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func normalizeDomain(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func validateSiteBase(site *siteDefinition) error {
	site.Domain = normalizeDomain(site.Domain)
	if !domainPattern.MatchString(site.Domain) {
		return &apiError{http.StatusBadRequest, "主域名格式无效"}
	}
	seen := map[string]bool{site.Domain: true}
	aliases := make([]string, 0, len(site.Aliases))
	for _, alias := range site.Aliases {
		alias = normalizeDomain(alias)
		if alias == "" {
			continue
		}
		if !domainPattern.MatchString(alias) || seen[alias] {
			return &apiError{http.StatusBadRequest, "别名域名格式无效或重复"}
		}
		seen[alias] = true
		aliases = append(aliases, alias)
	}
	site.Aliases = aliases
	site.ID = strings.ReplaceAll(site.Domain, ".", "-")
	switch site.Type {
	case "static", "php":
		if site.Root == "" {
			site.Root = filepath.Join(webRootDir, site.Domain, "public")
		}
		site.Root = filepath.Clean(site.Root)
		if !filepath.IsAbs(site.Root) || !inside(webRootDir, site.Root) || site.Root == webRootDir {
			return &apiError{http.StatusBadRequest, "网站目录必须位于 /var/www 下"}
		}
	case "proxy":
		if site.Upstream == "" {
			return &apiError{http.StatusBadRequest, "反向代理地址不能为空"}
		}
		parsed, err := url.Parse(site.Upstream)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return &apiError{http.StatusBadRequest, "反向代理地址必须是 http:// 或 https:// 地址"}
		}
	default:
		return &apiError{http.StatusBadRequest, "网站类型无效"}
	}
	if site.Type != "php" {
		site.RewriteMode = ""
		site.RewriteRules = ""
	} else {
		if site.RewriteMode == "" {
			site.RewriteMode = "laravel"
		}
		switch site.RewriteMode {
		case "none", "wordpress", "laravel", "thinkphp":
			site.RewriteRules = ""
		case "custom":
			site.RewriteRules = strings.TrimSpace(site.RewriteRules)
			if err := validateRewriteRules(site.RewriteRules); err != nil {
				return err
			}
		default:
			return &apiError{http.StatusBadRequest, "伪静态模式无效"}
		}
	}
	return nil
}

func validateSiteCertificatePaths(site *siteDefinition) error {
	if site.SSL {
		if site.Certificate == "" || site.PrivateKey == "" {
			return &apiError{http.StatusBadRequest, "启用 HTTPS 时必须选择或填写证书"}
		}
		for _, filename := range []string{site.Certificate, site.PrivateKey} {
			if !filepath.IsAbs(filename) {
				return &apiError{http.StatusBadRequest, "证书路径必须是绝对路径"}
			}
			if _, err := os.Stat(filename); err != nil {
				return &apiError{http.StatusBadRequest, "证书或私钥文件不存在"}
			}
		}
	}
	return nil
}

func validateSite(site *siteDefinition) error {
	if err := validateSiteBase(site); err != nil {
		return err
	}
	return validateSiteCertificatePaths(site)
}

func validateRewriteRules(rules string) error {
	if rules == "" {
		return &apiError{http.StatusBadRequest, "自定义伪静态规则不能为空"}
	}
	if len(rules) > 8192 || strings.ContainsAny(rules, "{}\x00") {
		return &apiError{http.StatusBadRequest, "自定义伪静态规则无效"}
	}
	for _, line := range strings.Split(rules, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allowed := strings.HasPrefix(line, "try_files ") || strings.HasPrefix(line, "rewrite ") || strings.HasPrefix(line, "return ")
		if !allowed || !strings.HasSuffix(line, ";") || strings.Count(line, ";") != 1 {
			return &apiError{http.StatusBadRequest, "自定义规则仅支持 try_files、rewrite 和 return 指令，且每行需以分号结尾"}
		}
	}
	return nil
}

func siteNames(site siteDefinition) string {
	return strings.Join(append([]string{site.Domain}, site.Aliases...), " ")
}

func renderRewriteRules(site siteDefinition) string {
	mode := site.RewriteMode
	if mode == "" {
		mode = "laravel"
	}
	switch mode {
	case "none":
		return "        try_files $uri $uri/ =404;\n"
	case "wordpress":
		return "        try_files $uri $uri/ /index.php?$args;\n"
	case "thinkphp":
		return "        try_files $uri $uri/ /index.php?s=$uri&$args;\n"
	case "custom":
		var rendered strings.Builder
		for _, line := range strings.Split(site.RewriteRules, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				rendered.WriteString("        ")
				rendered.WriteString(line)
				rendered.WriteByte('\n')
			}
		}
		return rendered.String()
	default:
		return "        try_files $uri $uri/ /index.php?$query_string;\n"
	}
}

func renderSiteBody(site siteDefinition) string {
	switch site.Type {
	case "proxy":
		return fmt.Sprintf(`    location / {
        proxy_pass %s;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
    }
`, site.Upstream)
	case "php":
		return fmt.Sprintf(`    root %s;
    index index.php index.html;
    error_page 404 /404.html;
    location = /404.html { internal; }
    location / {
%s    }
    location ~ \.php$ {
        try_files $uri =404;
        include fastcgi.conf;
        fastcgi_pass 127.0.0.1:9000;
    }
    location ~ /\. { deny all; }
`, site.Root, renderRewriteRules(site))
	default:
		return fmt.Sprintf(`    root %s;
    index index.html;
    error_page 404 /404.html;
    location = /404.html { internal; }
    location / { try_files $uri $uri/ =404; }
    location ~ /\. { deny all; }
`, site.Root)
	}
}

func renderACMEChallengeLocation() string {
	return fmt.Sprintf(`    location ^~ /.well-known/acme-challenge/ {
        root %s;
        default_type text/plain;
    }
`, acmeHTTPRoot)
}

func renderSiteConfig(site siteDefinition, settings nginxSettings) string {
	if site.NginxConfig != "" {
		return site.NginxConfig
	}
	names := siteNames(site)
	body := renderSiteBody(site)
	acmeLocation := renderACMEChallengeLocation()
	commonLogs := fmt.Sprintf("    access_log /var/log/nginx/%s.access.log;\n    error_log /var/log/nginx/%s.error.log;\n", site.ID, site.ID)
	serverSettings := renderSiteSettings(settings)
	if !site.SSL {
		return fmt.Sprintf("# Managed by HostDesk.\nserver {\n    listen 80;\n    listen [::]:80;\n    server_name %s;\n%s%s%s%s}\n", names, serverSettings, commonLogs, acmeLocation, body)
	}
	return fmt.Sprintf(`# Managed by HostDesk.
server {
    listen 80;
    listen [::]:80;
    server_name %s;
%s
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name %s;
    ssl_certificate %s;
    ssl_certificate_key %s;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:HostDeskSSL:10m;
%s%s%s%s}
`, names, acmeLocation, names, site.Certificate, site.PrivateKey, serverSettings, commonLogs, acmeLocation, body)
}

func (a *app) loadSites() ([]siteDefinition, error) {
	data, err := os.ReadFile(a.sitesPath())
	if errors.Is(err, os.ErrNotExist) {
		return []siteDefinition{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sites []siteDefinition
	if err := json.Unmarshal(data, &sites); err != nil {
		return nil, err
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Domain < sites[j].Domain })
	return sites, nil
}

func (a *app) saveSites(sites []siteDefinition) error {
	encoded, err := json.MarshalIndent(sites, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.sitesPath(), append(encoded, '\n'), 0600)
}

func (a *app) restoreSites(sites []siteDefinition, cause error) error {
	if err := a.saveSites(sites); err != nil {
		return fmt.Errorf("%v；恢复网站数据失败：%w", cause, err)
	}
	return cause
}

func siteConfigPath(site siteDefinition) string {
	suffix := ".conf"
	if !site.Enabled {
		suffix = ".conf.disabled"
	}
	return filepath.Join(nginxHTTPDir, "hostdesk-"+site.ID+suffix)
}

func activeSiteConfigPath(site siteDefinition) string {
	site.Enabled = true
	return siteConfigPath(site)
}

func normalizeSiteNginxConfig(config string) (string, error) {
	if len(config) > maxSiteNginxConfigBytes {
		return "", &apiError{http.StatusRequestEntityTooLarge, "Nginx 配置不能超过 256 KiB"}
	}
	if strings.TrimSpace(config) == "" {
		return "", &apiError{http.StatusBadRequest, "Nginx 配置不能为空"}
	}
	if strings.ContainsRune(config, '\x00') {
		return "", &apiError{http.StatusBadRequest, "Nginx 配置包含无效字符"}
	}
	if !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	return config, nil
}

func (a *app) applySiteNginxOverride(sites []siteDefinition, index int, config string) error {
	if err := ensureNginxLayout(); err != nil {
		return err
	}
	sites[index].NginxConfig = config
	site := sites[index]
	targetPath := siteConfigPath(site)
	validationPath := activeSiteConfigPath(site)

	targetSnapshot, err := captureFile(targetPath)
	if err != nil {
		return err
	}
	sitesSnapshot, err := captureFile(a.sitesPath())
	if err != nil {
		return err
	}
	snapshots := []fileSnapshot{targetSnapshot, sitesSnapshot}
	if validationPath != targetPath {
		validationSnapshot, snapshotErr := captureFile(validationPath)
		if snapshotErr != nil {
			return snapshotErr
		}
		snapshots = append(snapshots, validationSnapshot)
	}

	desired := []byte(renderSiteConfig(site, a.loadNginxSettings()))
	if err = writeAtomic(validationPath, desired, 0644); err == nil {
		err = nginxTest()
	}
	if validationPath != targetPath {
		if restoreErr := restoreFile(snapshots[len(snapshots)-1]); err == nil {
			err = restoreErr
		}
		if err == nil {
			err = writeAtomic(targetPath, desired, 0644)
		}
	}
	if err == nil {
		err = a.saveSites(sites)
	}
	if err == nil && site.Enabled {
		err = nginxReloadIfRunning()
	}
	if err != nil {
		restoreFiles(snapshots...)
		if site.Enabled {
			_ = nginxReloadIfRunning()
		}
		return err
	}

	return nil
}

func (a *app) handleSiteNginxGet(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, site := range sites {
		if site.ID != r.PathValue("id") {
			continue
		}
		path := siteConfigPath(site)
		config, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			config = []byte(renderSiteConfig(site, a.loadNginxSettings()))
		} else if readErr != nil {
			writeError(w, readErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"config":     string(config),
			"path":       path,
			"customized": site.NginxConfig != "",
		})
		return
	}
	writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
}

func (a *app) handleSiteNginxPut(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !packageInstalled("nginx") {
		writeError(w, &apiError{http.StatusConflict, "请先安装 Nginx"})
		return
	}
	var request struct {
		Config string `json:"config"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	config, err := normalizeSiteNginxConfig(request.Config)
	if err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	index := -1
	for i := range sites {
		if sites[i].ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
		return
	}
	if err := a.applySiteNginxOverride(sites, index, config); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSiteNginxDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !packageInstalled("nginx") {
		writeError(w, &apiError{http.StatusConflict, "请先安装 Nginx"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	index := -1
	for i := range sites {
		if sites[i].ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
		return
	}
	if err := a.applySiteNginxOverride(sites, index, ""); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) applySite(site siteDefinition, previous *siteDefinition) error {
	if err := ensureNginxLayout(); err != nil {
		return err
	}
	if site.Type != "proxy" {
		if err := os.MkdirAll(site.Root, 0755); err != nil {
			return err
		}
		if err := ensureDefaultSiteFiles(site); err != nil {
			return err
		}
	}
	targetPath := siteConfigPath(site)
	targetSnapshot, err := captureFile(targetPath)
	if err != nil {
		return err
	}
	snapshots := []fileSnapshot{targetSnapshot}
	previousPath := ""
	if previous != nil {
		previousPath = siteConfigPath(*previous)
		if previousPath != targetPath {
			previousSnapshot, err := captureFile(previousPath)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, previousSnapshot)
		}
	}
	if err := writeAtomic(targetPath, []byte(renderSiteConfig(site, a.loadNginxSettings())), 0644); err != nil {
		return err
	}
	if previousPath != "" && previousPath != targetPath {
		if err := os.Remove(previousPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			restoreFiles(snapshots...)
			return err
		}
	}
	activeChanged := site.Enabled || (previous != nil && previous.Enabled)
	if activeChanged {
		if err := nginxTest(); err != nil {
			restoreFiles(snapshots...)
			return err
		}
		if err := nginxReloadIfRunning(); err != nil {
			restoreFiles(snapshots...)
			_ = nginxReloadIfRunning()
			return err
		}
	}
	return nil
}

func removeSiteConfig(site siteDefinition) error {
	filename := siteConfigPath(site)
	snapshot, err := captureFile(filename)
	if err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !site.Enabled {
		return nil
	}
	if err := nginxTest(); err != nil {
		restoreFiles(snapshot)
		return err
	}
	if err := nginxReloadIfRunning(); err != nil {
		restoreFiles(snapshot)
		_ = nginxReloadIfRunning()
		return err
	}
	return nil
}

func (a *app) handleSitesList(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	records, err := a.loadCertificates()
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]siteView, 0, len(sites))
	for _, site := range sites {
		views = append(views, siteToView(site, records))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": views, "certificates": siteCertificateOptions(records), "nginxInstalled": packageInstalled("nginx")})
}

func (a *app) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	if !packageInstalled("nginx") {
		writeError(w, &apiError{http.StatusConflict, "请先安装 Nginx"})
		return
	}
	var request siteRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	site := request.siteDefinition()
	if err := validateSiteBase(&site); err != nil {
		writeError(w, err)
		return
	}
	if site.CreatedAt == "" {
		site.CreatedAt = time.Now().Format(time.RFC3339)
	}
	site.Enabled = true
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, current := range sites {
		if current.ID == site.ID {
			writeError(w, &apiError{http.StatusConflict, "该站点已经存在"})
			return
		}
	}
	records, err := a.loadCertificates()
	if err != nil {
		writeError(w, err)
		return
	}
	certificateSnapshots, err := a.resolveSiteCertificate(&site, request, nil, records)
	if err == nil {
		err = validateSiteCertificatePaths(&site)
	}
	if err != nil {
		restoreCertificateSnapshots(certificateSnapshots, err)
		writeError(w, err)
		return
	}
	previousSites := append([]siteDefinition(nil), sites...)
	sites = append(sites, site)
	err = a.saveSites(sites)
	if err == nil {
		err = a.applySite(site, nil)
		if err != nil {
			err = a.restoreSites(previousSites, err)
		}
	}
	if err != nil {
		restoreCertificateSnapshots(certificateSnapshots, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, siteToView(site, records))
}

func (a *app) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var request siteRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	index := -1
	for i := range sites {
		if sites[i].ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
		return
	}
	previous := sites[index]
	site := request.siteDefinition()
	site.ID = previous.ID
	site.Domain = previous.Domain
	site.CreatedAt = previous.CreatedAt
	site.Enabled = previous.Enabled
	site.NginxConfig = previous.NginxConfig
	if err := validateSiteBase(&site); err != nil {
		writeError(w, err)
		return
	}
	records, err := a.loadCertificates()
	if err != nil {
		writeError(w, err)
		return
	}
	certificateSnapshots, err := a.resolveSiteCertificate(&site, request, &previous, records)
	if err == nil {
		err = validateSiteCertificatePaths(&site)
	}
	if err != nil {
		restoreCertificateSnapshots(certificateSnapshots, err)
		writeError(w, err)
		return
	}
	previousSites := append([]siteDefinition(nil), sites...)
	sites[index] = site
	err = a.saveSites(sites)
	if err == nil {
		err = a.applySite(site, &previous)
		if err != nil {
			err = a.restoreSites(previousSites, err)
		}
	}
	if err != nil {
		restoreCertificateSnapshots(certificateSnapshots, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, siteToView(site, records))
}

func (a *app) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	index := -1
	for i := range sites {
		if sites[i].ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
		return
	}
	deleted := sites[index]
	previousSites := append([]siteDefinition(nil), sites...)
	sites = append(sites[:index], sites[index+1:]...)
	err = a.saveSites(sites)
	if err == nil {
		err = removeSiteConfig(deleted)
		if err != nil {
			err = a.restoreSites(previousSites, err)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSiteAction(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	action := r.PathValue("action")
	if action != "enable" && action != "disable" {
		writeError(w, &apiError{http.StatusBadRequest, "不支持该操作"})
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	sites, err := a.loadSites()
	if err != nil {
		writeError(w, err)
		return
	}
	index := -1
	for i := range sites {
		if sites[i].ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, &apiError{http.StatusNotFound, "站点不存在"})
		return
	}
	previous := sites[index]
	previousSites := append([]siteDefinition(nil), sites...)
	sites[index].Enabled = action == "enable"
	err = a.saveSites(sites)
	if err == nil {
		err = a.applySite(sites[index], &previous)
		if err != nil {
			err = a.restoreSites(previousSites, err)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sites[index])
}
