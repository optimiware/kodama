package identity

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
)

// DiscoveryDocumentFor builds the Kodama discovery document bound to one
// fixture provider and API URL. It is a fixture of the public discovery facts,
// not the composed Shell.
func DiscoveryDocumentFor(provider *Provider, apiURL, clientID string, scopes ...string) api.DiscoveryDocument {
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile"}
	}
	return api.DiscoveryDocument{
		Issuer:                      provider.Issuer(),
		ClientID:                    clientID,
		Audience:                    provider.Audience(),
		AuthorizationEndpoint:       provider.AuthorizationEndpoint(),
		TokenEndpoint:               provider.TokenEndpoint(),
		DeviceAuthorizationEndpoint: provider.DeviceAuthorizationEndpoint(),
		RevocationEndpoint:          provider.RevocationEndpoint(),
		Scopes:                      append([]string(nil), scopes...),
		APIURL:                      apiURL,
	}
}

// DiscoveryServer serves the fixed Kodama public discovery route over loopback
// TLS. A child process trusts it through SSL_CERT_FILE. The document can be
// changed after a login, which is how the endpoint-pinning tests model a hostile
// discovery response.
type DiscoveryServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	document api.DiscoveryDocument
	status   int
	delay    time.Duration
	drop     bool
	requests int
}

// NewDiscoveryServer starts one loopback TLS discovery server. An empty API URL
// is filled with the server's own URL.
func NewDiscoveryServer(options DiscoveryOptions) (*DiscoveryServer, error) {
	server := &DiscoveryServer{document: options.Document, delay: options.Delay}
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.handle)
	server.server = httptest.NewTLSServer(mux)
	if server.document.APIURL == "" {
		server.document.APIURL = server.server.URL
	}
	return server, nil
}

// DiscoveryOptions configures one discovery fixture.
type DiscoveryOptions struct {
	Document api.DiscoveryDocument
	Delay    time.Duration
}

// URL is the API origin a client configures as its server URL.
func (server *DiscoveryServer) URL() string {
	if server == nil || server.server == nil {
		return ""
	}
	return server.server.URL
}

// Document returns the current document.
func (server *DiscoveryServer) Document() api.DiscoveryDocument {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.document
}

// SetDocument replaces the served document.
func (server *DiscoveryServer) SetDocument(document api.DiscoveryDocument) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.document = document
}

// SetStatus makes the discovery route fail with status. Zero clears it.
func (server *DiscoveryServer) SetStatus(status int) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.status = status
}

// SetDelay delays the discovery response.
func (server *DiscoveryServer) SetDelay(delay time.Duration) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.delay = delay
}

// SetDrop makes the discovery route close the connection without a response.
func (server *DiscoveryServer) SetDrop(drop bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.drop = drop
}

// RequestCount returns how many discovery requests were served.
func (server *DiscoveryServer) RequestCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.requests
}

// Client returns the TLS client trusting the fixture certificate.
func (server *DiscoveryServer) Client() *http.Client {
	return server.server.Client()
}

// CACertPEM returns the fixture certificate as a PEM certificate.
func (server *DiscoveryServer) CACertPEM() []byte {
	if server == nil || server.server == nil || server.server.Certificate() == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.server.Certificate().Raw})
}

// Close stops the fixture server.
func (server *DiscoveryServer) Close() {
	if server == nil || server.server == nil {
		return
	}
	server.server.Close()
}

func (server *DiscoveryServer) handle(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	status := server.status
	delay := server.delay
	drop := server.drop
	document := server.document
	server.requests++
	server.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if drop {
		if hijacker, ok := writer.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				_ = connection.Close()
				return
			}
		}
		return
	}
	if status != 0 {
		writer.WriteHeader(status)
		return
	}
	if request.Method != http.MethodGet || request.URL.Path != api.DiscoveryPath {
		http.NotFound(writer, request)
		return
	}
	writeJSON(writer, http.StatusOK, document)
}
