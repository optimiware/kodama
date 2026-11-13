package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	maxAuthTransactions    = 4096
	maxSocialTransactions  = 4096
	maxGitHubSetupStates   = 64
	maxAuthCodes           = 4096
	maxRefreshTokens       = 8192
	maxDeviceSessions      = 2048
	maxDynamicClients      = 1024
	maxRevokedJTIs         = 8192
	browserFlowLifetime    = 10 * time.Minute
	unambiguousUserCharset = "BCDFGHJKLMNPQRSTVWXZ"
)

var (
	errInvalidGrant   = errors.New("invalid_grant")
	errExpiredToken   = errors.New("expired_token")
	errAuthPending    = errors.New("authorization_pending")
	errAccessDenied   = errors.New("access_denied")
	errNotFoundOrUsed = errors.New("state not found or expired")
)

type txKind int

const (
	txAuthorize txKind = iota + 1
	txDevice
)

type authTx struct {
	ID            string
	Kind          txKind
	ClientID      string
	RedirectURI   string
	State         string
	Nonce         string
	Scope         string
	CodeChallenge string
	LoginHint     string
	DeviceHash    [32]byte
	UserCode      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type socialTx struct {
	StateHash     [32]byte
	Provider      string
	AuthTxID      string
	BrowserID     string
	CodeVerifier  string
	UpstreamNonce string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type githubSetupTx struct {
	StateHash [32]byte
	AuthTxID  string
	BrowserID string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type authCodeEntry struct {
	CodeHash      [32]byte
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Nonce         string
	Scope         string
	User          UserIdentity
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type refreshEntry struct {
	TokenHash [32]byte
	FamilyID  string
	ClientID  string
	Scope     string
	User      UserIdentity
	CreatedAt time.Time
	ExpiresAt time.Time
	Rotated   bool
	Revoked   bool
}

type deviceStatus int

const (
	devicePending deviceStatus = iota + 1
	deviceApproved
	deviceDenied
)

type deviceEntry struct {
	DeviceHash         [32]byte
	NormalizedUserCode string
	DisplayUserCode    string
	ClientID           string
	Scope              string
	Status             deviceStatus
	User               UserIdentity
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

type registeredClient struct {
	ClientID      string
	ClientName    string
	RedirectURIs  []string
	GrantTypes    []string
	ResponseTypes []string
	IssuedAt      int64
}

type authStore struct {
	mu          sync.Mutex
	authTxs     map[string]*authTx
	socialTxs   map[[32]byte]*socialTx
	ghSetupTxs  map[[32]byte]*githubSetupTx
	authCodes   map[[32]byte]*authCodeEntry
	refresh     map[[32]byte]*refreshEntry
	devices     map[[32]byte]*deviceEntry
	userCodes   map[string][32]byte
	clients     map[string]*registeredClient
	clientOrder []string
	revokedJTIs map[string]time.Time
}

func newAuthStore() *authStore {
	return &authStore{
		authTxs:     make(map[string]*authTx),
		socialTxs:   make(map[[32]byte]*socialTx),
		ghSetupTxs:  make(map[[32]byte]*githubSetupTx),
		authCodes:   make(map[[32]byte]*authCodeEntry),
		refresh:     make(map[[32]byte]*refreshEntry),
		devices:     make(map[[32]byte]*deviceEntry),
		userCodes:   make(map[string][32]byte),
		clients:     make(map[string]*registeredClient),
		revokedJTIs: make(map[string]time.Time),
	}
}

func secretDigest(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func randomToken(random io.Reader, numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomUserCode generates an 8-character XXXX-XXXX code from RFC 8628's
// unambiguous consonant alphabet without modulo bias.
func randomUserCode(random io.Reader) (string, error) {
	const codeLen = 8
	const alphabetLen = len(unambiguousUserCharset) // 20
	const maxUnbiased = byte(240)                   // 12 * 20
	chars := make([]byte, 0, codeLen+1)
	var one [1]byte
	for len(chars) < codeLen+1 {
		if len(chars) == 4 {
			chars = append(chars, '-')
			continue
		}
		if _, err := io.ReadFull(random, one[:]); err != nil {
			return "", err
		}
		if one[0] >= maxUnbiased {
			continue
		}
		chars = append(chars, unambiguousUserCharset[int(one[0])%alphabetLen])
	}
	return string(chars), nil
}

func normalizeUserCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (store *authStore) putAuthTx(tx authTx, now time.Time) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneAuthTxsLocked(now)
	if len(store.authTxs) >= maxAuthTransactions {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range store.authTxs {
			if oldestKey == "" || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
			}
		}
		delete(store.authTxs, oldestKey)
	}
	copied := tx
	store.authTxs[tx.ID] = &copied
}

func (store *authStore) getAuthTx(id string, now time.Time) (authTx, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.authTxs[id]
	if !ok {
		return authTx{}, false
	}
	if !now.Before(entry.ExpiresAt) {
		delete(store.authTxs, id)
		return authTx{}, false
	}
	return *entry, true
}

func (store *authStore) consumeAuthTx(id string, now time.Time) (authTx, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.authTxs[id]
	if !ok {
		return authTx{}, false
	}
	delete(store.authTxs, id)
	if !now.Before(entry.ExpiresAt) {
		return authTx{}, false
	}
	return *entry, true
}

func (store *authStore) pruneAuthTxsLocked(now time.Time) {
	for k, v := range store.authTxs {
		if !now.Before(v.ExpiresAt) {
			delete(store.authTxs, k)
		}
	}
}

func (store *authStore) putSocialTx(rawState string, tx socialTx, now time.Time) {
	hash := secretDigest(rawState)
	tx.StateHash = hash
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, v := range store.socialTxs {
		if !now.Before(v.ExpiresAt) {
			delete(store.socialTxs, k)
		}
	}
	if len(store.socialTxs) >= maxSocialTransactions {
		var oldestKey [32]byte
		var oldestSet bool
		var oldestTime time.Time
		for k, v := range store.socialTxs {
			if !oldestSet || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
				oldestSet = true
			}
		}
		if oldestSet {
			delete(store.socialTxs, oldestKey)
		}
	}
	copied := tx
	store.socialTxs[hash] = &copied
}

func (store *authStore) consumeSocialTx(rawState string, provider string, now time.Time) (socialTx, bool) {
	hash := secretDigest(rawState)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.socialTxs[hash]
	if !ok {
		return socialTx{}, false
	}
	delete(store.socialTxs, hash)
	if !now.Before(entry.ExpiresAt) || entry.Provider != provider {
		return socialTx{}, false
	}
	return *entry, true
}

func (store *authStore) putGitHubSetupTx(rawState, authTxID, browserID string, now time.Time) {
	hash := secretDigest(rawState)
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, v := range store.ghSetupTxs {
		if !now.Before(v.ExpiresAt) {
			delete(store.ghSetupTxs, k)
		}
	}
	if len(store.ghSetupTxs) >= maxGitHubSetupStates {
		var oldestKey [32]byte
		var oldestSet bool
		var oldestTime time.Time
		for k, v := range store.ghSetupTxs {
			if !oldestSet || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
				oldestSet = true
			}
		}
		if oldestSet {
			delete(store.ghSetupTxs, oldestKey)
		}
	}
	store.ghSetupTxs[hash] = &githubSetupTx{
		StateHash: hash,
		AuthTxID:  authTxID,
		BrowserID: browserID,
		CreatedAt: now,
		ExpiresAt: now.Add(browserFlowLifetime),
	}
}

func (store *authStore) consumeGitHubSetupTx(rawState string, now time.Time) (githubSetupTx, bool) {
	hash := secretDigest(rawState)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.ghSetupTxs[hash]
	if !ok {
		return githubSetupTx{}, false
	}
	delete(store.ghSetupTxs, hash)
	if !now.Before(entry.ExpiresAt) {
		return githubSetupTx{}, false
	}
	return *entry, true
}

func (store *authStore) putAuthCode(rawCode string, entry authCodeEntry, now time.Time) {
	hash := secretDigest(rawCode)
	entry.CodeHash = hash
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, v := range store.authCodes {
		if !now.Before(v.ExpiresAt) {
			delete(store.authCodes, k)
		}
	}
	if len(store.authCodes) >= maxAuthCodes {
		var oldestKey [32]byte
		var oldestSet bool
		var oldestTime time.Time
		for k, v := range store.authCodes {
			if !oldestSet || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
				oldestSet = true
			}
		}
		if oldestSet {
			delete(store.authCodes, oldestKey)
		}
	}
	copied := entry
	store.authCodes[hash] = &copied
}

func (store *authStore) consumeAuthCode(rawCode string, now time.Time) (authCodeEntry, error) {
	hash := secretDigest(rawCode)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.authCodes[hash]
	if !ok {
		return authCodeEntry{}, errInvalidGrant
	}
	delete(store.authCodes, hash)
	if !now.Before(entry.ExpiresAt) {
		return authCodeEntry{}, errInvalidGrant
	}
	return *entry, nil
}

func (store *authStore) issueRefreshToken(rawToken, familyID, clientID, scope string, user UserIdentity, ttl time.Duration, now time.Time) {
	hash := secretDigest(rawToken)
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, v := range store.refresh {
		if !now.Before(v.ExpiresAt) {
			delete(store.refresh, k)
		}
	}
	if len(store.refresh) >= maxRefreshTokens {
		var oldestKey [32]byte
		var oldestSet bool
		var oldestTime time.Time
		for k, v := range store.refresh {
			if !oldestSet || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
				oldestSet = true
			}
		}
		if oldestSet {
			delete(store.refresh, oldestKey)
		}
	}
	store.refresh[hash] = &refreshEntry{
		TokenHash: hash,
		FamilyID:  familyID,
		ClientID:  clientID,
		Scope:     scope,
		User:      user,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
}

// rotateRefreshToken atomically consumes rawOldToken, revoking the entire token
// family if a previously rotated token is replayed, and records rawNewToken in
// the same family.
func (store *authStore) rotateRefreshToken(rawOldToken, rawNewToken, clientID string, ttl time.Duration, now time.Time) (refreshEntry, error) {
	oldHash := secretDigest(rawOldToken)
	newHash := secretDigest(rawNewToken)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.refresh[oldHash]
	if !ok {
		return refreshEntry{}, errInvalidGrant
	}
	if !now.Before(entry.ExpiresAt) {
		delete(store.refresh, oldHash)
		return refreshEntry{}, errInvalidGrant
	}
	if !constantTimeEqualStrings(entry.ClientID, clientID) {
		return refreshEntry{}, errInvalidGrant
	}
	if entry.Revoked {
		return refreshEntry{}, errInvalidGrant
	}
	if entry.Rotated {
		// Replay of a rotated refresh token compromises the family; revoke all members.
		family := entry.FamilyID
		for _, member := range store.refresh {
			if member.FamilyID == family {
				member.Revoked = true
			}
		}
		return refreshEntry{}, errInvalidGrant
	}
	entry.Rotated = true
	next := &refreshEntry{
		TokenHash: newHash,
		FamilyID:  entry.FamilyID,
		ClientID:  entry.ClientID,
		Scope:     entry.Scope,
		User:      entry.User,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	store.refresh[newHash] = next
	return *next, nil
}

func (store *authStore) revokeRefreshToken(rawToken, clientID string) bool {
	hash := secretDigest(rawToken)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.refresh[hash]
	if !ok {
		return false
	}
	if !constantTimeEqualStrings(entry.ClientID, clientID) {
		return false
	}
	family := entry.FamilyID
	for _, member := range store.refresh {
		if member.FamilyID == family {
			member.Revoked = true
		}
	}
	return true
}

func (store *authStore) revokeJTI(jti string, expiresAt time.Time, now time.Time) {
	if jti == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, exp := range store.revokedJTIs {
		if !now.Before(exp) {
			delete(store.revokedJTIs, k)
		}
	}
	if len(store.revokedJTIs) >= maxRevokedJTIs {
		for k := range store.revokedJTIs {
			delete(store.revokedJTIs, k)
			break
		}
	}
	store.revokedJTIs[jti] = expiresAt
}

func (store *authStore) isJTIRevoked(jti string, now time.Time) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	exp, ok := store.revokedJTIs[jti]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(store.revokedJTIs, jti)
		return false
	}
	return true
}

func (store *authStore) createDeviceSession(random io.Reader, clientID, scope string, ttl time.Duration, now time.Time) (string, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneDevicesLocked(now)
	if len(store.devices) >= maxDeviceSessions {
		var oldestKey [32]byte
		var oldestSet bool
		var oldestTime time.Time
		for k, v := range store.devices {
			if !oldestSet || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
				oldestSet = true
			}
		}
		if oldestSet {
			if victim := store.devices[oldestKey]; victim != nil {
				delete(store.userCodes, victim.NormalizedUserCode)
			}
			delete(store.devices, oldestKey)
		}
	}

	rawDeviceCode, err := randomToken(random, 32)
	if err != nil {
		return "", "", err
	}
	devHash := secretDigest(rawDeviceCode)

	var displayCode, normCode string
	for attempts := 0; attempts < 16; attempts++ {
		candidate, err := randomUserCode(random)
		if err != nil {
			return "", "", err
		}
		norm := normalizeUserCode(candidate)
		if _, exists := store.userCodes[norm]; !exists {
			displayCode = candidate
			normCode = norm
			break
		}
	}
	if normCode == "" {
		return "", "", errors.New("could not allocate unique device user code")
	}

	store.devices[devHash] = &deviceEntry{
		DeviceHash:         devHash,
		NormalizedUserCode: normCode,
		DisplayUserCode:    displayCode,
		ClientID:           clientID,
		Scope:              scope,
		Status:             devicePending,
		CreatedAt:          now,
		ExpiresAt:          now.Add(ttl),
	}
	store.userCodes[normCode] = devHash
	return rawDeviceCode, displayCode, nil
}

