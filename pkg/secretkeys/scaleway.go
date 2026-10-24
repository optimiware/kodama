package secretkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const scalewayFormat = "scaleway-symmetric-v1"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Scaleway wraps with Key Manager Encrypt/Decrypt in fr-par.
type Scaleway struct {
	client     *http.Client
	endpoint   string
	credential string
	active     string
	allowed    map[string]bool
	slots      chan struct{}
}

// NewScaleway uses credential as the X-Auth-Token. retained keys may unwrap only.
func NewScaleway(credential, active string, retained []string) (*Scaleway, error) {
	if credential == "" || !uuidPattern.MatchString(active) {
		return nil, ErrInvalid
	}
	p := &Scaleway{
		credential: credential, active: active, allowed: map[string]bool{}, slots: make(chan struct{}, 8),
		endpoint: "https://api.scaleway.com",
		client: &http.Client{Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	for _, id := range append([]string{active}, retained...) {
		if !uuidPattern.MatchString(id) {
			return nil, ErrInvalid
		}
		p.allowed[id] = true
	}
	return p, nil
}

type kmsRequest struct {
	Plaintext      []byte `json:"plaintext,omitempty"`
	Ciphertext     []byte `json:"ciphertext,omitempty"`
	AssociatedData []byte `json:"associated_data"`
}

type kmsResponse struct {
	KeyID      string `json:"key_id"`
	Plaintext  []byte `json:"plaintext"`
	Ciphertext []byte `json:"ciphertext"`
}

func (p *Scaleway) Wrap(ctx context.Context, key, binding []byte) (Wrapped, error) {
	if len(key) != 32 {
		return Wrapped{}, ErrInvalid
	}
	r, err := p.call(ctx, p.active, "encrypt", kmsRequest{Plaintext: key, AssociatedData: binding})
	if err != nil {
		return Wrapped{}, err
	}
	if len(r.Ciphertext) == 0 || len(r.Ciphertext) > 4096 {
		return Wrapped{}, ErrCiphertext
	}
	return Wrapped{Provider: "scaleway", KeyRef: "fr-par/" + p.active, Format: scalewayFormat, Ciphertext: r.Ciphertext}, nil
}

func (p *Scaleway) Unwrap(ctx context.Context, w Wrapped, binding []byte) ([]byte, error) {
	id, ok := strings.CutPrefix(w.KeyRef, "fr-par/")
	if !ok || !p.allowed[id] || w.Provider != "scaleway" || w.Format != scalewayFormat || len(w.Ciphertext) == 0 || len(w.Ciphertext) > 4096 {
		return nil, ErrCiphertext
	}
	r, err := p.call(ctx, id, "decrypt", kmsRequest{Ciphertext: w.Ciphertext, AssociatedData: binding})
	if err != nil {
		return nil, err
	}
	if len(r.Plaintext) != 32 {
		clear(r.Plaintext)
		return nil, ErrCiphertext
	}
	return r.Plaintext, nil
}

func (p *Scaleway) call(ctx context.Context, id, action string, input kmsRequest) (kmsResponse, error) {
	var zero kmsResponse
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return zero, ErrUnavailable
	}
	defer func() { <-p.slots }()
	body, err := json.Marshal(input)
	if err != nil {
		return zero, ErrInvalid
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/key-manager/v1alpha1/regions/fr-par/keys/"+id+"/"+action, bytes.NewReader(body))
	if err != nil {
		return zero, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", p.credential)
	resp, err := p.client.Do(req)
	if err != nil {
		return zero, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 401, 403:
			return zero, ErrDenied
		case 404:
			return zero, ErrNotFound
		case 400, 422:
			return zero, ErrCiphertext
		default:
			return zero, ErrUnavailable
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	defer clear(data)
	if err != nil || len(data) > 64<<10 {
		return zero, ErrUnavailable
	}
	var result kmsResponse
	if json.Unmarshal(data, &result) != nil || result.KeyID != id {
		clear(result.Plaintext)
		return zero, ErrCiphertext
	}
	return result, nil
}
