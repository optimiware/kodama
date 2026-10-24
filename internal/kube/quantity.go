package kube

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Quantity is a Kubernetes resource quantity in milli-units. The module only
// compares and bounds quantities; it never does arithmetic that could overflow.
type Quantity int64

var quantitySuffixes = []struct {
	suffix string
	mult   float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"m", 0.001}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// ParseQuantity parses the decimal and binary forms the policy accepts: a plain
// number, a milli suffix (m), decimal SI (k, M, G, T) and binary SI (Ki..Ti).
func ParseQuantity(s string) (Quantity, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty quantity")
	}
	mult := 1.0
	for _, sf := range quantitySuffixes {
		if strings.HasSuffix(s, sf.suffix) {
			s, mult = strings.TrimSuffix(s, sf.suffix), sf.mult
			break
		}
	}
	if s == "" || strings.ContainsAny(s, "eE+- ") {
		return 0, errors.New("malformed quantity")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("malformed quantity")
	}
	milli := v * mult * 1000
	if milli > float64(math.MaxInt64/2) {
		return 0, errors.New("quantity too large")
	}
	return Quantity(math.Round(milli)), nil
}

// equalQuantity reports whether two quantity strings denote the same amount.
func equalQuantity(a, b string) bool {
	qa, errA := ParseQuantity(a)
	qb, errB := ParseQuantity(b)
	return errA == nil && errB == nil && qa == qb
}

// QuantityBounds is a positive amount with an optional upper ceiling used to
// validate a policy value.
func mustPositive(name, s string) error {
	q, err := ParseQuantity(s)
	if err != nil || q <= 0 {
		return fmt.Errorf("%s must be a positive quantity", name)
	}
	return nil
}