func (store *authStore) lookupPendingDeviceByUserCode(userCode string, now time.Time) (deviceEntry, bool) {
	norm := normalizeUserCode(userCode)
	if norm == "" {
		return deviceEntry{}, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	devHash, ok := store.userCodes[norm]
	if !ok {
		return deviceEntry{}, false
	}
	entry, ok := store.devices[devHash]
	if !ok || !now.Before(entry.ExpiresAt) {
		delete(store.userCodes, norm)
		delete(store.devices, devHash)
		return deviceEntry{}, false
	}
	if entry.Status != devicePending {
		return deviceEntry{}, false
	}
	return *entry, true
}

func (store *authStore) resolveDeviceSession(devHash [32]byte, status deviceStatus, user UserIdentity, now time.Time) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.devices[devHash]
	if !ok || !now.Before(entry.ExpiresAt) {
		if ok {
			delete(store.userCodes, entry.NormalizedUserCode)
			delete(store.devices, devHash)
		}
		return false
	}
	if entry.Status != devicePending {
		return false
	}
	entry.Status = status
	entry.User = user
	delete(store.userCodes, entry.NormalizedUserCode)
	return true
}

func (store *authStore) pollDeviceSession(rawDeviceCode, clientID string, now time.Time) (deviceEntry, error) {
	devHash := secretDigest(rawDeviceCode)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.devices[devHash]
	if !ok {
		return deviceEntry{}, errInvalidGrant
	}
	if !constantTimeEqualStrings(entry.ClientID, clientID) {
		return deviceEntry{}, errInvalidGrant
	}
	if !now.Before(entry.ExpiresAt) {
		delete(store.userCodes, entry.NormalizedUserCode)
		delete(store.devices, devHash)
		return deviceEntry{}, errExpiredToken
	}
	switch entry.Status {
	case devicePending:
		return deviceEntry{}, errAuthPending
	case deviceDenied:
		delete(store.userCodes, entry.NormalizedUserCode)
		delete(store.devices, devHash)
		return deviceEntry{}, errAccessDenied
	case deviceApproved:
		copied := *entry
		delete(store.userCodes, entry.NormalizedUserCode)
		delete(store.devices, devHash)
		return copied, nil
	default:
		return deviceEntry{}, errInvalidGrant
	}
}

