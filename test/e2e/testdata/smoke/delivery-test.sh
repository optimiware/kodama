# AC08 exact-digest test action. This action itself is declared with the same
# digest-pinned image the build action recorded. It proves the recorded output
# and the deploy candidate name that exact digest, then submits the declared
# outputs twice through the mounted execution credential with the same
# idempotency key; the identical retry must converge without a second effect.
set -eu
expected=$(cat /kodama/outputs/build/image)
test -n "$SMOKE_IMAGE"
test "$expected" = "$SMOKE_IMAGE"
candidate=$(cat "$W49_WORKSPACE/ac08-candidate")
test_result=$(cat "$W49_WORKSPACE/ac08-test-result")
test "$test_result" = "pass:$candidate"
cd "$W49_WORKSPACE/repo"
test "$(git rev-parse HEAD)" = "$candidate"
test "$(git show HEAD:deploy/image.txt)" = "$SMOKE_IMAGE"

python3 - "$candidate" <<'AC08_OUTPUTS'
import json
import os
import ssl
import sys
import urllib.error
import urllib.request

candidate = sys.argv[1]
base = os.environ["KODAMA_URL"].rstrip("/")
if not base.startswith("https://"):
    raise SystemExit("KODAMA_URL must use https")
execution = os.environ["KODAMA_EXECUTION"]
action_name = os.environ["KODAMA_ACTION"]
ca_file = os.environ.get("SMOKE_CA_FILE", "")


def token():
    with open("/kodama/secrets/KODAMA_TOKEN", "r", encoding="utf-8") as handle:
        return handle.read().strip()


def tls_context():
    if not ca_file:
        return ssl.create_default_context()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.check_hostname = True
    context.verify_mode = ssl.CERT_REQUIRED
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_verify_locations(cafile=ca_file)
    return context


opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=tls_context()))


def call(method, path, envelope):
    body = json.dumps(envelope, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(
        base + path,
        data=body,
        method=method,
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": "Bearer " + token(),
        },
    )
    try:
        with opener.open(request, timeout=20) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


status, data = call(
    "GET",
    "/api/v1/executions/" + execution,
    {"input": {"execution_id": execution}},
)
if status != 200:
    raise SystemExit("execution.inspect returned status %d" % status)
view = json.loads(data.decode("utf-8"))["body"]
action = next((item for item in view.get("actions", []) if item.get("name") == action_name), None)
if action is None:
    raise SystemExit("the current action is not visible in execution.inspect")
action_id = action["action_id"]
outputs = {
    "candidate": candidate,
    "test_result": "pass:" + candidate,
}
envelope = {
    "input": {
        "execution_id": execution,
        "action_id": action_id,
        "outputs": outputs,
    },
    "expected_version": view["version"],
    "idempotency_key": "ac08:" + execution + ":" + action_id,
}
path = "/api/v1/executions/" + execution + "/actions/" + action_id + "/outputs"
first_status = None
first_body = b""
for _ in range(30):
    first_status, first_body = call("PUT", path, envelope)
    if first_status == 200:
        break
    if first_status != 409:
        break
    # A running execution advances its version as observations commit. Refresh
    # the expected version and retry the SAME input and idempotency key.
    status, data = call("GET", "/api/v1/executions/" + execution, {"input": {"execution_id": execution}})
    if status != 200:
        raise SystemExit("execution.inspect after a version conflict returned status %d" % status)
    envelope["expected_version"] = json.loads(data.decode("utf-8"))["body"]["version"]
if first_status != 200:
    raise SystemExit(
        "AC08 first output submission status %d; body=%r"
        % (first_status, first_body[:400])
    )
# The execution version advances with committed transitions; refresh it for
# the identical retry while keeping the same idempotency key and input content.
status, data = call(
    "GET",
    "/api/v1/executions/" + execution,
    {"input": {"execution_id": execution}},
)
if status != 200:
    raise SystemExit("execution.inspect before the retry returned status %d" % status)
envelope["expected_version"] = json.loads(data.decode("utf-8"))["body"]["version"]
second_status, second_body = call("PUT", path, envelope)
if second_status != 200:
    raise SystemExit(
        "AC08 identical output retry status %d; body=%r"
        % (second_status, second_body[:400])
    )
print("ac08-output-retry-converged")
AC08_OUTPUTS

printf '%s\n' "$SMOKE_TEST_MARKER"
