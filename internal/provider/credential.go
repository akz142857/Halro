package provider

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
)

type Authorizer interface {
	Scheme() domain.CredentialScheme
	Authorize(*http.Request, []byte) error
	Close()
}

type StaticHeaderAuthorizer struct {
	mu           sync.RWMutex
	scheme       domain.CredentialScheme
	header       string
	prefix       string
	secret       []byte
	clearHeaders []string
}

func NewStaticHeaderAuthorizer(scheme domain.CredentialScheme, header, prefix string, secret []byte, clearHeaders ...string) (*StaticHeaderAuthorizer, error) {
	if scheme == "" || strings.TrimSpace(header) == "" || len(secret) == 0 {
		return nil, errors.New("credential scheme, header, and secret are required")
	}
	return &StaticHeaderAuthorizer{
		scheme: scheme, header: http.CanonicalHeaderKey(header), prefix: prefix,
		secret: bytes.Clone(secret), clearHeaders: append([]string(nil), clearHeaders...),
	}, nil
}

func (a *StaticHeaderAuthorizer) Scheme() domain.CredentialScheme { return a.scheme }
func (a *StaticHeaderAuthorizer) Authorize(request *http.Request, _ []byte) error {
	if request == nil {
		return errors.New("request is required")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.secret) == 0 {
		return errors.New("credential authorizer is closed")
	}
	for _, header := range a.clearHeaders {
		request.Header.Del(header)
	}
	request.Header.Set(a.header, a.prefix+string(a.secret))
	return nil
}
func (a *StaticHeaderAuthorizer) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.secret)
	a.secret = nil
}
