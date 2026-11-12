package services

// Email-to-task intake.
//
// One externally identified RFC822 message creates and starts exactly one
// diagnostic task through the public `task.create_and_start` operation. The
// trusted configuration selects the tenant, project and pipeline; nothing in
// the message does. The bounded Message-ID is the only message identity. Its
// canonical value is the trimmed header token with exactly one surrounding
// `<...>` pair removed; that value is used consistently by the receipt key, the
// submitted metadata, the returned result and the fallback title. The stable
// receipt key is derived from the trusted scope plus that identity, never from
// the content, so a redelivery converges while changed content under the same
// identity is refused by the served receipt.
//
// Line endings are normalized only after the raw bytes are bounded: CRLF in the
// accepted body becomes LF, and that same normalized body supplies both the
// submitted description and the content fingerprint. An optional single leading
// mbox `From ` envelope line is ignored, never treated as message content.
//
// The intake reads no factory database: it crosses only the public operation
// boundary with the public operation codecs.
//
// The message must be one UTF-8 text message. A Message-ID, subject or body
// containing invalid UTF-8 is refused as malformed, which deliberately includes
// a body with a non-UTF-8 charset (for example iso-8859-1 8-bit bytes). This is
// fail-closed behaviour: no byte is reinterpreted or replaced, so the
// JSON-encoded public request never inflates beyond the bytes that were bounded
// here.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// Email intake bounds. The whole raw message, the header field count, the
// Message-ID value and the body are bounded inclusively; attachments are
// refused, never stored.
const (
	// MaxEmailMessageBytes bounds the whole raw message.
	MaxEmailMessageBytes = 32 << 10
	// MaxEmailBodyBytes bounds the body after the header block.
	MaxEmailBodyBytes = 8 << 10
	// MaxEmailHeaderCount bounds the total number of header fields.
	MaxEmailHeaderCount = 64
	// MaxEmailMessageIDBytes bounds the Message-ID value.
	MaxEmailMessageIDBytes = 256
	// MaxEmailSubjectBytes bounds the derived task title.
	MaxEmailSubjectBytes = 200
)

// Email intake refusals.
var (
	// ErrEmailMalformed reports a message that is not one bounded RFC822 text
	// message with exactly one Message-ID.
	ErrEmailMalformed = errors.New("email intake: malformed message")
	// ErrEmailOversize reports a message or one of its parts beyond a bound.
	ErrEmailOversize = errors.New("email intake: message exceeds a bound")
	// ErrEmailAttachment reports a message that carries an attachment or is
	// multipart. Attachments are refused, never stored.
	ErrEmailAttachment = errors.New("email intake: attachments are refused")
	// ErrEmailContentChanged reports a redelivery of one Message-ID whose
	// content differs from the originally committed message.
	ErrEmailContentChanged = errors.New("email intake: message identity reused with changed content")
)

// emailCreateAndStartOperation is the one served public operation the intake
// crosses.
const emailCreateAndStartOperation = "task.create_and_start"

// EmailIntakeConfig is the complete, trusted intake configuration. Every field
// is required and none of it comes from the message.
type EmailIntakeConfig struct {
	TenantID   id.TenantID
	ProjectID  id.ProjectID
	PipelineID string
	Handler    api.Handler
}

// validate checks the trusted configuration without crossing any boundary.
func (config EmailIntakeConfig) validate() error {
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return fmt.Errorf("email intake tenant: %w", err)
	}
	if _, err := id.ParseProjectID(string(config.ProjectID)); err != nil {
		return fmt.Errorf("email intake project: %w", err)
	}
	if strings.TrimSpace(config.PipelineID) == "" {
		return fmt.Errorf("email intake pipeline is missing")
	}
	if config.Handler == nil {
		return fmt.Errorf("email intake handler is missing")
	}
	return nil
}

// EmailIntakeResult is the identity of the one created task and execution.
type EmailIntakeResult struct {
	MessageID        string
	TaskID           string
	ExecutionID      string
	LifecycleVersion uint64
}

// errEmailSourceRead reports a failed read from the message source. It never
// carries source-provided text, so a reader cannot echo its input into an error
// the caller prints.
var errEmailSourceRead = errors.New("email intake: message source read failed")

