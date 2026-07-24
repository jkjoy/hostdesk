package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type siteRequest struct {
	Domain          string   `json:"domain"`
	Aliases         []string `json:"aliases"`
	Type            string   `json:"type"`
	Root            string   `json:"root"`
	Upstream        string   `json:"upstream"`
	RewriteMode     string   `json:"rewriteMode"`
	RewriteRules    string   `json:"rewriteRules"`
	SSL             bool     `json:"ssl"`
	CertificateMode string   `json:"certificateMode"`
	CertificateID   string   `json:"certificateId"`
	CertificatePEM  string   `json:"certificatePem"`
	PrivateKeyPEM   string   `json:"privateKeyPem"`
}

type siteView struct {
	ID                    string   `json:"id"`
	Domain                string   `json:"domain"`
	Aliases               []string `json:"aliases"`
	Type                  string   `json:"type"`
	Root                  string   `json:"root"`
	Upstream              string   `json:"upstream"`
	RewriteMode           string   `json:"rewriteMode,omitempty"`
	RewriteRules          string   `json:"rewriteRules,omitempty"`
	Enabled               bool     `json:"enabled"`
	SSL                   bool     `json:"ssl"`
	CertificateMode       string   `json:"certificateMode,omitempty"`
	CertificateID         string   `json:"certificateId,omitempty"`
	CertificateConfigured bool     `json:"certificateConfigured"`
	CreatedAt             string   `json:"createdAt"`
}

