package gitwebhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
)

// Wire contract (schema version 1).
//
// A server-side receive hook POSTs one ref update per delivery:
//
//	{"schemaVersion":1,"repository":"<repository id>","ref":"refs/heads/main",
//	 "before":"<object name or zeros>","after":"<object name>"}
//
// and authenticates it with three headers:
//
//	Kodama-Delivery-Id  unique per update, reused verbatim on every retry
//	Kodama-Timestamp    Unix seconds at signing
//	Kodama-Signature    "v1=" + hex(HMAC-SHA256(secret, SigningString(...)))
//
// The payload carries no tenant, project, URL, credential, command or file
// content; it names the repository only so the receiver can detect a hook that
// is attached to another repository.
const (
	SchemaVersion = 1
	// MaxBodyBytes bounds a notification body.
	MaxBodyBytes = 8 << 10
	// MaxClockSkew bounds how far a signed timestamp may differ from now.
	MaxClockSkew = 5 * time.Minute

	HeaderDeliveryID = "Kodama-Delivery-Id"
	HeaderTimestamp  = "Kodama-Timestamp"
	HeaderSignature  = "Kodama-Signature"

	signaturePrefix = "v1="
	signingContext  = "kodama-git-hook-v1"
	// MinSecretBytes is the shortest accepted hook secret.
	MinSecretBytes = 32
)

var deliveryIDRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateDeliveryID checks a delivery identifier's syntax.
func ValidateDeliveryID(id string) error {
	if !deliveryIDRegexp.MatchString(id) {
		return invalidf("delivery id must match %s", deliveryIDRegexp)
	}
	return nil
}

// SigningString is the exact text a hook signs. It binds the trigger, the
// delivery identity, the time and the body digest, so a signature cannot be
// replayed against another trigger, delivery or payload.
func SigningString(triggerID TriggerID, deliveryID string, timestamp int64, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{signingContext, string(triggerID), deliveryID, strconv.FormatInt(timestamp, 10), hex.EncodeToString(sum[:])}, "\n")
}

// Sign computes the Kodama-Signature header value.
func Sign(secret []byte, triggerID TriggerID, deliveryID string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(SigningString(triggerID, deliveryID, timestamp, body)))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature compares in constant time; any malformed value is a mismatch.
func verifySignature(secret []byte, triggerID TriggerID, deliveryID string, timestamp int64, body []byte, got string) bool {
	want := Sign(secret, triggerID, deliveryID, timestamp, body)
	return len(got) == len(want) && hmac.Equal([]byte(got), []byte(want))
}

// BodyDigest identifies a body for delivery deduplication.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Payload is the decoded notification body.
type Payload struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	Ref           string `json:"ref"`
	Before        string `json:"before"`
	After         string `json:"after"`
}

var zeroCommits = map[int]string{40: strings.Repeat("0", 40), 64: strings.Repeat("0", 64)}

// IsDeletion reports whether the update removed the ref.
func (p Payload) IsDeletion() bool { return isZero(p.After) }

func isZero(s string) bool { return s == zeroCommits[len(s)] && s != "" }

// ParsePayload strictly decodes a body: one JSON object, no unknown member, no
// trailing data, schema version 1, a syntactically valid repository, ref and
// object names. It never interprets any field as a command or a path.
func ParsePayload(body []byte) (Payload, error) {
	var p Payload
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return p, invalidf("body must be 1..%d bytes", MaxBodyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Payload{}, invalidf("body is not a version 1 notification")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Payload{}, invalidf("body has trailing data")
	}
	if p.SchemaVersion != SchemaVersion {
		return Payload{}, invalidf("unsupported schemaVersion")
	}
	if err := workspace.RepositoryID(p.Repository).Validate(); err != nil {
		return Payload{}, invalidf("repository is not a repository identifier")
	}
	if len(p.Ref) > 255 || strings.ContainsAny(p.Ref, "\x00\r\n") {
		return Payload{}, invalidf("ref is malformed")
	}
	if strings.HasPrefix(p.Ref, "refs/heads/") && workspace.ValidateBranch(p.Ref) != nil {
		return Payload{}, invalidf("ref is not a safe branch reference")
	}
	if !isZero(p.Before) {
		if err := workspace.Commit(p.Before).Validate(); err != nil {
			return Payload{}, invalidf("before must be a full object name")
		}
	}
	if !isZero(p.After) {
		if err := workspace.Commit(p.After).Validate(); err != nil {
			return Payload{}, invalidf("after must be a full object name")
		}
	}
	return p, nil
}
