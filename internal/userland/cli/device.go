package cli

import (
	"fmt"
	"math"
	"time"
)

// defaultDevicePollInterval is the RFC 8628 default polling interval.
const defaultDevicePollInterval = 5 * time.Second

// maxDeviceLoginLifetime is the hard cap on one device flow, independent of a
// maliciously large expires_in.
const maxDeviceLoginLifetime = 15 * time.Minute

// maxDuration is the largest representable duration.
const maxDuration = time.Duration(math.MaxInt64)

// Error renders the public OAuth error code and description. It never renders
// a token, code or verifier.
func (err *OAuthError) Error() string {
	if err == nil {
		return "oauth error"
	}
	if err.Description == "" {
		return "oauth error: " + err.Code
	}
	return "oauth error: " + err.Code + ": " + err.Description
}

// SelectLoginMode chooses the login flow. An explicit force wins, conflicting
// forces are a usage error, an SSH session uses the device flow and a graphical
// display uses the browser flow.
func SelectLoginMode(environment LoginEnvironment) (LoginMode, error) {
	if environment.ForceDevice && environment.ForceBrowser {
		return "", fmt.Errorf("login cannot force both the browser and device flows")
	}
	if environment.ForceDevice {
		return LoginDevice, nil
	}
	if environment.ForceBrowser {
		return LoginBrowser, nil
	}
	if environment.SSHConnection != "" {
		return LoginDevice, nil
	}
	if environment.Display != "" {
		return LoginBrowser, nil
	}
	return LoginDevice, nil
}

// NextPollInterval advances the RFC 8628 device polling schedule. A pending
// authorization keeps the current interval, slow_down adds five seconds
// (saturated at the maximum duration) and every other code is terminal.
func NextPollInterval(current time.Duration, errorCode string) (time.Duration, bool, error) {
	switch errorCode {
	case "authorization_pending":
		if current <= 0 {
			return defaultDevicePollInterval, false, nil
		}
		return current, false, nil
	case "slow_down":
		base := current
		if base <= 0 {
			base = defaultDevicePollInterval
		}
		if base > maxDuration-defaultDevicePollInterval {
			return maxDuration, false, nil
		}
		return base + defaultDevicePollInterval, false, nil
	default:
		return 0, true, &OAuthError{Code: errorCode, Description: "the device authorization is terminal"}
	}
}