type siteCertificateOption struct {
	ID        string    `json:"id"`
	Domains   []string  `json:"domains"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (request siteRequest) siteDefinition() siteDefinition {
	return siteDefinition{
		Domain: request.Domain, Aliases: request.Aliases, Type: request.Type, Root: request.Root,
		Upstream: request.Upstream, RewriteMode: request.RewriteMode, RewriteRules: request.RewriteRules,
		SSL: request.SSL,
	}
}

func managedCertificateID(records []certificateRecord, certificate, privateKey string) string {
	for _, record := range records {
		if record.Certificate == certificate && record.PrivateKey == privateKey && certificate != "" && privateKey != "" {
			return record.ID
		}
	}
	return ""
}

func siteToView(site siteDefinition, records []certificateRecord) siteView {
	view := siteView{
		ID: site.ID, Domain: site.Domain, Aliases: site.Aliases, Type: site.Type, Root: site.Root,
		Upstream: site.Upstream, RewriteMode: site.RewriteMode, RewriteRules: site.RewriteRules,
		Enabled: site.Enabled, SSL: site.SSL, CreatedAt: site.CreatedAt,
		CertificateConfigured: site.SSL && site.Certificate != "" && site.PrivateKey != "",
	}
	if view.CertificateConfigured {
		view.CertificateID = managedCertificateID(records, site.Certificate, site.PrivateKey)
		if view.CertificateID != "" {
			view.CertificateMode = "managed"
		} else {
			view.CertificateMode = "custom"
		}
	}
	return view
}

func siteCertificateOptions(records []certificateRecord) []siteCertificateOption {
	options := make([]siteCertificateOption, 0, len(records))
	for _, record := range records {
		if record.Certificate == "" || record.PrivateKey == "" {
			continue
		}
		if _, certErr := os.Stat(record.Certificate); certErr != nil {
			continue
		}
		if _, keyErr := os.Stat(record.PrivateKey); keyErr != nil {
			continue
		}
		options = append(options, siteCertificateOption{ID: record.ID, Domains: record.Domains, ExpiresAt: record.ExpiresAt})
	}
	return options
}

func validateCertificatePEM(certificatePEM, privateKeyPEM []byte, domains []string) error {
	if len(certificatePEM) == 0 || len(privateKeyPEM) == 0 {
		return &apiError{http.StatusBadRequest, "证书和私钥内容不能为空"}
	}
	if len(certificatePEM) > 256<<10 || len(privateKeyPEM) > 64<<10 {
		return &apiError{http.StatusBadRequest, "证书或私钥内容过大"}
	}
	if _, err := tls.X509KeyPair(certificatePEM, privateKeyPEM); err != nil {
		return &apiError{http.StatusBadRequest, "证书或私钥格式无效，或两者不匹配"}
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return &apiError{http.StatusBadRequest, "证书 PEM 格式无效"}
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return &apiError{http.StatusBadRequest, "证书内容无效"}
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return &apiError{http.StatusBadRequest, "证书尚未生效或已经过期"}
	}
	for _, domain := range domains {
		if err := certificate.VerifyHostname(domain); err != nil {
			return &apiError{http.StatusBadRequest, "证书不包含网站域名 " + domain}
		}
	}
	return nil
}

func validateCertificateFiles(certificate, privateKey string, domains []string) error {
	certificatePEM, err := os.ReadFile(certificate)
	if err != nil {
		return &apiError{http.StatusBadRequest, "无法读取证书文件"}
	}
	privateKeyPEM, err := os.ReadFile(privateKey)
	if err != nil {
		return &apiError{http.StatusBadRequest, "无法读取私钥文件"}
	}
	return validateCertificatePEM(certificatePEM, privateKeyPEM, domains)
}

func siteDomains(site siteDefinition) []string {
	return append([]string{site.Domain}, site.Aliases...)
}

func (a *app) resolveSiteCertificate(site *siteDefinition, request siteRequest, previous *siteDefinition, records []certificateRecord) ([]fileSnapshot, error) {
	if !site.SSL {
		site.Certificate = ""
		site.PrivateKey = ""
		return nil, nil
	}
	switch request.CertificateMode {
	case "managed":
		for _, record := range records {
			if record.ID != strings.TrimSpace(request.CertificateID) {
				continue
			}
			if record.Certificate == "" || record.PrivateKey == "" {
				break
			}
			if err := validateCertificateFiles(record.Certificate, record.PrivateKey, siteDomains(*site)); err != nil {
				return nil, err
			}
			site.Certificate = record.Certificate
			site.PrivateKey = record.PrivateKey
			return nil, nil
		}
		return nil, &apiError{http.StatusBadRequest, "请选择有效的已有证书"}
	case "custom":
		certificatePEM := []byte(strings.TrimSpace(request.CertificatePEM))
		privateKeyPEM := []byte(strings.TrimSpace(request.PrivateKeyPEM))
		if len(certificatePEM) == 0 && len(privateKeyPEM) == 0 && previous != nil && previous.SSL && managedCertificateID(records, previous.Certificate, previous.PrivateKey) == "" {
			if err := validateCertificateFiles(previous.Certificate, previous.PrivateKey, siteDomains(*site)); err != nil {
				return nil, err
			}
			site.Certificate = previous.Certificate
			site.PrivateKey = previous.PrivateKey
			return nil, nil
		}
		if (len(certificatePEM) == 0) != (len(privateKeyPEM) == 0) {
			return nil, &apiError{http.StatusBadRequest, "证书和私钥必须同时填写"}
		}
		if err := validateCertificatePEM(certificatePEM, privateKeyPEM, siteDomains(*site)); err != nil {
			return nil, err
		}
		directory := filepath.Join(certificateDir, "manual-"+site.ID)
		certificatePath := filepath.Join(directory, "fullchain.pem")
		privateKeyPath := filepath.Join(directory, "privkey.pem")
		certificateSnapshot, err := captureFile(certificatePath)
		if err != nil {
			return nil, err
		}
		privateKeySnapshot, err := captureFile(privateKeyPath)
		if err != nil {
			return nil, err
		}
		snapshots := []fileSnapshot{certificateSnapshot, privateKeySnapshot}
		if err := writeAtomic(certificatePath, append(certificatePEM, '\n'), 0644); err != nil {
			return nil, err
		}
		if err := writeAtomic(privateKeyPath, append(privateKeyPEM, '\n'), 0600); err != nil {
			restoreFiles(snapshots...)
			return nil, err
		}
		site.Certificate = certificatePath
		site.PrivateKey = privateKeyPath
		return snapshots, nil
	default:
		return nil, &apiError{http.StatusBadRequest, "请选择证书来源"}
	}
}

func restoreCertificateSnapshots(snapshots []fileSnapshot, err error) error {
	if err != nil && len(snapshots) > 0 {
		restoreFiles(snapshots...)
		_ = nginxReloadIfRunning()
	}
	return err
}
