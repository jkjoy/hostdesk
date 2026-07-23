package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newAuthTestApp(t *testing.T, dataDir string) *app {
	t.Helper()
	db, err := openAuthDatabase(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{
		db: db, rootReal: "/test-root", sessions: make(map[string]*sessionInfo),
	}
}

func authRequest(t *testing.T, handler http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func authenticatedRequest(t *testing.T, handler http.HandlerFunc, method, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func TestFreshDatabaseRequiresSetupAndCreatesAdministrator(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	required, err := a.setupRequired()
	if err != nil || !required {
		t.Fatalf("fresh database must require setup: required=%v err=%v", required, err)
	}

	response := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup failed: status=%d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM administrators").Scan(&count); err != nil || count != 1 {
		t.Fatalf("setup must create exactly one administrator: count=%d err=%v", count, err)
	}
	info, err := os.Stat(filepath.Join(dataDir, authDatabaseName))
	if err != nil {
		t.Fatalf("database file missing: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions must be 0600: mode=%v", info.Mode().Perm())
	}

	response = authRequest(t, a.handleSetup, http.MethodPost, `{"username":"other","password":"another-password-456"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("second setup must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLegacyAdministratorMigrationAndLogin(t *testing.T) {
	dataDir := t.TempDir()
	salt := randomToken(16)
	hash, err := derivePassword("legacy-password-123", salt)
	if err != nil {
		t.Fatal(err)
	}
	legacy := authFile{Username: "legacy-admin", Salt: salt, Hash: hash}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	a := newAuthTestApp(t, dataDir)
	required, err := a.setupRequired()
	if err != nil || required {
		t.Fatalf("migrated database must not require setup: required=%v err=%v", required, err)
	}

	response := authRequest(t, a.handleLogin, http.MethodPost, `{"username":"legacy-admin","password":"legacy-password-123"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("migrated login failed: status=%d body=%s", response.Code, response.Body.String())
	}
	response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"legacy-admin","password":"wrong-password"}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password must fail: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLoginProtectionLocksRepeatedFailures(t *testing.T) {
	dataDir := t.TempDir()
	a := newAuthTestApp(t, dataDir)
	response := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup failed: %s", response.Body.String())
	}
	for attempt := 0; attempt < 5; attempt++ {
		response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"owner","password":"wrong-password"}`)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d returned %d: %s", attempt+1, response.Code, response.Body.String())
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = newAuthTestApp(t, dataDir)
	response = authRequest(t, a.handleLogin, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("locked login returned %d without Retry-After: %s", response.Code, response.Body.String())
	}
}

func TestClientIPOnlyTrustsLoopbackProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Real-IP", "203.0.113.8")
	if ip := requestClientIP(request); ip != "203.0.113.8" {
		t.Fatalf("loopback proxy IP was not used: %s", ip)
	}
	request.RemoteAddr = "198.51.100.7:4321"
	request.Header.Set("X-Real-IP", "203.0.113.9")
	if ip := requestClientIP(request); ip != "198.51.100.7" {
		t.Fatalf("untrusted forwarded IP was used: %s", ip)
	}
}

func TestAdministratorCredentialsCanBeChanged(t *testing.T) {
	a := newAuthTestApp(t, t.TempDir())
	setup := authRequest(t, a.handleSetup, http.MethodPost, `{"username":"owner","password":"strong-password-123"}`)
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup failed: %s", setup.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(setup.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := setup.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("setup returned %d cookies", len(cookies))
	}
	response := authenticatedRequest(t, a.handleAccountUpdate, http.MethodPut,
		`{"username":"new-owner","currentPassword":"wrong-password","newPassword":"new-strong-password-456"}`, session.CSRF, cookies[0])
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong current password returned %d: %s", response.Code, response.Body.String())
	}
	response = authenticatedRequest(t, a.handleAccountUpdate, http.MethodPut,
		`{"username":"new-owner","currentPassword":"strong-password-123","newPassword":"new-strong-password-456"}`, session.CSRF, cookies[0])
	if response.Code != http.StatusOK {
		t.Fatalf("credential update failed: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(cookies[0])
	if oldSession, _ := a.session(request); oldSession != nil {
		t.Fatal("old session remained valid after credential update")
	}
	administrator, err := a.administrator()
	if err != nil || administrator.Username != "new-owner" {
		t.Fatalf("administrator was not updated: %+v err=%v", administrator, err)
	}
	candidate, err := derivePassword("new-strong-password-456", administrator.Salt)
	if err != nil || candidate != administrator.Hash {
		t.Fatal("new password hash does not match")
	}
}

func TestPHPPackagesByVersion(t *testing.T) {
	php85 := phpPackages("php85")
	if slices.Contains(php85, "php85-opcache") {
		t.Fatal("PHP 8.5 OPcache is built in and must not be installed as a package")
	}
	if !slices.Contains(php85, "php85-fpm") || !slices.Contains(php85, "php85-mysqli") {
		t.Fatal("PHP 8.5 base package list is incomplete")
	}
	if !slices.Contains(phpPackages("php84"), "php84-opcache") {
		t.Fatal("PHP 8.4 must retain its separate OPcache package")
	}
	if phpService("php85") != "php-fpm85" || phpService("php84") != "php-fpm84" {
		t.Fatal("PHP OpenRC service name is invalid")
	}
}

func TestSystemOverviewParsers(t *testing.T) {
	memory := parseMemoryInfo("MemTotal: 2048000 kB\nMemFree: 128000 kB\nMemAvailable: 1024000 kB\n")
	if memory.Total != 2048000*1024 || memory.Available != 1024000*1024 || memory.Used != 1024000*1024 {
		t.Fatalf("unexpected memory usage: %+v", memory)
	}

	network := parseNetworkDev("Inter-| Receive | Transmit\n  lo: 100 0 0 0 0 0 0 0 200 0\neth0: 1024 0 0 0 0 0 0 0 2048 0\ndocker0: 4096 0 0 0 0 0 0 0 8192 0\n", map[string]bool{"eth0": true})
	if network.ReceivedBytes != 1024 || network.TransmittedBytes != 2048 {
		t.Fatalf("unexpected network usage: %+v", network)
	}

	loads := parseLoadAverage("0.25 0.50 0.75 1/100 123\n")
	if len(loads) != 3 || loads[0] != 0.25 || loads[2] != 0.75 {
		t.Fatalf("unexpected load averages: %v", loads)
	}
}

func TestCPUUsageCalculation(t *testing.T) {
	before, err := parseCPUStat("cpu  100 0 50 850 0 0 0 0 0 0\ncpu0 50 0 25 425\n")
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseCPUStat("cpu  140 0 70 890 0 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if usage := cpuPercent(before, after); usage != 60 {
		t.Fatalf("unexpected CPU usage: %.2f", usage)
	}
}

func TestContainerListAndInspectParsing(t *testing.T) {
	list, err := parseContainerList(`{"ID":"abc123","Names":"web","Image":"nginx:alpine","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:8080->80/tcp","Networks":"frontend","CreatedAt":"2026-07-21 10:00:00 +0800 CST","Labels":"com.docker.compose.project=demo"}`)
	if err != nil || len(list) != 1 {
		t.Fatalf("container list parsing failed: %+v, %v", list, err)
	}
	if list[0].Name != "web" || list[0].ManagedBy != "Docker Compose" || list[0].State != "running" {
		t.Fatalf("unexpected container list item: %+v", list[0])
	}
	applyContainerStats(list, `{"Container":"abc123","Name":"web","CPUPerc":"1.25%","MemUsage":"32MiB / 256MiB","MemPerc":"12.5%","NetIO":"1MB / 2MB","PIDs":"8"}`)
	if list[0].CPUPercent != "1.25%" || list[0].MemoryUsage != "32MiB / 256MiB" || list[0].NetworkIO != "1MB / 2MB" {
		t.Fatalf("container stats were not applied: %+v", list[0])
	}

	detail, err := parseContainerInspect(`[{"Id":"abc123","Name":"/web","Created":"2026-07-21T02:00:00Z","RestartCount":2,"State":{"Status":"running","Running":true,"Paused":false,"ExitCode":0,"StartedAt":"2026-07-21T02:01:00Z"},"Config":{"Hostname":"abc123","Image":"nginx:alpine","Env":["MODE=prod"],"Cmd":["nginx","-g","daemon off;"],"Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":"web"}},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped","MaximumRetryCount":0},"Memory":268435456,"NanoCpus":1500000000,"NetworkMode":"frontend","Privileged":false,"ReadonlyRootfs":true},"Mounts":[{"Type":"bind","Source":"/srv/web","Destination":"/usr/share/nginx/html","Mode":"ro","RW":false}],"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]},"Networks":{"frontend":{}}}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Name != "web" || detail.CPUs != 1.5 || detail.MemoryBytes != 268435456 || detail.ComposeProject != "demo" || len(detail.Ports) != 1 || len(detail.Mounts) != 1 {
		t.Fatalf("unexpected container detail: %+v", detail)
	}
}