func (store *authStore) pruneDevicesLocked(now time.Time) {
	for k, v := range store.devices {
		if !now.Before(v.ExpiresAt) {
			delete(store.userCodes, v.NormalizedUserCode)
			delete(store.devices, k)
		}
	}
}

func (store *authStore) registerClient(client registeredClient) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.clients) >= maxDynamicClients && len(store.clientOrder) > 0 {
		oldest := store.clientOrder[0]
		store.clientOrder = store.clientOrder[1:]
		delete(store.clients, oldest)
	}
	copied := client
	copied.RedirectURIs = append([]string(nil), client.RedirectURIs...)
	copied.GrantTypes = append([]string(nil), client.GrantTypes...)
	copied.ResponseTypes = append([]string(nil), client.ResponseTypes...)
	store.clients[client.ClientID] = &copied
	store.clientOrder = append(store.clientOrder, client.ClientID)
}

func (store *authStore) lookupRegisteredClient(clientID string) (registeredClient, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.clients[clientID]
	if !ok {
		return registeredClient{}, false
	}
	copied := *entry
	copied.RedirectURIs = append([]string(nil), entry.RedirectURIs...)
	copied.GrantTypes = append([]string(nil), entry.GrantTypes...)
	copied.ResponseTypes = append([]string(nil), entry.ResponseTypes...)
	return copied, true
}
