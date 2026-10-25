package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/optimiware/kodama/test/support/stage/probe"
)

func main() {
	mode := flag.String("mode", "", "proxy, kms or kms-fresh")
	flag.Parse()
	if *mode == "proxy" {
		ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		if err != nil {
			fail()
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			fail()
		}
		origin, _ := url.Parse("https://kubernetes.default.svc")
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		h := &probe.Proxy{Origin: origin, Client: client}
		s := &http.Server{Addr: "127.0.0.1:6444", Handler: h, ReadHeaderTimeout: 10 * time.Second}
		if s.ListenAndServeTLS("/fault/tls.crt", "/fault/tls.key") != nil {
			fail()
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var err error
	switch *mode {
	case "kms":
		err = probe.KMS(ctx)
	case "kms-fresh":
		err = probe.FreshKMS(ctx, os.Stdin)
	default:
		fail()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fail() {
	fmt.Fprintln(os.Stderr, "stageprobe: configuration or listener failed (details suppressed)")
	os.Exit(1)
}
