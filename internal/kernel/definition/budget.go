package definition

import (
	"fmt"
	"math"
)

const (
	// MaxBudgetTokens is the sane upper bound on a declared token budget (1B).
	MaxBudgetTokens int64 = 1_000_000_000
	// MaxBudgetCostUSD is the sane upper bound on a declared USD budget ($1M).
	MaxBudgetCostUSD float64 = 1_000_000.0
)

// ValidateBudget checks that an optional LLM budget declaration specifies at
// least one positive, finite, bounded limit and no negative values.
func ValidateBudget(budget *Budget) error {
	if budget == nil {
		return nil
	}
	if budget.MaxTokens < 0 {
		return invalidInput("budget max_tokens must not be negative")
	}
	if budget.MaxTokens > MaxBudgetTokens {
		return invalidInput("budget max_tokens %d exceeds maximum %d", budget.MaxTokens, MaxBudgetTokens)
	}
	if math.IsNaN(budget.MaxCostUSD) || math.IsInf(budget.MaxCostUSD, 0) {
		return invalidInput("budget max_cost_usd must be a finite number")
	}
	if budget.MaxCostUSD < 0 {
		return invalidInput("budget max_cost_usd must not be negative")
	}
	if budget.MaxCostUSD > MaxBudgetCostUSD {
		return invalidInput("budget max_cost_usd %.2f exceeds maximum %.2f", budget.MaxCostUSD, MaxBudgetCostUSD)
	}
	if budget.MaxTokens == 0 && budget.MaxCostUSD == 0 {
		return invalidInput("budget must specify at least one positive max_tokens or max_cost_usd limit")
	}
	return nil
}

func validateContainerBudget(action string, budget *Budget) error {
	if budget == nil {
		return nil
	}
	if err := ValidateBudget(budget); err != nil {
		return invalidInput("action %q: %s", action, fmt.Sprint(err))
	}
	return nil
}
