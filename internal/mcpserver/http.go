package mcpserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"time"
)

func httpTransport(ca []byte) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("mcpserver: default HTTP transport is not configurable")
	}
	t := base.Clone()
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("mcpserver: CA bundle contains no certificates")
		}
		t.TLSClientConfig.RootCAs = pool
	}
	return t, nil
}

func httpClient(timeout time.Duration, ca []byte) (*http.Client, error) {
	transport, err := httpTransport(ca)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
