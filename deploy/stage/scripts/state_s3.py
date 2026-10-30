#!/usr/bin/env python3
"""List and delete OpenTofu state objects in the Scaleway state bucket (the scw CLI cannot list objects).

    state_s3.py list [prefix]      one object key per line
    state_s3.py delete <key>...    removes the current version of each key (a delete marker; versioning keeps the rest)
    state_s3.py selftest           checks the request signing against AWS's published example

Environment: SCW_ACCESS_KEY, SCW_SECRET_KEY, KODAMA_TFSTATE_BUCKET; SCW_S3_ENDPOINT/SCW_S3_REGION override fr-par.
Standard library only, so the stage scripts need nothing beyond python3. The bucket itself is never deleted here.
"""
import datetime
import hashlib
import hmac
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

EMPTY_SHA256 = hashlib.sha256(b"").hexdigest()


def _quote(value: str, safe: str = "-_.~") -> str:
    return urllib.parse.quote(value, safe=safe)


def signature(method, path, query, headers, payload_hash, secret, region, amzdate):
    """AWS Signature Version 4 (service s3); returns (signature, signed_headers, scope)."""
    names = sorted(h.lower() for h in headers)
    lowered = {k.lower(): v.strip() for k, v in headers.items()}
    canonical = "\n".join([
        method,
        _quote(path, safe="/-_.~"),
        "&".join(f"{_quote(k)}={_quote(v)}" for k, v in sorted(query.items())),
        "".join(f"{n}:{lowered[n]}\n" for n in names),
        ";".join(names),
        payload_hash,
    ])
    scope = f"{amzdate[:8]}/{region}/s3/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amzdate, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ("AWS4" + secret).encode()
    for part in (amzdate[:8], region, "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    return hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest(), ";".join(names), scope


def request(method, path, query=None):
    endpoint = os.environ.get("SCW_S3_ENDPOINT", "https://s3.fr-par.scw.cloud")
    region = os.environ.get("SCW_S3_REGION", "fr-par")
    access, secret = os.environ["SCW_ACCESS_KEY"], os.environ["SCW_SECRET_KEY"]
    query = query or {}
    host = urllib.parse.urlsplit(endpoint).netloc
    amzdate = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    headers = {"host": host, "x-amz-content-sha256": EMPTY_SHA256, "x-amz-date": amzdate}
    sig, signed, scope = signature(method, path, query, headers, EMPTY_SHA256, secret, region, amzdate)
    auth = f"AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={signed}, Signature={sig}"
    url = endpoint + _quote(path, safe="/-_.~") + ("?" + "&".join(f"{_quote(k)}={_quote(v)}" for k, v in sorted(query.items())) if query else "")
    req = urllib.request.Request(url, method=method, headers={**{k: v for k, v in headers.items() if k != "host"}, "Authorization": auth})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.read()
    except urllib.error.HTTPError as err:
        sys.exit(f"[state_s3] {method} {path} failed: HTTP {err.code} {err.read()[:200].decode(errors='replace')}")


def list_keys(bucket, prefix=""):
    token = ""
    while True:
        query = {"list-type": "2", "prefix": prefix}
        if token:
            query["continuation-token"] = token
        root = ET.fromstring(request("GET", f"/{bucket}", query))
        ns = {"s3": root.tag.partition("}")[0].lstrip("{")}
        for node in root.findall("s3:Contents/s3:Key", ns):
            yield node.text
        if (root.findtext("s3:IsTruncated", default="false", namespaces=ns) or "false") != "true":
            return
        token = root.findtext("s3:NextContinuationToken", default="", namespaces=ns)


def selftest():
    # GET Object with a Range header, from "Signature Calculations for the Authorization Header" (AWS S3 documentation).
    headers = {"host": "examplebucket.s3.amazonaws.com", "range": "bytes=0-9", "x-amz-content-sha256": EMPTY_SHA256, "x-amz-date": "20130524T000000Z"}
    got, signed, _ = signature("GET", "/test.txt", {}, headers, EMPTY_SHA256, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "20130524T000000Z")
    want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
    if got != want or signed != "host;range;x-amz-content-sha256;x-amz-date":
        sys.exit(f"[state_s3] selftest failed: {got} != {want}")
    print("state_s3 selftest ok")


def main(argv):
    if len(argv) < 2 or argv[1] not in ("list", "delete", "selftest"):
        sys.exit(__doc__)
    if argv[1] == "selftest":
        return selftest()
    bucket = os.environ["KODAMA_TFSTATE_BUCKET"]
    if argv[1] == "list":
        for key in list_keys(bucket, argv[2] if len(argv) > 2 else ""):
            print(key)
        return
    for key in argv[2:]:
        if not key or key.endswith("/"):
            sys.exit(f"[state_s3] refusing to delete '{key}': give exact object keys")
        request("DELETE", f"/{bucket}/{key}")
        print(f"deleted {key}")


if __name__ == "__main__":
    main(sys.argv)
