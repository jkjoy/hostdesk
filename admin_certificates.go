package main

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"
)

const (
	acmeHTTPRoot   = "/var/lib/hostdesk/acme-http"
	certificateDir = "/etc/hostdesk/certificates"
	renewBefore    = 30 * 24 * time.Hour
)

type certificateRecord struct {
	ID                string      `json:"id"`
	SiteID            string      `json:"siteId"`
	Domains           []string    `json:"domains"`
	Email             string      `json:"email"`
	Challenge         string      `json:"challenge"`
	Provider          string      `json:"provider,omitempty"`
	EncryptedDNS      tokenString `json:"encryptedDnsToken,omitempty"`
	Certificate       string      `json:"certificate"`
	PrivateKey        string      `json:"privateKey"`
	IssuerCertificate string      `json:"issuerCertificate,omitempty"`
	IssuedAt          time.Time   `json:"issuedAt"`
	ExpiresAt         time.Time   `json:"expiresAt"`
	LastAttempt       time.Time   `json:"lastAttempt,omitempty"`
	LastError         string      `json:"lastError,omitempty"`
	AutoRenew         bool        `json:"autoRenew"`
}

// tokenString keeps encrypted credentials distinct from ordinary display data.
type tokenString string

type certificateRequest struct {
	SiteID    string   `json:"siteId"`
	Domains   []string `json:"domains"`
	Email     string   `json:"email"`
	Challenge string   `json:"challenge"`
	AutoRenew bool     `json:"autoRenew"`
}

