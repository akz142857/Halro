package deadman

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func secureClient(tlsConfig TLSConfig, timeout time.Duration) (*http.Client, error) {
	caPEM, err := os.ReadFile(tlsConfig.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file contains no certificates")
	}
	configuration := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: tlsConfig.ServerName}
	if tlsConfig.ClientCertFile != "" {
		if err := requirePrivateFile(tlsConfig.ClientKeyFile); err != nil {
			return nil, fmt.Errorf("client private key: %w", err)
		}
		certificate, err := tls.LoadX509KeyPair(tlsConfig.ClientCertFile, tlsConfig.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		configuration.Certificates = []tls.Certificate{certificate}
	}
	transport := &http.Transport{TLSClientConfig: configuration, Proxy: nil, DisableKeepAlives: false, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirects are forbidden")
		},
	}, nil
}

func token(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if err := requirePrivateFile(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" || len(value) > 16<<10 {
		return "", errors.New("bearer token file is empty or too large")
	}
	return value, nil
}

func requirePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("must not be accessible by group or other users")
	}
	return nil
}

func request(ctx context.Context, client *http.Client, method, rawURL, tokenFile string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	credential, err := token(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read bearer credential: %w", err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func checkTargetWithClient(ctx context.Context, cfg Config, target TargetConfig, client *http.Client) (time.Duration, string) {
	started := time.Now()
	checkContext, cancel := context.WithTimeout(ctx, cfg.Timeout.Value())
	defer cancel()
	if client == nil {
		return time.Since(started), "tls_configuration"
	}
	response, err := request(checkContext, client, http.MethodGet, target.URL, target.BearerTokenFile, nil)
	if err != nil {
		return time.Since(started), "request_failed"
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return time.Since(started), "unexpected_status"
	}
	if target.Freshness != nil {
		if reason := checkFreshness(checkContext, client, target); reason != "" {
			return time.Since(started), reason
		}
	}
	return time.Since(started), ""
}

func checkFreshness(ctx context.Context, client *http.Client, target TargetConfig) string {
	response, err := request(ctx, client, http.MethodGet, target.Freshness.URL, target.BearerTokenFile, nil)
	if err != nil {
		return "freshness_request_failed"
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "freshness_unexpected_status"
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if decoder.Decode(&payload) != nil || payload.Status != "success" || payload.Data.ResultType != "scalar" || len(payload.Data.Result) != 2 {
		return "freshness_invalid_response"
	}
	var ageString string
	if json.Unmarshal(payload.Data.Result[1], &ageString) != nil {
		return "freshness_invalid_response"
	}
	age, err := strconv.ParseFloat(ageString, 64)
	if err != nil || math.IsNaN(age) || math.IsInf(age, 0) || age < 0 || age > target.Freshness.MaxAge.Value().Seconds() {
		return "freshness_stale"
	}
	return ""
}
