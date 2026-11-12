package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrCheckpointMissing reports an absent checkpoint reference. Storage
// implementations wrap it so the store can map the absence to a typed
// restoration error.
var ErrCheckpointMissing = errors.New("checkpoint missing")

// Storage is the durable checkpoint backend boundary. It is an external,
// Userland-controlled store; the session package opens nothing itself.
type Storage interface {
	Read(ctx context.Context, ref Ref) (Record, error)
	CompareAndSwap(ctx context.Context, record Record, expectedRevision uint64) (Ref, error)
}

// Clock is the injected time boundary.
type Clock interface {
	Now() time.Time
}

// StoreConfig carries the durable boundaries and the sensitive values the store
// must filter before persistence.
type StoreConfig struct {
	Storage Storage
	Clock   Clock
	Secrets []string
}

// NewStore builds a durable store over explicit boundaries. It opens no
// storage, starts no process and reads no clock of its own.
func NewStore(config StoreConfig) (Store, error) {
	if config.Storage == nil {
		return nil, fmt.Errorf("session store requires storage")
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("session store requires a clock")
	}
	secrets := make([]string, 0, len(config.Secrets))
	for _, secret := range config.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	return &durableStore{storage: config.Storage, clock: config.Clock, secrets: secrets}, nil
}

// durableStore is the checkpoint Store over an external storage boundary.
type durableStore struct {
	storage Storage
	clock   Clock
	secrets []string
}

// credentialLabelSource is the credential-shaped label shared by the textual
// assignment pattern and the structural argument-key check. The label is
// matched as the suffix of an identifier because '_' is a word character:
// GITHUB_TOKEN, AWS_SECRET_ACCESS_KEY and db_password all label a credential
// yet carry no word boundary of their own.
const credentialLabelSource = `[A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key(?:[_-]?id)?|private[_-]?key)(?:[_-](?:access[_-]?key|key|id|token|secret|value))?`