// readEmailSource reads at most MaxEmailMessageBytes+1 bytes and honours the
// context while doing so. The read runs in its own goroutine so a source that
// blocks (for example stdin with no writer) cannot swallow a cancellation; the
// result channel is buffered so the goroutine can always finish and exit
// without touching shared state after this function returns.
func readEmailSource(ctx context.Context, source io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type sourceResult struct {
		raw []byte
		err error
	}
	results := make(chan sourceResult, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(source, MaxEmailMessageBytes+1))
		results <- sourceResult{raw: raw, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-results:
		if result.err != nil {
			return nil, errEmailSourceRead
		}
		return result.raw, nil
	}
}

// parsedEmail is one validated bounded message.
type parsedEmail struct {
	messageID     string
	subject       string
	description   string
	contentDigest string
}

// IntakeEmail reads one bounded message from source and creates and starts one
// task with the trusted scope and pipeline. A message that is malformed,
// oversize or attached is refused before any public call.
func IntakeEmail(ctx context.Context, config EmailIntakeConfig, source io.Reader) (EmailIntakeResult, error) {
	if err := config.validate(); err != nil {
		return EmailIntakeResult{}, err
	}
	if source == nil {
		return EmailIntakeResult{}, fmt.Errorf("email intake message source is missing")
	}
	raw, err := readEmailSource(ctx, source)
	if err != nil {
		return EmailIntakeResult{}, err
	}
	if len(raw) > MaxEmailMessageBytes {
		return EmailIntakeResult{}, fmt.Errorf("%w: raw message is %d bytes; at most %d are allowed",
			ErrEmailOversize, len(raw), MaxEmailMessageBytes)
	}
	parsed, err := parseEmailMessage(raw)
	if err != nil {
		return EmailIntakeResult{}, err
	}
	// The read and parse above are local; honour a cancellation that arrived
	// while the message was being consumed before crossing the public boundary.
	if err := ctx.Err(); err != nil {
		return EmailIntakeResult{}, err
	}

	request := api.TaskCreateAndStartRequest{
		ProjectID:   string(config.ProjectID),
		Title:       parsed.title(),
		Description: parsed.description,
		PipelineID:  config.PipelineID,
		Metadata: map[string]string{
			"email.message_id":     parsed.messageID,
			"email.content_sha256": parsed.contentDigest,
		},
	}
	decoded, err := invokePublicOperation(ctx, config.Handler, emailCreateAndStartOperation, request,
		emailIdempotencyKey(config, parsed.messageID))
	if err != nil {
		if isEmailChangedContent(err) {
			return EmailIntakeResult{}, ErrEmailContentChanged
		}
		return EmailIntakeResult{}, err
	}
	committed, ok := decoded.(api.TaskCreateAndStartResult)
	if !ok {
		return EmailIntakeResult{}, fmt.Errorf("%s returned an unexpected %T", emailCreateAndStartOperation, decoded)
	}
	return EmailIntakeResult{
		MessageID:        parsed.messageID,
		TaskID:           committed.TaskID,
		ExecutionID:      committed.ExecutionID,
		LifecycleVersion: committed.LifecycleVersion,
	}, nil
}

// title derives the bounded task title from the subject, with a stable fallback
// for a subject-less message. It never carries message-selected authority.
func (parsed parsedEmail) title() string {
	title := boundEmailTitle(parsed.subject)
	if title != "" {
		return title
	}
	return boundEmailTitle("Email " + parsed.messageID)
}

