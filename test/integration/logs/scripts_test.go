package logs_test

import "fmt"

// Step scripts. The workload image is a minimal shell userland; the entrypoint
// runs `/bin/sh -ec <script>` and captures its merged stdout/stderr.

// finishOK writes the results manifest of a completed command.
const finishOK = `printf '{"outcome":"completed"}' > "$KODAMA_RESULTS_FILE"`

// dumpIdentity prints every bootstrap file the command can read, including the
// current runtime token and any admission material: the delivered credentials
// that must never reach a log.
const dumpIdentity = `for f in /kodama/identity/*; do printf 'identity-file %s=%s\n' "$f" "$(cat "$f" 2>/dev/null)"; done`

// chatter prints ordinary stdout and stderr lines and the delivered credentials.
func chatter(tag string) string {
	return fmt.Sprintf(`echo "%[1]s stdout line one"
echo "%[1]s stderr line two" >&2
%[2]s
echo "%[1]s done"
%[3]s
`, tag, dumpIdentity, finishOK)
}

// numbered prints n numbered lines.
func numbered(tag string, n int) string {
	return fmt.Sprintf(`i=1
while [ $i -le %d ]; do printf '%s-line-%%04d\n' $i; i=$((i+1)); done
`, n, tag)
}

// bulk prints well over the retained capture capacity (1 MiB diagnostic budget).
func bulk(tag string) string {
	return fmt.Sprintf(`echo %[1]s-begin
yes %[1]s-bulk-padding-padding-padding-padding-padding-padding-padding-padding | head -n 20000
echo %[1]s-end
`, tag)
}
