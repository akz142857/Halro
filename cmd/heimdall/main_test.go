package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestHealthcheckAcceptsOnlySuccessfulLoopbackReadiness(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusServiceUnavailable
		if request.URL.Path == "/ready" {
			status = http.StatusOK
		} else if request.URL.Path == "/redirect" {
			status = http.StatusFound
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("health")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/ready", time.Second, client); err != nil {
		t.Fatal(err)
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/unready", time.Second, client); err == nil {
		t.Fatal("unready endpoint passed healthcheck")
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/redirect", time.Second, client); err == nil {
		t.Fatal("redirect passed healthcheck")
	}
	if err := runHealthcheckWithClient("https://example.com/health/ready", time.Second, client); err == nil {
		t.Fatal("non-loopback healthcheck URL was accepted")
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/ready?token=secret", time.Second, client); err == nil {
		t.Fatal("healthcheck URL with query was accepted")
	}
}
