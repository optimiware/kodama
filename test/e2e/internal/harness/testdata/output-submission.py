import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

CA_FILE = __KODAMA_CA_FILE__
OUTPUTS = __KODAMA_OUTPUTS__
TOKEN_FILE = "/kodama/secrets/KODAMA_TOKEN"
MAX_BODY = 262144


class RefuseRedirect(urllib.request.HTTPRedirectHandler):
    # Refuse every redirect; an installation answer is observed, never chased.
    def redirect_request(self, request, response, code, message, headers, new_url):
        return None


def token():
    # Re-read the mounted credential for every call so a rotated execution
    # token is observed without keeping a stale copy in memory.
    with open(TOKEN_FILE, "r", encoding="utf-8") as handle:
        return handle.read().strip()


def tls_context():
    # A standard client context, not the distribution's create_default_context:
    # that preset enables the optional VERIFY_X509_STRICT extra checks, which
    # reject the W48 installation CA for lacking a keyUsage extension. This
    # context still requires a trusted chain (CERT_REQUIRED), verifies the host
    # name, and refuses anything older than TLS 1.2; it never disables
    # verification. A publicly trusted installation uses the system roots.
    if not CA_FILE:
        return ssl.create_default_context()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.check_hostname = True
    context.verify_mode = ssl.CERT_REQUIRED
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_verify_locations(cafile=CA_FILE)
    return context


def client():
    return urllib.request.build_opener(
        urllib.request.HTTPSHandler(context=tls_context()),
        RefuseRedirect(),
    )


def call(connection, method, path, envelope):
    body = json.dumps(envelope, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(
        BASE_URL + path,
        data=body,
        method=method,
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": "Bearer " + token(),
        },
    )
    try:
        with connection.open(request, timeout=20) as response:
            return response.status, response.read(MAX_BODY + 1)
    except urllib.error.HTTPError as error:
        return error.code, error.read(MAX_BODY + 1)


BASE_URL = os.environ["KODAMA_URL"].rstrip("/")
if not BASE_URL.startswith("https://"):
    raise SystemExit("KODAMA_URL must use https; refusing to send the bearer token in plaintext")
EXECUTION = os.environ["KODAMA_EXECUTION"]
ACTION_NAME = os.environ["KODAMA_ACTION"]


def main():
    connection = client()
    for attempt in range(20):
        status, data = call(
            connection,
            "GET",
            "/api/v1/executions/" + EXECUTION,
            {"input": {"execution_id": EXECUTION}},
        )
        if status != 200:
            raise SystemExit("execution.inspect returned status %d" % status)
        if len(data) > MAX_BODY:
            raise SystemExit("execution.inspect response exceeded the bound")
        view = json.loads(data.decode("utf-8"))["body"]
        action = next(
            (item for item in view.get("actions", []) if item.get("name") == ACTION_NAME),
            None,
        )
        if action is None:
            raise SystemExit("the current action is not visible in execution.inspect")
        action_id = action["action_id"]
        status, data = call(
            connection,
            "PUT",
            "/api/v1/executions/" + EXECUTION + "/actions/" + action_id + "/outputs",
            {
                "input": {
                    "execution_id": EXECUTION,
                    "action_id": action_id,
                    "outputs": OUTPUTS,
                },
                "expected_version": view["version"],
                "idempotency_key": "q01out:"
                + EXECUTION
                + ":"
                + action_id
                + ":"
                + str(attempt),
            },
        )
        if status == 200:
            return 0
        code = ""
        try:
            code = json.loads(data.decode("utf-8")).get("code", "")
        except ValueError:
            code = ""
        if code != "version_conflict":
            raise SystemExit("execution.action.outputs returned status %d" % status)
        time.sleep(0.2)
    raise SystemExit("output submission did not converge")


if __name__ == "__main__":
    sys.exit(main())