func TestContainerSettingsValidation(t *testing.T) {
	valid := containerSettings{Name: "web-1", RestartPolicy: "unless-stopped", CPUs: 1.5, MemoryMB: 256}
	if err := validateContainerSettings(&valid); err != nil {
		t.Fatalf("valid container settings rejected: %v", err)
	}
	for _, settings := range []containerSettings{
		{Name: "../bad", RestartPolicy: "always"},
		{Name: "web", RestartPolicy: "sometimes"},
		{Name: "web", RestartPolicy: "no", CPUs: -1},
		{Name: "web", RestartPolicy: "no", MemoryMB: 5},
	} {
		if err := validateContainerSettings(&settings); err == nil {
			t.Fatalf("invalid container settings accepted: %+v", settings)
		}
	}
	if err := validateContainerIdentifier("../../etc/passwd"); err == nil {
		t.Fatal("unsafe container identifier accepted")
	}
}

func TestServerSettingsValidation(t *testing.T) {
	hostname, err := validateHostname("Web-01.Example.COM")
	if err != nil || hostname != "web-01.example.com" {
		t.Fatalf("valid hostname rejected: %q, %v", hostname, err)
	}
	for _, value := range []string{"", "-server", "server-", "server_name", "server;reboot", strings.Repeat("a", 64) + ".com"} {
		if _, err := validateHostname(value); err == nil {
			t.Fatalf("invalid hostname accepted: %q", value)
		}
	}
	if timezone, err := validateTimezone("Asia/Shanghai"); err != nil || timezone != "Asia/Shanghai" {
		t.Fatalf("valid timezone rejected: %q, %v", timezone, err)
	}
	for _, value := range []string{"", "../etc/passwd", "/etc/localtime", "Missing/Timezone"} {
		if _, err := validateTimezone(value); err == nil {
			t.Fatalf("invalid timezone accepted: %q", value)
		}
	}
}

