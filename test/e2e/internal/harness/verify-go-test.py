import json
import sys


def main():
    if len(sys.argv) < 3:
        print(
            "verify-go-test: usage: verify-go-test.py <go-test-json-file> <expected-test>...",
            file=sys.stderr,
        )
        return 2
    path = sys.argv[1]
    expected = sys.argv[2:]

    ran = set()
    skipped = set()
    failed = set()
    try:
        with open(path, "r", encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(event, dict):
                    continue
                test = event.get("Test")
                if not test:
                    continue
                # Subtest identities carry the parent prefix
                # (TestReleaseSmoke/ProtectedTask). They are tracked so a
                # missing or skipped required scenario fails the gate just like
                # a missing top-level test.
                action = event.get("Action")
                if action == "run":
                    ran.add(test)
                elif action == "skip":
                    skipped.add(test)
                elif action == "fail":
                    failed.add(test)
    except OSError as error:
        print("verify-go-test: cannot read the go test result: %s" % error, file=sys.stderr)
        return 2

    def under(name, names):
        # A selected test covers its own identity and every subtest below it:
        # an expected parent must fail when one of its subtests was skipped or
        # failed, because that scenario never produced its proof.
        return sorted(
            actual for actual in names if actual == name or actual.startswith(name + "/")
        )

    if not ran:
        print("verify-go-test: the selected run executed zero tests", file=sys.stderr)
        return 1
    missing = [name for name in expected if name not in ran]
    if missing:
        print(
            "verify-go-test: expected test(s) did not run: %s" % ", ".join(missing),
            file=sys.stderr,
        )
        return 1
    skipped_selected = sorted(
        {actual for name in expected for actual in under(name, skipped)}
    )
    if skipped_selected:
        print(
            "verify-go-test: selected test(s) skipped: %s" % ", ".join(skipped_selected),
            file=sys.stderr,
        )
        return 1
    failed_selected = sorted(
        {actual for name in expected for actual in under(name, failed)}
    )
    if failed_selected:
        print(
            "verify-go-test: selected test(s) failed: %s" % ", ".join(failed_selected),
            file=sys.stderr,
        )
        return 1
    print(
        "verify-go-test: %d selected test(s) ran, none skipped or failed"
        % len(expected)
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