// credentialPatterns are the credential shapes redacted before persistence.
// Every pattern is case-insensitive so a differently cased header or label
// cannot reach the storage boundary.
var (
	// bearerCredentialPattern matches a bearer credential value, keeping the
	// scheme word so redaction renders a readable placeholder.
	bearerCredentialPattern = regexp.MustCompile(`(?i)\b(bearer)[ \t]+[^\s,;]+`)
	// pemPrivateKeyPattern matches a whole PEM private key block, or an
	// unterminated header through the end of the value.
	pemPrivateKeyPattern = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	// credentialAssignmentPattern matches a labelled credential assignment,
	// keeping the label and separator so redaction stays readable. The label is
	// matched as the suffix of an identifier because '_' is a word character:
	// GITHUB_TOKEN, AWS_SECRET_ACCESS_KEY and db_password all label a
	// credential yet carry no word boundary of their own. The separator allows
	// the closing quote of a JSON/YAML key, and the value covers bare tokens,
	// quoted values and an unterminated quote through the end of its line. It
	// mirrors the knowledge refusal gate in internal/userland/skill/secrets.go.
	credentialAssignmentPattern = regexp.MustCompile(`(?im)(^|[^A-Za-z0-9])(` + credentialLabelSource + `)(["']?[ \t]*[=:][ \t]*)((?:'[^'\r\n]*[^\s'\r\n][^'\r\n]*'|"[^"\r\n]*[^\s"\r\n][^"\r\n]*"|"[^"\r\n]*$|'[^'\r\n]*$|[^\s"',;][^\s,;]*))`)
	// credentialKeyPattern matches an object key that ends in a credential
	// label. The label may be the whole key, an identifier suffix (db_password,
	// apiKey) or the tail after any separator a key can carry (db.password,
	// auth.token, x/secret, aws:secret, "my password", "é_token"). Only ASCII
	// letters and digits count as word characters, matching the textual
	// assignment pass, so a non-ASCII letter before the label still separates
	// it. Keys that merely start with a label (max_tokens, token_count,
	// tokens_used) or embed one without a separating boundary (author,
	// secretary) do not match.
	credentialKeyPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:` + credentialLabelSource + `)$`)
	// awsAccessKeyPattern matches an AWS-style access key id.
	awsAccessKeyPattern = regexp.MustCompile(`(?i)\bAKIA[0-9A-Z]{16}\b`)
)

// Save validates and redacts one record, then commits it with compare-and-swap.
// It returns a reference only after the backend acknowledges the commit and
// that reference parses as a checkpoint reference.
func (store *durableStore) Save(ctx context.Context, record Record) (Ref, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	if record.SessionID == "" {
		return "", fmt.Errorf("checkpoint record has no session id")
	}
	if record.Revision == math.MaxUint64 {
		return "", fmt.Errorf("checkpoint revision %d cannot be advanced", record.Revision)
	}

	persisted := record
	persisted.Messages = copyMessages(record.Messages)
	persisted.ContextRefs = append([]Ref(nil), record.ContextRefs...)
	persisted.Usage = append([]Usage(nil), record.Usage...)

	// Tool-call correlation is raw transcript content: without raw-content
	// retention the call list and the tool-call id are withheld with the
	// content; with retention the call arguments are redacted like content.
	if persisted.Retention.RawContent {
		for index := range persisted.Messages {
			persisted.Messages[index].Content = store.redact(persisted.Messages[index].Content)
			for callIndex := range persisted.Messages[index].ToolCalls {
				call := &persisted.Messages[index].ToolCalls[callIndex]
				call.Arguments = store.redactArguments(call.Arguments)
				// A record that passed Validate before redaction must never
				// become unrestorable. Structural redaction already
				// guarantees a bounded JSON object; re-running the call
				// validation keeps that a checked invariant, substituting the
				// safe empty object rather than persisting a corrupt record.
				if err := call.Validate(); err != nil {
					call.Arguments = "{}"
				}
			}
		}
		persisted.Outcome.Summary = store.redact(persisted.Outcome.Summary)
	} else if persisted.Retention.PlaceholderTranscript {
		persisted.Messages = store.placeholderMessages(persisted.Messages)
		persisted.Outcome.Summary = ""
	} else {
		for index := range persisted.Messages {
			persisted.Messages[index].Content = ""
			persisted.Messages[index].ToolCalls = nil
			persisted.Messages[index].ToolCallID = ""
		}
		persisted.Outcome.Summary = ""
	}

	ref, err := store.storage.CompareAndSwap(ctx, persisted, record.Revision)
	if err != nil {
		return "", err
	}
	if _, err := ParseRef(string(ref)); err != nil {
		return "", err
	}
	return ref, nil
}

// Restore reads and validates one checkpoint, resolves every durable context
// reference and withholds raw content that retention no longer permits.
func (store *durableStore) Restore(ctx context.Context, ref Ref) (Record, error) {
	if _, err := ParseRef(string(ref)); err != nil {
		if version, ok := refVersionTag(string(ref)); ok && version != RefVersion {
			return Record{}, RestoreError{Reason: ReasonVersionMismatch, Ref: ref}
		}
		return Record{}, err
	}

	record, err := store.storage.Read(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrCheckpointMissing) {
			return Record{}, RestoreError{Reason: ReasonMissing, Ref: ref}
		}
		return Record{}, err
	}
	if record.Version != RefVersion {
		return Record{}, RestoreError{Reason: ReasonVersionMismatch, Ref: ref}
	}
	if err := record.Validate(); err != nil {
		return Record{}, RestoreError{Reason: ReasonCorrupt, Ref: ref}
	}
	for _, contextRef := range record.ContextRefs {
		if _, err := store.storage.Read(ctx, contextRef); err != nil {
			return Record{}, RestoreError{Reason: ReasonContextMissing, Ref: ref}
		}
	}

	// Copy before withholding so retention never mutates the storage's record.
	record.Messages = copyMessages(record.Messages)

	// A restored checkpoint has no evidence of an external active reference,
	// so raw content follows its recorded retention window only. Correlation
	// is raw transcript content, so it is withheld with the content.
	if !record.Retention.RetainRaw(store.clock.Now(), record.CreatedAt, false) {
		if !record.Retention.RawContent && record.Retention.PlaceholderTranscript {
			record.Messages = store.placeholderMessages(record.Messages)
		} else {
			for index := range record.Messages {
				record.Messages[index].Content = ""
				record.Messages[index].ToolCalls = nil
				record.Messages[index].ToolCallID = ""
			}
		}
		record.Outcome.Summary = ""
	}
	return record, nil
}

// placeholderMessages preserves redacted user/system prompts and a closed
// synthetic placeholder transcript when raw content retention is disabled.
func (store *durableStore) placeholderMessages(messages []Message) []Message {
	copied := copyMessages(messages)
	correlation := 0
	var durableIDs map[string]ToolCallID
	for index := range copied {
		switch copied[index].Role {
		case "user", "system":
			durableIDs = nil
			copied[index].Content = store.redact(copied[index].Content)
			copied[index].ToolCalls = nil
			copied[index].ToolCallID = ""
		case "assistant":
			durableIDs = nil
			copied[index].ToolCallID = ""
			if len(copied[index].ToolCalls) == 0 {
				copied[index].Content = "[assistant output not retained]"
				continue
			}
			copied[index].Content = ""
			durableIDs = make(map[string]ToolCallID, len(copied[index].ToolCalls))
			calls := make([]ToolCall, len(copied[index].ToolCalls))
			for callIndex, call := range copied[index].ToolCalls {
				correlation++
				id := ToolCallID(fmt.Sprintf("call-durable-%d", correlation))
				calls[callIndex] = ToolCall{ID: string(id), Name: call.Name, Arguments: "{}"}
				if call.ID != "" {
					durableIDs[call.ID] = id
				}
			}
			copied[index].ToolCalls = calls
		case "tool":
			copied[index].ToolCalls = nil
			copied[index].Content = "[tool output not retained]"
			oldID := string(copied[index].ToolCallID)
			if id, exists := durableIDs[oldID]; exists {
				copied[index].ToolCallID = id
				delete(durableIDs, oldID)
			} else {
				copied[index].ToolCallID = ""
			}
		default:
			durableIDs = nil
			copied[index].Content = ""
			copied[index].ToolCalls = nil
			copied[index].ToolCallID = ""
		}
	}
	return copied
}

// copyMessages returns a deep copy of the message list, including each
// assistant message's tool-call slice, so a caller cannot mutate a record after
// it crosses the storage boundary.
func copyMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	copied := make([]Message, len(messages))
	for index := range messages {
		copied[index] = messages[index]
		if messages[index].ToolCalls != nil {
			copied[index].ToolCalls = append([]ToolCall(nil), messages[index].ToolCalls...)
		}
	}
	return copied
}

// refVersionTag reports the version tag of a checkpoint-shaped reference even
// when the reference does not parse, so an unsupported family version is
// reported as a typed version mismatch rather than a generic parse error.
func refVersionTag(value string) (string, bool) {
	if !strings.HasPrefix(value, RefPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(value, RefPrefix)
	if rest == "" {
		return "", false
	}
	if separator := strings.IndexByte(rest, '/'); separator >= 0 {
		rest = rest[:separator]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// redact replaces every credential-shaped value and every supplied secret.
func (store *durableStore) redact(value string) string {
	redacted := pemPrivateKeyPattern.ReplaceAllString(value, "[redacted]")
	redacted = redactCredentialAssignments(redacted)
	redacted = awsAccessKeyPattern.ReplaceAllString(redacted, "[redacted]")
	redacted = bearerCredentialPattern.ReplaceAllString(redacted, "${1} [redacted]")
	for _, secret := range store.secrets {
		redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
	}
	return redacted
}

// redactCredentialAssignments replaces every labelled credential value while
// keeping the label, the separator and any wrapping quotes, so a redacted
// JSON/YAML document stays readable and structurally intact.
func redactCredentialAssignments(value string) string {
	return credentialAssignmentPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := credentialAssignmentPattern.FindStringSubmatch(match)
		if parts == nil {
			return match
		}
		replacement := "[redacted]"
		if assigned := parts[4]; len(assigned) >= 2 &&
			(assigned[0] == '"' || assigned[0] == '\'') &&
			assigned[len(assigned)-1] == assigned[0] {
			quote := string(assigned[0])
			replacement = quote + "[redacted]" + quote
		}
		return parts[1] + parts[2] + parts[3] + replacement
	})
}

// redactArguments redacts one tool-call arguments JSON-object string
// structurally: it decodes the object, withholds credential-shaped values and
// re-encodes it deterministically. Numbers keep their spelling because the
// decoder uses json.Number, and the result is always a bounded JSON object, so
// the call validation that passed before redaction still passes after it. The
// encoder deliberately leaves '<', '>' and '&' unescaped: HTML escaping would
// grow a valid in-bound object past the arguments bound and discard it. A
// value that is not a bounded object, a decode or re-encode failure, or a
// redacted form that genuinely exceeds the bound falls back to the safe empty
// object.
func (store *durableStore) redactArguments(arguments string) string {
	if !boundedJSONObject([]byte(arguments), MaxToolArgumentsBytes) {
		return "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "{}"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "{}"
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return "{}"
	}
	redacted, ok := store.redactJSONValue(object)
	if !ok {
		return "{}"
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(redacted); err != nil {
		return "{}"
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if !boundedJSONObject(encoded, MaxToolArgumentsBytes) {
		return "{}"
	}
	return string(encoded)
}

// redactJSONValue redacts one decoded JSON value. A credential-shaped object
// key withholds its whole value whatever the value's JSON type, so no part of
// that subtree can reach storage in clear. Object keys are otherwise kept byte
// for byte: they name the provider's arguments, so rewriting a key could change
// the call's meaning. The one exception is a key whose own text carries a
// credential assignment or a supplied secret; that key is replaced with a
// deterministic, collision-free placeholder and its value is withheld.
func (store *durableStore) redactJSONValue(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return store.redact(typed), true
	case map[string]any:
		return store.redactJSONObject(typed)
	case []any:
		for index, member := range typed {
			redacted, ok := store.redactJSONValue(member)
			if !ok {
				return nil, false
			}
			typed[index] = redacted
		}
		return typed, true
	case nil, bool, json.Number:
		return value, true
	default:
		return nil, false
	}
}

// redactJSONObject redacts one decoded JSON object. Keys are visited in sorted
// order so a renamed key's placeholder index is deterministic, and a
// placeholder skips every key the object already carries plus every
// placeholder this pass generated, so renaming can never collide with or
// overwrite a sibling.
func (store *durableStore) redactJSONObject(object map[string]any) (any, bool) {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	used := make(map[string]struct{}, len(object)*2)
	for _, key := range keys {
		used[key] = struct{}{}
	}
	redacted := make(map[string]any, len(object))
	for _, key := range keys {
		switch {
		case store.redact(key) != key:
			redacted[redactedKeyPlaceholder(used)] = "[redacted]"
		case credentialKeyPattern.MatchString(key):
			redacted[key] = "[redacted]"
		default:
			value, ok := store.redactJSONValue(object[key])
			if !ok {
				return nil, false
			}
			redacted[key] = value
		}
	}
	return redacted, true
}

// redactedKeyPlaceholder returns the first free "[redacted-key-N]" name from
// ordinal 1 upwards and reserves it, so no later renamed key can take the same
// name and overwrite an earlier value.
func redactedKeyPlaceholder(used map[string]struct{}) string {
	for index := 1; ; index++ {
		candidate := fmt.Sprintf("[redacted-key-%d]", index)
		if _, exists := used[candidate]; exists {
			continue
		}
		used[candidate] = struct{}{}
		return candidate
	}
}