func TestSwapParsingAndFstabRendering(t *testing.T) {
	swaps := parseSwaps("Filename Type Size Used Priority\n/swapfile file 2097148 160000 -2\n" + hostDeskSwapPath + " file 1048572 0 -3\n")
	if len(swaps) != 2 || !swaps[0].Active || swaps[0].Managed || !swaps[1].Managed || swaps[1].SizeBytes != 1048572*1024 {
		t.Fatalf("unexpected swap parsing result: %+v", swaps)
	}
	original := []byte("UUID=root / ext4 defaults 0 1\n/swapfile none swap defaults 0 0\n")
	enabled := renderManagedSwapFstab(original, true)
	if strings.Count(string(enabled), hostDeskSwapMarker) != 1 || strings.Count(string(enabled), hostDeskSwapPath+" none swap defaults 0 0") != 1 {
		t.Fatalf("managed swap entry was not added once:\n%s", enabled)
	}
	disabled := renderManagedSwapFstab(enabled, false)
	if strings.Contains(string(disabled), hostDeskSwapPath) || !strings.Contains(string(disabled), "/swapfile none swap defaults 0 0") {
		t.Fatalf("managed swap removal changed unrelated entries:\n%s", disabled)
	}
}

func TestAdminInputValidation(t *testing.T) {
	validSite := siteDefinition{Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787"}
	if err := validateSite(&validSite); err != nil {
		t.Fatalf("valid proxy site rejected: %v", err)
	}
	for _, site := range []siteDefinition{
		{Domain: "invalid", Type: "static"},
		{Domain: "example.com", Type: "proxy", Upstream: "file:///etc/passwd"},
		{Domain: "example.com", Type: "static", Root: "/etc"},
	} {
		if err := validateSite(&site); err == nil {
			t.Fatalf("invalid site accepted: %+v", site)
		}
	}
	if err := validateNginxSettings(nginxSettings{ClientMaxBodySize: "64m", KeepaliveTimeout: 65}); err != nil {
		t.Fatalf("valid nginx settings rejected: %v", err)
	}
	if err := validateNginxSettings(nginxSettings{ClientMaxBodySize: "0m", KeepaliveTimeout: 65}); err == nil {
		t.Fatal("invalid nginx size accepted")
	}
	if err := validatePHPSettings(phpSettings{UploadMaxFilesize: "64m", PostMaxSize: "64m", MemoryLimit: "256m", MaxExecutionTime: 60}); err != nil {
		t.Fatalf("valid PHP settings rejected: %v", err)
	}
	if err := validatePHPSettings(phpSettings{UploadMaxFilesize: "64m", PostMaxSize: "64m", MemoryLimit: "256m", MaxExecutionTime: 3601}); err == nil {
		t.Fatal("invalid PHP execution time accepted")
	}
	customRewrite := siteDefinition{
		Domain: "example.com", Type: "php", Root: "/var/www/example.com/public",
		RewriteMode: "custom", RewriteRules: "rewrite ^/old$ /new permanent;",
	}
	if err := validateSite(&customRewrite); err != nil {
		t.Fatalf("valid custom rewrite rejected: %v", err)
	}
	for _, rules := range []string{"include /etc/nginx/nginx.conf;", "rewrite ^ /index.php last; include /etc/nginx/nginx.conf;", "rewrite ^ /index.php last;\n}"} {
		invalid := customRewrite
		invalid.RewriteRules = rules
		if err := validateSite(&invalid); err == nil {
			t.Fatalf("unsafe rewrite rules accepted: %q", rules)
		}
	}
}

func TestRenderPHPRewritePresets(t *testing.T) {
	base := siteDefinition{Domain: "example.com", Type: "php", Root: "/var/www/example.com/public"}
	tests := []struct {
		mode string
		want string
	}{
		{"", "try_files $uri $uri/ /index.php?$query_string;"},
		{"none", "try_files $uri $uri/ =404;"},
		{"wordpress", "try_files $uri $uri/ /index.php?$args;"},
		{"thinkphp", "try_files $uri $uri/ /index.php?s=$uri&$args;"},
	}
	for _, test := range tests {
		site := base
		site.RewriteMode = test.mode
		if config := renderSiteConfig(site, defaultNginxSettings()); !strings.Contains(config, test.want) {
			t.Fatalf("rewrite mode %q missing %q", test.mode, test.want)
		}
	}
	site := base
	site.RewriteMode = "custom"
	site.RewriteRules = "rewrite ^/old$ /new permanent;"
	if config := renderSiteConfig(site, defaultNginxSettings()); !strings.Contains(config, "        rewrite ^/old$ /new permanent;") {
		t.Fatal("custom rewrite was not rendered inside the location block")
	}
}

func TestProtectedDatabaseUsers(t *testing.T) {
	for _, user := range []string{"", "root", "ROOT", "mariadb.sys", "mysql", "PUBLIC"} {
		if !protectedDatabaseUser(user) {
			t.Fatalf("database user %q must be protected", user)
		}
	}
	if protectedDatabaseUser("application_user") {
		t.Fatal("application database user must remain manageable")
	}
}

func TestRenderSiteConfigIncludesManagedSettings(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787"},
		nginxSettings{ClientMaxBodySize: "64m", KeepaliveTimeout: 65, Gzip: true},
	)
	for _, directive := range []string{"client_max_body_size 64m;", "keepalive_timeout 65;", "gzip on;"} {
		if !strings.Contains(config, directive) {
			t.Fatalf("managed site config is missing %q", directive)
		}
	}
}

