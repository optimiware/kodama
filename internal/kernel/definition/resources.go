package definition

import (
	"fmt"
	"math"
	"strings"

	"github.com/optimiware/kodema/internal/fault"
)

// Structural container resource bounds enforced at definition validation.
// Runtime adapters impose tighter installation-level maximums on top of these
// structural bounds so a workload can never request unbounded host resources.
const (
	// MinContainerMemoryBytes is the smallest admissible container memory limit
	// (16 MiB).
	MinContainerMemoryBytes int64 = 16 << 20
	// MaxContainerMemoryBytes is the largest structurally admissible container
	// memory limit (1 TiB).
	MaxContainerMemoryBytes int64 = 1 << 40
	// MinContainerCPUMillicores is the smallest admissible container CPU limit
	// (50m = 0.05 CPU).
	MinContainerCPUMillicores int64 = 50
	// MaxContainerCPUMillicores is the largest structurally admissible
	// container CPU limit (1024 CPUs).
	MaxContainerCPUMillicores int64 = 1024 * 1000
)

// ValidateResources checks an optional container resource declaration. A nil
// declaration is valid and selects the installation default. A non-nil
// declaration must specify at least one of Memory or CPU, and every specified
// quantity must parse cleanly within the structural bounds.
func ValidateResources(resources *Resources) error {
	if resources == nil {
		return nil
	}
	if resources.Memory == "" && resources.CPU == "" {
		return invalidInput("container resources must specify at least one of memory or cpu")
	}
	if resources.Memory != "" {
		if _, err := ParseMemoryQuantity(resources.Memory); err != nil {
			return err
		}
	}
	if resources.CPU != "" {
		if _, err := ParseCPUQuantity(resources.CPU); err != nil {
			return err
		}
	}
	return nil
}

// ParseMemoryQuantity parses one Kubernetes-compatible memory quantity string
// into bytes and validates that it lies within [MinContainerMemoryBytes,
// MaxContainerMemoryBytes].
//
// Supported forms:
//   - bare positive integer bytes (e.g. "268435456")
//   - binary IEC units Ki, Mi, Gi, Ti (e.g. "512Mi", "2Gi", "1.5Gi")
//   - decimal SI units k, K, M, G, T (e.g. "512M", "2G")
func ParseMemoryQuantity(raw string) (int64, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("memory quantity %q must not be empty or have surrounding whitespace", raw))
	}
	numPart, suffix := splitQuantity(raw)
	if numPart == "" {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("memory quantity %q is invalid", raw))
	}
	multiplier, ok := memoryMultiplier(suffix)
	if !ok {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("memory quantity %q has unsupported unit %q", raw, suffix))
	}
	bytes, err := parseScaledPositive(numPart, multiplier)
	if err != nil {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("memory quantity %q is invalid: %v", raw, err))
	}
	if bytes < MinContainerMemoryBytes || bytes > MaxContainerMemoryBytes {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("memory quantity %q (%s) is outside [%s, %s]",
			raw, FormatMemoryBytes(bytes), FormatMemoryBytes(MinContainerMemoryBytes), FormatMemoryBytes(MaxContainerMemoryBytes)))
	}
	return bytes, nil
}

// ParseCPUQuantity parses one Kubernetes-compatible CPU quantity string into
// millicores and validates that it lies within [MinContainerCPUMillicores,
// MaxContainerCPUMillicores].
//
// Supported forms:
//   - millicores with trailing 'm' (e.g. "250m", "1500m")
//   - whole or decimal cores with up to 3 fractional digits (e.g. "1", "2", "0.5", "1.25")
func ParseCPUQuantity(raw string) (int64, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q must not be empty or have surrounding whitespace", raw))
	}
	numPart, suffix := splitQuantity(raw)
	if numPart == "" {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q is invalid", raw))
	}
	var millicores int64
	switch suffix {
	case "m":
		if strings.ContainsRune(numPart, '.') {
			return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q must be a whole number of millicores", raw))
		}
		parsed, err := parsePositiveUint(numPart)
		if err != nil {
			return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q is invalid: %v", raw, err))
		}
		millicores = parsed
	case "":
		if dot := strings.IndexByte(numPart, '.'); dot >= 0 && len(numPart)-dot-1 > 3 {
			return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q has finer than millicore precision", raw))
		}
		parsed, err := parseScaledPositive(numPart, 1000)
		if err != nil {
			return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q is invalid: %v", raw, err))
		}
		millicores = parsed
	default:
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q has unsupported unit %q", raw, suffix))
	}
	if millicores < MinContainerCPUMillicores || millicores > MaxContainerCPUMillicores {
		return 0, fault.New(fault.InvalidInput, fmt.Sprintf("cpu quantity %q (%s) is outside [%s, %s]",
			raw, FormatCPUMillicores(millicores), FormatCPUMillicores(MinContainerCPUMillicores), FormatCPUMillicores(MaxContainerCPUMillicores)))
	}
	return millicores, nil
}