// boundEmailTitle truncates one title to MaxEmailSubjectBytes without splitting
// a UTF-8 sequence.
func boundEmailTitle(value string) string {
	if len(value) <= MaxEmailSubjectBytes {
		return value
	}
	cut := value[:MaxEmailSubjectBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}

// emailIdempotencyKey derives the stable receipt key from the trusted scope and
// the bounded Message-ID only. The pipeline is deliberately excluded and the
// content never contributes, so a configuration change conflicts with the
// committed receipt instead of duplicating the task.
func emailIdempotencyKey(config EmailIntakeConfig, messageID string) string {
	scope := strings.Join([]string{
		"email-intake", "v1", string(config.TenantID), string(config.ProjectID), messageID,
	}, "\x00")
	sum := sha256.Sum256([]byte(scope))
	return "email-intake:v1:" + hex.EncodeToString(sum[:])
}

// isEmailChangedContent reports the public changed-idempotency-content refusal.
func isEmailChangedContent(err error) bool {
	var pointer *api.Error
	if errors.As(err, &pointer) {
		return pointer.Code == api.CodeChangedIdempotencyContent
	}
	var value api.Error
	return errors.As(err, &value) && value.Code == api.CodeChangedIdempotencyContent
}

// invokePublicOperation crosses the served public boundary with the public
// operation codec: the request is strictly encoded (and validated), the result
// operation identity is checked and the result is strictly decoded. It is the
// package-level counterpart of Adapter.invoke over the same codec table.
func invokePublicOperation(ctx context.Context, handler api.Handler, operation string, request any, idempotencyKey string) (any, error) {
	codec, ok := publicCodecs[operation]
	if !ok {
		return nil, fmt.Errorf("operation %q has no public codec", operation)
	}
	input, err := codec.EncodeRequest(request)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", operation, err)
	}
	result, err := handler.Invoke(ctx, api.Invocation{
		Operation: operation,
		Request: api.Request{
			Input:          input,
			IdempotencyKey: idempotencyKey,
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Operation != operation {
		return nil, fmt.Errorf("operation %q returned a result for %q", operation, result.Operation)
	}
	decoded, err := codec.DecodeResult(result.Body)
	if err != nil {
		return nil, fmt.Errorf("decode %s result: %w", operation, err)
	}
	return decoded, nil
}

// parseEmailMessage enforces every bound and reads only the fields the intake
// needs. Headers can select nothing but the subject and the content digest.
func parseEmailMessage(raw []byte) (parsedEmail, error) {
	// A NUL byte is never part of one bounded text message and must be refused
	// before any parser sees it, because parsers may quote it back. This covers
	// the entire raw input, including a leading mbox envelope line.
	if bytes.IndexByte(raw, 0) >= 0 {
		return parsedEmail{}, fmt.Errorf("%w: message contains a NUL byte", ErrEmailMalformed)
	}
	// An mbox drop may prefix the message with exactly one `From ` envelope
	// line. It starts at byte zero, is terminated by LF (with or without a
	// preceding CR) and is ignored. A second one is ambiguous and refused; the
	// envelope bytes already counted toward the raw-message bound above.
	content := raw
	if bytes.HasPrefix(content, []byte("From ")) {
		if end := bytes.IndexByte(content, '\n'); end >= 0 {
			content = content[end+1:]
		}
	}
	if bytes.HasPrefix(content, []byte("From ")) {
		return parsedEmail{}, fmt.Errorf("%w: more than one leading From line", ErrEmailMalformed)
	}
	buffered := bufio.NewReader(bytes.NewReader(content))
	// textproto enforces the RFC822 header grammar: a malformed line or a
	// missing header/body separator is an error here. Its error quotes the
	// offending raw line, so only fixed text is reported.
	header, err := textproto.NewReader(buffered).ReadMIMEHeader()
	if err != nil {
		return parsedEmail{}, fmt.Errorf("%w: headers", ErrEmailMalformed)
	}
	fields := 0
	for _, values := range header {
		fields += len(values)
	}
	if fields > MaxEmailHeaderCount {
		return parsedEmail{}, fmt.Errorf("%w: %d header fields; at most %d are allowed",
			ErrEmailOversize, fields, MaxEmailHeaderCount)
	}

	identities := header.Values("Message-Id")
	if len(identities) != 1 {
		return parsedEmail{}, fmt.Errorf("%w: want exactly one Message-ID, found %d", ErrEmailMalformed, len(identities))
	}
	messageID := strings.TrimSpace(identities[0])
	if messageID == "" {
		return parsedEmail{}, fmt.Errorf("%w: Message-ID is empty", ErrEmailMalformed)
	}
	// The bound applies to the raw header value before any bracket removal, so
	// a sender cannot smuggle a larger identity in behind conventional
	// brackets.
	if len(messageID) > MaxEmailMessageIDBytes {
		return parsedEmail{}, fmt.Errorf("%w: Message-ID is %d bytes; at most %d are allowed",
			ErrEmailOversize, len(messageID), MaxEmailMessageIDBytes)
	}
	// Exactly one surrounding `<...>` pair is conventional framing, never part
	// of the identity. Only a single pair is removed and no other character is
	// trimmed; case is preserved.
	if len(messageID) >= 2 && messageID[0] == '<' && messageID[len(messageID)-1] == '>' {
		messageID = messageID[1 : len(messageID)-1]
	}
	if messageID == "" {
		return parsedEmail{}, fmt.Errorf("%w: Message-ID has no identity token", ErrEmailMalformed)
	}
	// JSON encoding replaces invalid UTF-8 bytes with a three-byte rune, which
	// would inflate the bounded identity while the receipt key keeps the raw
	// bytes; refuse it here, as the body and subject are refused below.
	if !utf8.ValidString(messageID) {
		return parsedEmail{}, fmt.Errorf("%w: Message-ID is not valid UTF-8", ErrEmailMalformed)
	}
	if !validEmailMessageID(messageID) {
		return parsedEmail{}, fmt.Errorf("%w: Message-ID has invalid characters", ErrEmailMalformed)
	}
	if err := validateEmailBodyHeaders(header); err != nil {
		return parsedEmail{}, err
	}

	body, err := io.ReadAll(io.LimitReader(buffered, MaxEmailBodyBytes+1))
	if err != nil {
		return parsedEmail{}, fmt.Errorf("email intake: message body read failed")
	}
	if len(body) > MaxEmailBodyBytes {
		return parsedEmail{}, fmt.Errorf("%w: body is %d bytes; at most %d are allowed",
			ErrEmailOversize, len(body), MaxEmailBodyBytes)
	}
	// JSON encoding replaces invalid UTF-8 bytes with a three-byte rune, which
	// would inflate every bound after the check; refuse it here instead.
	if !utf8.Valid(body) {
		return parsedEmail{}, fmt.Errorf("%w: body is not valid UTF-8", ErrEmailMalformed)
	}
	// The raw body above was bounded as received. Only after that bound has
	// been enforced is CRLF folded to LF, so a CRLF-heavy message cannot grow
	// past the accepted-body bound. A bare CR is content, not a line ending,
	// and stays distinct.
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))

	subject := strings.TrimSpace(header.Get("Subject"))
	if !utf8.ValidString(subject) {
		return parsedEmail{}, fmt.Errorf("%w: subject is not valid UTF-8", ErrEmailMalformed)
	}
	// The digest and the submitted description both come from the same
	// normalized body, so the served receipt fingerprints what is submitted.
	digest := sha256.New()
	fmt.Fprintf(digest, "%d:", len(subject))
	digest.Write([]byte(subject))
	digest.Write([]byte{0})
	digest.Write(body)
	return parsedEmail{
		messageID:     messageID,
		subject:       subject,
		description:   strings.TrimSpace(string(body)),
		contentDigest: hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

// validEmailMessageID accepts one canonical opaque identity token with no
// whitespace or control character. One conventional bracket pair has already
// been removed, so brackets are neither required nor trimmed again.
func validEmailMessageID(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// validateEmailBodyHeaders refuses multipart and attached messages and accepts
// only a single text/plain part. Nothing here selects scope or authority.
func validateEmailBodyHeaders(header textproto.MIMEHeader) error {
	if contentType := strings.TrimSpace(header.Get("Content-Type")); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			// The parser error can quote the attacker-supplied value.
			return fmt.Errorf("%w: content type is not a media type", ErrEmailMalformed)
		}
		if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
			return fmt.Errorf("%w: multipart message", ErrEmailAttachment)
		}
		if !strings.EqualFold(mediaType, "text/plain") {
			return fmt.Errorf("%w: content type is not text/plain", ErrEmailMalformed)
		}
	}
	for _, disposition := range header.Values("Content-Disposition") {
		if strings.TrimSpace(disposition) == "" {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(disposition)
		if err != nil {
			return fmt.Errorf("%w: content disposition is not a media type", ErrEmailMalformed)
		}
		if strings.EqualFold(mediaType, "attachment") {
			return fmt.Errorf("%w: content disposition is attachment", ErrEmailAttachment)
		}
	}
	return nil
}
