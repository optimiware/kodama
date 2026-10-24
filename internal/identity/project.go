package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Bounds of editable project content; they match the T-002 schema constraints.
const (
	MaxKeyChars         = 63
	MaxDisplayNameChars = 256
	MinActiveWIPLimit   = 1
	MaxActiveWIPLimit   = 100000
	DefaultActiveWIP    = 5
	MaxSettingsBytes    = 16 << 10
	MaxSettingsKeys     = 64
	maxSettingsDepth    = 6
)

// Changed-setting names carried by project.settings_changed.
const (
	SettingDisplayName    = "displayName"
	SettingSettings       = "settings"
	SettingActiveWIPLimit = "activeWipLimit"
	SettingKey            = "key"
	SettingRoleBindings   = "roleBindings"
)

var (
	keyPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	settingKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// Project is the identity module's view of a tenant project: immutable
// ownership and key, validated settings, the active work-in-progress limit and
// an optimistic version. Its role bindings live in Binding; a change to either
// advances Version so one project version covers both.
type Project struct {
	Scope          domain.Scope
	Key            string
	DisplayName    string
	Settings       json.RawMessage // a JSON object
	ActiveWIPLimit int
	Version        domain.Version
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewProject creates a project at version 1. A zero wipLimit selects the default.
func NewProject(scope domain.Scope, key, displayName string, settings json.RawMessage, wipLimit int, now time.Time) (Project, error) {
	if err := scope.Validate(); err != nil {
		return Project{}, invalid("%v", err)
	}
	if !keyPattern.MatchString(key) {
		return Project{}, invalid("key must match %s", keyPattern)
	}
	if err := validateDisplayName(displayName); err != nil {
		return Project{}, err
	}
	if len(bytes.TrimSpace(settings)) == 0 {
		settings = json.RawMessage(`{}`)
	}
	canon, err := validateSettings(settings)
	if err != nil {
		return Project{}, err
	}
	if wipLimit == 0 {
		wipLimit = DefaultActiveWIP
	}
	if err := validateWIP(wipLimit); err != nil {
		return Project{}, err
	}
	return Project{Scope: scope, Key: key, DisplayName: displayName, Settings: canon, ActiveWIPLimit: wipLimit,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// ProjectPatch is a partial update; nil members are unchanged.
type ProjectPatch struct {
	DisplayName    *string
	Settings       json.RawMessage // replaces the settings object when non-empty
	ActiveWIPLimit *int
}

// Update applies the patch and returns the next version with the names of the
// settings that changed. The key and ownership are immutable and not patchable.
// A patch that changes nothing is ErrNoChange.
func (p Project) Update(patch ProjectPatch, now time.Time) (Project, []string, error) {
	return p.Apply(patch, false, now)
}

// Apply is Update that can additionally record that role bindings of this
// project changed in the same transaction, so one version covers both. With an
// empty patch and bindingsChanged false it is ErrNoChange.
func (p Project) Apply(patch ProjectPatch, bindingsChanged bool, now time.Time) (Project, []string, error) {
	next := p
	var changed []string
	if patch.DisplayName != nil {
		if err := validateDisplayName(*patch.DisplayName); err != nil {
			return Project{}, nil, err
		}
		if *patch.DisplayName != p.DisplayName {
			next.DisplayName = *patch.DisplayName
			changed = append(changed, SettingDisplayName)
		}
	}
	if len(bytes.TrimSpace(patch.Settings)) > 0 {
		canon, err := validateSettings(patch.Settings)
		if err != nil {
			return Project{}, nil, err
		}
		if !jsonEqual(canon, p.Settings) {
			next.Settings = canon
			changed = append(changed, SettingSettings)
		}
	}
	if patch.ActiveWIPLimit != nil {
		if err := validateWIP(*patch.ActiveWIPLimit); err != nil {
			return Project{}, nil, err
		}
		if *patch.ActiveWIPLimit != p.ActiveWIPLimit {
			next.ActiveWIPLimit = *patch.ActiveWIPLimit
			changed = append(changed, SettingActiveWIPLimit)
		}
	}
	if bindingsChanged {
		changed = append(changed, SettingRoleBindings)
	}
	if len(changed) == 0 {
		return Project{}, nil, ErrNoChange
	}
	return next.advance(now, changed)
}

// BindingsChanged advances the version for a role-binding change of this
// project, so both entry points (membership routes and project updates) keep one
// consistent project version.
func (p Project) BindingsChanged(now time.Time) (Project, []string, error) {
	return p.advance(now, []string{SettingRoleBindings})
}

func (p Project) advance(now time.Time, changed []string) (Project, []string, error) {
	p.Version++
	if err := p.Version.Validate(); err != nil {
		return Project{}, nil, invalid("%v", err)
	}
	p.UpdatedAt = now.UTC()
	return p, changed, nil
}

func validateDisplayName(s string) error {
	if utf8.RuneCountInString(s) > MaxDisplayNameChars || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("displayName must be at most %d characters without control characters", MaxDisplayNameChars)
	}
	return nil
}

func validateWIP(n int) error {
	if n < MinActiveWIPLimit || n > MaxActiveWIPLimit {
		return invalid("activeWipLimit must be between %d and %d", MinActiveWIPLimit, MaxActiveWIPLimit)
	}
	return nil
}

// validateSettings requires one bounded JSON object with well-formed member
// names and returns its compact form.
func validateSettings(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > MaxSettingsBytes {
		return nil, invalid("settings must be at most %d bytes", MaxSettingsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, invalid("settings must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("settings must be a single JSON object")
	}
	if len(obj) > MaxSettingsKeys {
		return nil, invalid("settings may have at most %d members", MaxSettingsKeys)
	}
	for k, v := range obj {
		if !settingKeyPattern.MatchString(k) {
			return nil, invalid("settings member names must match %s", settingKeyPattern)
		}
		if depth(v) > maxSettingsDepth {
			return nil, invalid("settings are nested deeper than %d levels", maxSettingsDepth)
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, invalid("settings must be valid JSON")
	}
	return buf.Bytes(), nil
}

func depth(raw json.RawMessage) int {
	d, max := 0, 0
	inString, escaped := false, false
	for _, c := range raw {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			d++
			if d > max {
				max = d
			}
		case c == '}' || c == ']':
			d--
		}
	}
	return max
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(ab, bb)
}