func TestRenderSSLConfigUsesPrivateSessionCache(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787", SSL: true, Certificate: "/cert.pem", PrivateKey: "/key.pem"},
		defaultNginxSettings(),
	)
	if !strings.Contains(config, "shared:HostDeskSSL:10m") || strings.Contains(config, "shared:SSL:10m") {
		t.Fatal("HostDesk TLS cache must not conflict with Alpine's shared SSL cache")
	}
}

func TestRenderSiteConfigExposesACMEChallenge(t *testing.T) {
	config := renderSiteConfig(
		siteDefinition{ID: "example-com", Domain: "example.com", Type: "proxy", Upstream: "http://127.0.0.1:8787", SSL: true, Certificate: "/cert.pem", PrivateKey: "/key.pem"},
		defaultNginxSettings(),
	)
	if strings.Count(config, "location ^~ /.well-known/acme-challenge/") != 2 || !strings.Contains(config, acmeHTTPRoot) {
		t.Fatal("HTTP-01 challenge must be served by HTTP and HTTPS server blocks")
	}
}

func TestCertificateDomainValidation(t *testing.T) {
	domains, err := normalizeCertificateDomains([]string{"example.com", "www.example.com", "example.com"}, "http")
	if err != nil || len(domains) != 2 {
		t.Fatalf("valid certificate domains rejected: %v, %v", domains, err)
	}
	if _, err := normalizeCertificateDomains([]string{"*.example.com"}, "http"); err == nil {
		t.Fatal("HTTP-01 must reject wildcard domains")
	}
	if _, err := normalizeCertificateDomains([]string{"*.example.com"}, "dns-cloudflare"); err != nil {
		t.Fatalf("DNS-01 wildcard rejected: %v", err)
	}
}

