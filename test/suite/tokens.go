package suite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tokenScript signs one OAuthSonas persona in and prints its access token.
const tokenScript = "scripts/oauthsonas-token"

// TokenMinter acquires one fresh access token for a persona.
type TokenMinter func(ctx context.Context, persona string) (string, error)

// WriteTokenFile replaces path with token through a private temporary file and
// a rename in the same directory. A concurrent reader sees the previous token
// or the new one, never a partial or world-readable file.
func WriteTokenFile(path, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to write an empty token")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return fmt.Errorf("create the temporary token file: %w", err)
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("restrict the temporary token file: %w", err)
	}
	if _, err := temporary.WriteString(token); err != nil {
		temporary.Close()
		return fmt.Errorf("write the temporary token file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write the temporary token file: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("publish the token file: %w", err)
	}
	return nil
}

// TokenRefresher keeps the private token files of a long run current. The
// installed OAuthSonas tokens live about 15 minutes; every test reads its
// token file when it starts, so a file refreshed well inside that lifetime
// gives each test a token that outlives it.
type TokenRefresher struct {
	// Files maps a persona to the private token file it owns.
	Files map[string]string
	Mint  TokenMinter
}

// Refresh mints and publishes every persona's token. It attempts every persona
// and returns the failures together; a failed persona keeps its previous file.
func (refresher TokenRefresher) Refresh(ctx context.Context) error {
	personas := make([]string, 0, len(refresher.Files))
	for persona := range refresher.Files {
		personas = append(personas, persona)
	}
	sort.Strings(personas)
	var failures []error
	for _, persona := range personas {
		token, err := refresher.Mint(ctx, persona)
		if err == nil {
			err = WriteTokenFile(refresher.Files[persona], token)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("persona %s: %w", persona, err))
		}
	}
	return errors.Join(failures...)
}

// Run refreshes every interval until ctx ends and reports each failed round. A
// failed round is retried at the next interval: the tests still hold the
// previous tokens, and they name an expired credential themselves.
func (refresher TokenRefresher) Run(ctx context.Context, interval time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := refresher.Refresh(ctx); err != nil && ctx.Err() == nil {
				report(err)
			}
		}
	}
}

// StageTokens owns the persona token files of one stage run.
type StageTokens struct {
	// SecondAdminFile is the second tenant administrator's token file, or
	// empty when the run names no second-tenant persona.
	SecondAdminFile string

	refresher   TokenRefresher
	refreshable bool
	directory   string
}

// stagePersonaFiles are the persona token files the stage runner hands to the
// e2e proofs.
var stagePersonaFiles = map[string]string{
	"admin":    "Q01_ADMIN_TOKEN_FILE",
	"engineer": "Q01_ENGINEER_TOKEN_FILE",
	"reviewer": "Q01_REVIEWER_TOKEN_FILE",
	"operator": "Q01_OPERATOR_TOKEN_FILE",
}

// OpenStageTokens prepares the token files of a stage run. The four persona
// files come from the environment (the stage runner acquired them). When
// Q02_SECOND_ADMIN_PERSONA names the second tenant's administrator and no
// token value was exported for it, its token is acquired now into a private
// file this run owns.
func OpenStageTokens(ctx context.Context, installation string, getenv func(string) string) (*StageTokens, error) {
	document, err := readDescriptor(installation)
	if err != nil {
		return nil, err
	}
	tokens := &StageTokens{
		refresher:   TokenRefresher{Files: map[string]string{}, Mint: document.tokenMinter()},
		refreshable: document.canMintTokens(),
	}
	for persona, variable := range stagePersonaFiles {
		if path := strings.TrimSpace(getenv(variable)); path != "" {
			tokens.refresher.Files[persona] = path
		}
	}
	second := strings.TrimSpace(getenv("Q02_SECOND_ADMIN_PERSONA"))
	if second == "" || getenv("Q02_SECOND_ADMIN_TOKEN") != "" || strings.TrimSpace(getenv("Q02_SECOND_ADMIN_TOKEN_FILE")) != "" {
		return tokens, nil
	}
	if _, taken := tokens.refresher.Files[second]; taken {
		return nil, fmt.Errorf("Q02_SECOND_ADMIN_PERSONA=%s is one of the first tenant's personas", second)
	}
	if tokens.directory, err = os.MkdirTemp("", "kodama-suite-tokens-"); err != nil {
		return nil, fmt.Errorf("create the private token directory: %w", err)
	}
	tokens.SecondAdminFile = filepath.Join(tokens.directory, "second-admin.token")
	token, err := tokens.refresher.Mint(ctx, second)
	if err == nil {
		err = WriteTokenFile(tokens.SecondAdminFile, token)
	}
	if err != nil {
		tokens.Close()
		return nil, fmt.Errorf("acquire the second tenant administrator token (persona %s): %w", second, err)
	}
	tokens.refresher.Files[second] = tokens.SecondAdminFile
	return tokens, nil
}

// Refresher returns the refresher over every token file of the run and
// whether the descriptor carries the OAuthSonas settings it needs.
func (tokens *StageTokens) Refresher() (TokenRefresher, bool) {
	return tokens.refresher, tokens.refreshable && len(tokens.refresher.Files) > 0
}

// Close removes the token files this run created.
func (tokens *StageTokens) Close() {
	if tokens.directory != "" {
		_ = os.RemoveAll(tokens.directory)
	}
}

// tokenMinter signs a persona in with the descriptor's OAuthSonas settings.
// The token script reads the picker access code from its private file; neither
// the code nor a token reaches an argument, the environment or an error.
func (document descriptor) tokenMinter() TokenMinter {
	oauth := document.OAuth
	return func(ctx context.Context, persona string) (string, error) {
		if !document.canMintTokens() {
			return "", errors.New("the installation descriptor names no oauthsonas issuer, ca_file and access_code_file")
		}
		command := exec.CommandContext(ctx, tokenScript, persona)
		command.Env = append(os.Environ(),
			"OAUTHSONAS_URL="+oauth.Issuer,
			"OAUTHSONAS_CA="+oauth.CAFile,
			"OAUTHSONAS_ACCESS_CODE_FILE="+oauth.AccessCodeFile,
		)
		for name, value := range map[string]string{
			"OAUTHSONAS_CLIENT_ID": oauth.ClientID,
			"OAUTHSONAS_REDIRECT":  oauth.RedirectURI,
			"OAUTHSONAS_RESOLVE":   oauth.Resolve,
		} {
			if value != "" {
				command.Env = append(command.Env, name+"="+value)
			}
		}
		var output bytes.Buffer
		command.Stdout = &output
		if err := command.Run(); err != nil {
			return "", fmt.Errorf("%s failed: %w", tokenScript, err)
		}
		return output.String(), nil
	}
}

// canMintTokens reports whether the descriptor carries the settings a token
// acquisition needs.
func (document descriptor) canMintTokens() bool {
	return document.OAuth.Issuer != "" && document.OAuth.CAFile != "" && document.OAuth.AccessCodeFile != ""
}