// FormatMemoryBytes renders a positive byte limit in a concise, human-readable
// binary unit suitable for operator diagnostics (e.g. "2 GiB", "512 MiB").
func FormatMemoryBytes(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const (
		kib = int64(1 << 10)
		mib = int64(1 << 20)
		gib = int64(1 << 30)
		tib = int64(1 << 40)
	)
	switch {
	case bytes%tib == 0:
		return fmt.Sprintf("%d TiB", bytes/tib)
	case bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	case bytes%mib == 0:
		return fmt.Sprintf("%d MiB", bytes/mib)
	case bytes%kib == 0:
		return fmt.Sprintf("%d KiB", bytes/kib)
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// FormatCPUMillicores renders a positive millicore count in canonical
// Kubernetes syntax (e.g. "2", "500m", "1500m").
func FormatCPUMillicores(millicores int64) string {
	if millicores <= 0 {
		return ""
	}
	if millicores%1000 == 0 {
		return fmt.Sprintf("%d", millicores/1000)
	}
	return fmt.Sprintf("%dm", millicores)
}

func splitQuantity(raw string) (string, string) {
	index := 0
	for index < len(raw) {
		ch := raw[index]
		if (ch >= '0' && ch <= '9') || ch == '.' {
			index++
			continue
		}
		break
	}
	return raw[:index], raw[index:]
}

func memoryMultiplier(suffix string) (int64, bool) {
	switch suffix {
	case "":
		return 1, true
	case "Ki":
		return 1 << 10, true
	case "Mi":
		return 1 << 20, true
	case "Gi":
		return 1 << 30, true
	case "Ti":
		return 1 << 40, true
	case "k", "K":
		return 1_000, true
	case "M":
		return 1_000_000, true
	case "G":
		return 1_000_000_000, true
	case "T":
		return 1_000_000_000_000, true
	default:
		return 0, false
	}
}

func parsePositiveUint(digits string) (int64, error) {
	if digits == "" {
		return 0, fmt.Errorf("missing numeric value")
	}
	var value int64
	for index := 0; index < len(digits); index++ {
		ch := digits[index]
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("unexpected character %q", ch)
		}
		digit := int64(ch - '0')
		if value > (math.MaxInt64-digit)/10 {
			return 0, fmt.Errorf("value overflows int64")
		}
		value = value*10 + digit
	}
	if value <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return value, nil
}

// parseScaledPositive parses a positive integer or decimal string multiplied by
// multiplier without floating-point rounding drift or int64 overflow.
func parseScaledPositive(numPart string, multiplier int64) (int64, error) {
	intPart, fracPart, hasDot := strings.Cut(numPart, ".")
	if hasDot {
		if strings.ContainsRune(fracPart, '.') {
			return 0, fmt.Errorf("multiple decimal points")
		}
		if intPart == "" && fracPart == "" {
			return 0, fmt.Errorf("missing digits")
		}
		if fracPart == "" {
			return 0, fmt.Errorf("trailing decimal point")
		}
	}
	var whole int64
	if intPart != "" {
		for index := 0; index < len(intPart); index++ {
			ch := intPart[index]
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("unexpected character %q", ch)
			}
			digit := int64(ch - '0')
			if whole > (math.MaxInt64-digit)/10 {
				return 0, fmt.Errorf("value overflows int64")
			}
			whole = whole*10 + digit
		}
	}
	if whole > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("value overflows int64")
	}
	total := whole * multiplier

	if hasDot {
		if multiplier == 1 {
			return 0, fmt.Errorf("fractional bytes are not supported")
		}
		var frac int64
		var denom int64 = 1
		// Consume up to 9 fractional digits for exact integer arithmetic.
		for index := 0; index < len(fracPart); index++ {
			ch := fracPart[index]
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("unexpected character %q", ch)
			}
			if index < 9 {
				frac = frac*10 + int64(ch-'0')
				denom *= 10
			}
		}
		// Compute (frac * multiplier) / denom safely using quotient/remainder.
		q := multiplier / denom
		r := multiplier % denom
		if frac > 0 && q > math.MaxInt64/frac {
			return 0, fmt.Errorf("value overflows int64")
		}
		add := frac*q + (frac*r)/denom
		if total > math.MaxInt64-add {
			return 0, fmt.Errorf("value overflows int64")
		}
		total += add
	}
	if total <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return total, nil
}