func TestCredentialEncryption(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	encrypted, err := a.encryptCredential("cloudflare-secret-token")
	if err != nil || strings.Contains(string(encrypted), "cloudflare-secret-token") {
		t.Fatalf("credential was not encrypted: %v", err)
	}
	plain, err := a.decryptCredential(encrypted)
	if err != nil || plain != "cloudflare-secret-token" {
		t.Fatalf("credential decrypt failed: %q, %v", plain, err)
	}
}

func TestFileSnapshotRestore(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(filename, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	restoreFiles(snapshot)
	data, err := os.ReadFile(filename)
	if err != nil || string(data) != "old\n" {
		t.Fatalf("snapshot was not restored: %q, %v", data, err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode was not restored: %v, %v", info.Mode().Perm(), err)
	}
}

func TestCleanRelativeRejectsTraversal(t *testing.T) {
	unsafe := []string{"../etc/passwd", "docs/../../secret", `..\\secret`}
	for _, value := range unsafe {
		if _, err := cleanRelative(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
	if value, err := cleanRelative("/docs/./readme.md"); err != nil || value != "docs/readme.md" {
		t.Fatalf("unexpected normalization: %q, %v", value, err)
	}
}

func TestSafeArchiveName(t *testing.T) {
	for _, value := range []string{"../etc/passwd", "/etc/passwd", `C:\\Windows\\file`} {
		if safeArchiveName(value) {
			t.Fatalf("expected %q to be unsafe", value)
		}
	}
	if !safeArchiveName("folder/file.txt") {
		t.Fatal("safe path rejected")
	}
}

func TestExtractZipRejectsTraversalAndLinks(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "bad.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	item, _ := writer.Create("../escape.txt")
	_, _ = item.Write([]byte("bad"))
	_ = writer.Close()
	_ = file.Close()
	a := &app{extractMax: defaultExtractMax}
	if err := a.extractZip(archive, root); err == nil {
		t.Fatal("expected traversal archive to be rejected")
	}
}

func TestExtractTarRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "bad.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	writer := tar.NewWriter(gzipWriter)
	_ = writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0777})
	_ = writer.Close()
	_ = gzipWriter.Close()
	_ = file.Close()
	a := &app{extractMax: defaultExtractMax}
	if err := a.extractTar(archive, root, true); err == nil {
		t.Fatal("expected symlink archive to be rejected")
	}
}

func TestPrepareExtractPathRejectsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := prepareExtractPath(root, filepath.Join(root, "linked", "file.txt"), false); err == nil {
		t.Fatal("expected existing symlink to be rejected")
	}
}