type certificateView struct {
	ID          string    `json:"id"`
	SiteID      string    `json:"siteId"`
	Domains     []string  `json:"domains"`
	Email       string    `json:"email"`
	Challenge   string    `json:"challenge"`
	Provider    string    `json:"provider,omitempty"`
	IssuedAt    time.Time `json:"issuedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	AutoRenew   bool      `json:"autoRenew"`
	RenewalDue  bool      `json:"renewalDue"`
}

type acmeAccountDisk struct {
	Email        string                 `json:"email"`
	PrivateKey   string                 `json:"privateKey"`
	Registration *registration.Resource `json:"registration,omitempty"`
}

type acmeUser struct {
	email        string
	registration *registration.Resource
	privateKey   crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.privateKey }

type webrootProvider struct{ root string }

func (p webrootProvider) challengePath(token string) (string, error) {
	if token == "" || filepath.Base(token) != token || strings.ContainsAny(token, "/\\") {
		return "", errors.New("ACME challenge token invalid")
	}
	return filepath.Join(p.root, ".well-known", "acme-challenge", token), nil
}

func (p webrootProvider) Present(_ string, token, keyAuth string) error {
	filename, err := p.challengePath(token)
	if err != nil {
		return err
	}
	return writeAtomic(filename, []byte(keyAuth), 0644)
}

func (p webrootProvider) CleanUp(_ string, token, _ string) error {
	filename, err := p.challengePath(token)
	if err != nil {
		return nil
	}
	err = os.Remove(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *app) certificatesPath() string  { return filepath.Join(a.dataDir, "certificates.json") }
func (a *app) acmeAccountPath() string   { return filepath.Join(a.dataDir, "acme-account.json") }
func (a *app) credentialKeyPath() string { return filepath.Join(a.dataDir, "credential.key") }

func (a *app) loadCertificates() ([]certificateRecord, error) {
	data, err := os.ReadFile(a.certificatesPath())
	if errors.Is(err, os.ErrNotExist) {
		return []certificateRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records []certificateRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func (a *app) saveCertificates(records []certificateRecord) error {
	encoded, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.certificatesPath(), append(encoded, '\n'), 0600)
}

func certificateToView(record certificateRecord) certificateView {
	return certificateView{
		ID: record.ID, SiteID: record.SiteID, Domains: record.Domains, Email: record.Email,
		Challenge: record.Challenge, Provider: record.Provider, IssuedAt: record.IssuedAt, ExpiresAt: record.ExpiresAt,
		LastAttempt: record.LastAttempt, LastError: record.LastError, AutoRenew: record.AutoRenew,
		RenewalDue: record.ExpiresAt.Before(time.Now().Add(renewBefore)),
	}
}

func normalizeCertificateDomains(domains []string, challenge string) ([]string, error) {
	if len(domains) == 0 || len(domains) > 20 {
		return nil, &apiError{http.StatusBadRequest, "证书需要 1 到 20 个域名"}
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		wildcard := strings.HasPrefix(domain, "*.")
		base := strings.TrimPrefix(domain, "*.")
		if !domainPattern.MatchString(base) || (wildcard && challenge != "dns-cloudflare") {
			return nil, &apiError{http.StatusBadRequest, "域名格式无效；通配符证书只能使用 DNS 验证"}
		}
		if !seen[domain] {
			seen[domain] = true
			result = append(result, domain)
		}
	}
	return result, nil
}

func validateCertificateRequest(request *certificateRequest, sites []siteDefinition, existing *certificateRecord) (siteDefinition, error) {
	request.SiteID = strings.TrimSpace(request.SiteID)
	request.Email = strings.TrimSpace(request.Email)
	if request.Email != "" {
		address, err := mail.ParseAddress(request.Email)
		if err != nil || address.Address != request.Email {
			return siteDefinition{}, &apiError{http.StatusBadRequest, "联系邮箱格式无效"}
		}
	}
	if request.Challenge != "http" && request.Challenge != "dns-cloudflare" {
		return siteDefinition{}, &apiError{http.StatusBadRequest, "证书验证方式无效"}
	}
	domains, err := normalizeCertificateDomains(request.Domains, request.Challenge)
	if err != nil {
		return siteDefinition{}, err
	}
	request.Domains = domains
	var site *siteDefinition
	for i := range sites {
		if sites[i].ID == request.SiteID {
			site = &sites[i]
			break
		}
	}
	if site == nil {
		return siteDefinition{}, &apiError{http.StatusBadRequest, "请选择有效网站"}
	}
	allowed := map[string]bool{site.Domain: true}
	for _, alias := range site.Aliases {
		allowed[alias] = true
	}
	for _, domain := range domains {
		base := strings.TrimPrefix(domain, "*.")
		if !allowed[base] {
			return siteDefinition{}, &apiError{http.StatusBadRequest, "证书域名必须属于所选网站"}
		}
	}
	if request.Challenge == "http" && !site.Enabled {
		return siteDefinition{}, &apiError{http.StatusConflict, "HTTP 验证要求网站处于启用状态"}
	}
	return *site, nil
}

func (a *app) credentialKey() ([]byte, error) {
	data, err := os.ReadFile(a.credentialKeyPath())
	if err == nil {
		if len(data) != 32 {
			return nil, errors.New("credential encryption key invalid")
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data = make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return nil, err
	}
	if err := writeAtomic(a.credentialKeyPath(), data, 0600); err != nil {
		return nil, err
	}
	return data, nil
}

func (a *app) encryptCredential(value string) (tokenString, error) {
	key, err := a.credentialKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return tokenString(base64.RawURLEncoding.EncodeToString(sealed)), nil
}

func (a *app) decryptCredential(value tokenString) (string, error) {
	key, err := a.credentialKey()
	if err != nil {
		return "", err
	}
	data, err := base64.RawURLEncoding.DecodeString(string(value))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize() {
		return "", errors.New("encrypted credential invalid")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func encodeECPrivateKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func parseECPrivateKey(value string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("ACME account key invalid")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func (a *app) loadACMEUser(email string) (*acmeUser, *acmeAccountDisk, error) {
	data, err := os.ReadFile(a.acmeAccountPath())
	if errors.Is(err, os.ErrNotExist) {
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			return nil, nil, keyErr
		}
		encoded, keyErr := encodeECPrivateKey(key)
		if keyErr != nil {
			return nil, nil, keyErr
		}
		disk := &acmeAccountDisk{Email: email, PrivateKey: encoded}
		return &acmeUser{email: email, privateKey: key}, disk, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var disk acmeAccountDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, nil, err
	}
	key, err := parseECPrivateKey(disk.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	if email != "" {
		disk.Email = email
	}
	return &acmeUser{email: disk.Email, registration: disk.Registration, privateKey: key}, &disk, nil
}

func (a *app) saveACMEAccount(disk *acmeAccountDisk) error {
	encoded, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.acmeAccountPath(), append(encoded, '\n'), 0600)
}

func (a *app) newACMEClient(email, challenge string, credentials dnsProviderCredentials) (*lego.Client, error) {
	user, disk, err := a.loadACMEUser(email)
	if err != nil {
		return nil, err
	}
	config := lego.NewConfig(user)
	config.Certificate.KeyType = certcrypto.EC256
	config.Certificate.Timeout = 2 * time.Minute
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}
	switch challenge {
	case "http":
		err = client.Challenge.SetHTTP01Provider(webrootProvider{root: acmeHTTPRoot})
	case "dns-cloudflare":
		providerConfig := cloudflare.NewDefaultConfig()
		providerConfig.AuthToken = credentials.Token
		providerConfig.ZoneToken = credentials.Token
		provider, providerErr := cloudflare.NewDNSProviderConfig(providerConfig)
		if providerErr != nil {
			return nil, providerErr
		}
		err = client.Challenge.SetDNS01Provider(provider)
	default:
		err = errors.New("unsupported ACME challenge")
	}
	if err != nil {
		return nil, err
	}
	if user.registration == nil {
		resource, registerErr := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if registerErr != nil {
			return nil, registerErr
		}
		user.registration = resource
		disk.Registration = resource
	}
	if err := a.saveACMEAccount(disk); err != nil {
		return nil, err
	}
	return client, nil
}

func parseCertificatePeriod(data []byte) (time.Time, time.Time, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, time.Time{}, errors.New("issued certificate PEM invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return cert.NotBefore, cert.NotAfter, nil
}

func (a *app) activateSiteCertificate(siteID, certPath, keyPath string) error {
	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	index := -1
	for i := range sites {
		if sites[i].ID == siteID {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("certificate site no longer exists")
	}
	previousSites := append([]siteDefinition(nil), sites...)
	previous := sites[index]
	sites[index].SSL = true
	sites[index].Certificate = certPath
	sites[index].PrivateKey = keyPath
	if err := a.saveSites(sites); err != nil {
		return err
	}
	if err := a.applySite(sites[index], &previous); err != nil {
		return a.restoreSites(previousSites, err)
	}
	return nil
}

func (a *app) issueCertificate(record certificateRecord) (certificateRecord, error) {
	if record.Challenge == "http" {
		sites, err := a.loadSites()
		if err != nil {
			return record, err
		}
		for i := range sites {
			if sites[i].ID == record.SiteID {
				if err := os.MkdirAll(filepath.Join(acmeHTTPRoot, ".well-known", "acme-challenge"), 0755); err != nil {
					return record, err
				}
				if err := a.applySite(sites[i], &sites[i]); err != nil {
					return record, err
				}
				break
			}
		}
	}
	var credentials dnsProviderCredentials
	if strings.HasPrefix(record.Challenge, "dns-") {
		provider := record.Provider
		if provider == "" {
			provider = strings.TrimPrefix(record.Challenge, "dns-")
		}
		var err error
		credentials, err = a.dnsProviderCredentials(provider, record.EncryptedDNS)
		if err != nil {
			return record, err
		}
	}
	client, err := a.newACMEClient(record.Email, record.Challenge, credentials)
	if err != nil {
		return record, err
	}
	resource, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: record.Domains, Bundle: true})
	if err != nil {
		return record, err
	}
	issuedAt, expiresAt, err := parseCertificatePeriod(resource.Certificate)
	if err != nil {
		return record, err
	}
	dir := filepath.Join(certificateDir, record.ID)
	certPath := filepath.Join(dir, "fullchain.pem")
	keyPath := filepath.Join(dir, "privkey.pem")
	issuerPath := filepath.Join(dir, "issuer.pem")
	certSnapshot, err := captureFile(certPath)
	if err != nil {
		return record, err
	}
	keySnapshot, err := captureFile(keyPath)
	if err != nil {
		return record, err
	}
	issuerSnapshot, err := captureFile(issuerPath)
	if err != nil {
		return record, err
	}
	snapshots := []fileSnapshot{certSnapshot, keySnapshot, issuerSnapshot}
	if err = writeAtomic(certPath, resource.Certificate, 0644); err == nil {
		err = writeAtomic(keyPath, resource.PrivateKey, 0600)
	}
	if err == nil && len(resource.IssuerCertificate) > 0 {
		err = writeAtomic(issuerPath, resource.IssuerCertificate, 0644)
	}
	if err == nil {
		err = a.activateSiteCertificate(record.SiteID, certPath, keyPath)
	}
	if err != nil {
		restoreFiles(snapshots...)
		_ = nginxReloadIfRunning()
		return record, err
	}
	record.Certificate = certPath
	record.PrivateKey = keyPath
	record.IssuerCertificate = issuerPath
	record.IssuedAt = issuedAt
	record.ExpiresAt = expiresAt
	record.LastAttempt = time.Now()
	record.LastError = ""
	return record, nil
}

func upsertCertificate(records []certificateRecord, record certificateRecord) []certificateRecord {
	for i := range records {
		if records[i].ID == record.ID {
			records[i] = record
			return records
		}
	}
	return append(records, record)
}

func (a *app) handleCertificatesList(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, false) == nil {
		return
	}
	records, err := a.loadCertificates()
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]certificateView, 0, len(records))
	for _, record := range records {
		views = append(views, certificateToView(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificates": views, "renewBeforeDays": int(renewBefore.Hours() / 24)})
}

func (a *app) handleCertificateCreate(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	var request certificateRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if strings.TrimSpace(request.Email) == "" {
		defaultEmail, settingErr := a.appSetting(settingACMEEmail)
		if settingErr != nil {
			writeError(w, settingErr)
			return
		}
		request.Email = defaultEmail
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
	var existing *certificateRecord
	for i := range records {
		if records[i].ID == request.SiteID {
			existing = &records[i]
			break
		}
	}
	_, err = validateCertificateRequest(&request, sites, existing)
	if err != nil {
		writeError(w, err)
		return
	}
	record := certificateRecord{
		ID: request.SiteID, SiteID: request.SiteID, Domains: request.Domains, Email: request.Email,
		Challenge: request.Challenge, AutoRenew: request.AutoRenew,
	}
	if strings.HasPrefix(request.Challenge, "dns-") {
		record.Provider = strings.TrimPrefix(request.Challenge, "dns-")
		var legacyCredential tokenString
		if existing != nil {
			legacyCredential = existing.EncryptedDNS
		}
		if _, credentialErr := a.dnsProviderCredentials(record.Provider, legacyCredential); credentialErr != nil {
			writeError(w, credentialErr)
			return
		}
	}
	if existing != nil {
		record.Certificate = existing.Certificate
		record.PrivateKey = existing.PrivateKey
		record.IssuerCertificate = existing.IssuerCertificate
		record.IssuedAt = existing.IssuedAt
		record.ExpiresAt = existing.ExpiresAt
		record.EncryptedDNS = existing.EncryptedDNS
	}
	record, err = a.issueCertificate(record)
	if err != nil {
		writeError(w, &apiError{http.StatusBadGateway, "证书申请失败：" + err.Error()})
		return
	}
	records = upsertCertificate(records, record)
	if err := a.saveCertificates(records); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, certificateToView(record))
}

func (a *app) renewCertificate(id string) (certificateRecord, error) {
	records, err := a.loadCertificates()
	if err != nil {
		return certificateRecord{}, err
	}
	index := -1
	for i := range records {
		if records[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return certificateRecord{}, &apiError{http.StatusNotFound, "证书不存在"}
	}
	records[index].LastAttempt = time.Now()
	record, issueErr := a.issueCertificate(records[index])
	if issueErr != nil {
		records[index].LastAttempt = time.Now()
		records[index].LastError = issueErr.Error()
		_ = a.saveCertificates(records)
		return certificateRecord{}, issueErr
	}
	records[index] = record
	if err := a.saveCertificates(records); err != nil {
		return certificateRecord{}, err
	}
	return record, nil
}

func (a *app) handleCertificateRenew(w http.ResponseWriter, r *http.Request) {
	if a.authorize(w, r, true) == nil {
		return
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	record, err := a.renewCertificate(r.PathValue("id"))
	if err != nil {
		writeError(w, &apiError{http.StatusBadGateway, "证书续期失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, certificateToView(record))
}

func (a *app) renewDueCertificates() {
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	records, err := a.loadCertificates()
	if err != nil {
		log.Printf("证书自动续期读取失败：%v", err)
		return
	}
	now := time.Now()
	for _, record := range records {
		if !record.AutoRenew || record.ExpiresAt.After(now.Add(renewBefore)) || (!record.LastAttempt.IsZero() && record.LastAttempt.After(now.Add(-24*time.Hour))) {
			continue
		}
		if _, err := a.renewCertificate(record.ID); err != nil {
			log.Printf("证书 %s 自动续期失败：%v", record.ID, err)
		} else {
			log.Printf("证书 %s 自动续期完成", record.ID)
		}
	}
}

func (a *app) certificateRenewalLoop() {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		<-timer.C
		a.renewDueCertificates()
		timer.Reset(12 * time.Hour)
	}
}
