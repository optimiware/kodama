#!/usr/bin/env python3
"""Refuse candidate images whose registry copy was not built from the candidate commit.

For each digest-pinned image the registry's own manifest and config are read (verifying both digests), and the
config's `org.opencontainers.image.revision` label must equal the full candidate commit. A local image ID, a tag,
a cached layer carrying another revision (for example `dirty-debug`) or an unlabelled image is refused.

The registry is read anonymously over HTTPS (the stage registry namespace is public-read); plain HTTP is accepted
only for loopback hosts so the offline tests can serve a fixture. Credentials are never sent or printed.
Prints one JSON object per verified image; exits 1 on any refusal.
"""

import argparse
import hashlib
import ipaddress
import json
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

LABEL = "org.opencontainers.image.revision"
ACCEPT = ", ".join((
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
))
REF = re.compile(r"^(?P<host>[^/@\s]+)/(?P<repo>[^@\s]+)@(?P<digest>sha256:[0-9a-f]{64})$")
FULL_COMMIT = re.compile(r"^[0-9a-f]{40}$")


class Refused(Exception):
    pass


def loopback(host):
    name = host.rsplit(":", 1)[0].strip("[]") if not host.startswith("[") else host.split("]")[0].strip("[")
    if name == "localhost":
        return True
    try:
        return ipaddress.ip_address(name).is_loopback
    except ValueError:
        return False


def scheme(host):
    return "http" if loopback(host) else "https"


class _KeepCredentialsOnHost(urllib.request.HTTPRedirectHandler):
    """Registries redirect blob reads to object storage: the bearer token must not follow to another host."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and urllib.parse.urlparse(newurl).netloc != urllib.parse.urlparse(req.full_url).netloc:
            new.remove_header("Authorization")
        return new


_OPENER = urllib.request.build_opener(_KeepCredentialsOnHost)


def get(url, headers):
    request = urllib.request.Request(url, headers=headers)
    with _OPENER.open(request, timeout=30) as response:
        return response.read()


def fetch(host, path, token_cache):
    url = f"{scheme(host)}://{host}{path}"
    headers = {"Accept": ACCEPT}
    if token_cache.get(host):
        headers["Authorization"] = "Bearer " + token_cache[host]
    try:
        return get(url, headers)
    except urllib.error.HTTPError as err:
        if err.code != 401:
            raise Refused(f"registry answered HTTP {err.code} for {path.split('/')[-2]}")
        challenge = err.headers.get("WWW-Authenticate", "")
    fields = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
    realm = fields.get("realm", "")
    parsed = urllib.parse.urlparse(realm)
    if parsed.scheme not in ("https", "http") or (parsed.scheme == "http" and not loopback(parsed.netloc)):
        raise Refused("the registry token endpoint is not HTTPS")
    query = urllib.parse.urlencode({k: v for k, v in (("service", fields.get("service")), ("scope", fields.get("scope"))) if v})
    try:
        body = json.loads(get(realm + ("&" if "?" in realm else "?") + query, {}))
        token_cache[host] = body.get("token") or body["access_token"]
        return get(url, {"Accept": ACCEPT, "Authorization": "Bearer " + token_cache[host]})
    except (urllib.error.URLError, ValueError, KeyError):
        raise Refused("the registry could not be read anonymously")


def revision(ref, token_cache):
    m = REF.match(ref)
    if not m:
        raise Refused(f"{ref!r} is not a digest-pinned registry image")
    host, repo, digest = m["host"], m["repo"], m["digest"]
    manifest_bytes = fetch(host, f"/v2/{repo}/manifests/{digest}", token_cache)
    if "sha256:" + hashlib.sha256(manifest_bytes).hexdigest() != digest:
        raise Refused("the registry manifest does not match the pinned digest")
    manifest = json.loads(manifest_bytes)
    if "manifests" in manifest:
        raise Refused("a multi-platform index was pinned; pin the image manifest built for the candidate")
    config_digest = manifest["config"]["digest"]
    config_bytes = fetch(host, f"/v2/{repo}/blobs/{config_digest}", token_cache)
    if "sha256:" + hashlib.sha256(config_bytes).hexdigest() != config_digest:
        raise Refused("the registry image config does not match its digest")
    labels = json.loads(config_bytes).get("config", {}).get("Labels") or {}
    return labels.get(LABEL, "")


def values_image(path):
    """The release image of a Helm values file: top-level `image:` repository@digest."""
    repository = digest = ""
    inside = False
    for line in open(path, encoding="utf-8"):
        if re.match(r"^image:\s*$", line):
            inside = True
            continue
        if inside and re.match(r"^\S", line):
            break
        if inside:
            m = re.match(r'^  (repository|digest):\s*"?([^"\s]+)"?\s*$', line)
            if m and m[1] == "repository":
                repository = m[2]
            elif m:
                digest = m[2]
    if not repository or not digest:
        raise Refused("the values file does not pin the release image by digest")
    return f"{repository}@{digest}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--commit", required=True, help="the full candidate commit")
    parser.add_argument("--values", help="Helm values whose release image is verified")
    parser.add_argument("images", nargs="*", help="further digest-pinned images (the test-only helper)")
    args = parser.parse_args()
    if not FULL_COMMIT.match(args.commit):
        print("verify_image_revision: --commit must be a full 40-hex commit", file=sys.stderr)
        return 1
    refs = list(args.images)
    try:
        if args.values:
            refs.insert(0, values_image(args.values))
        if not refs:
            raise Refused("no image to verify")
        cache = {}
        for ref in refs:
            got = revision(ref, cache)
            print(json.dumps({"image": ref, "revision": got}), flush=True)
            if got != args.commit:
                raise Refused(f"{ref} carries revision {got or 'none'}, not the candidate {args.commit}")
    except (Refused, urllib.error.URLError, ValueError, KeyError) as err:
        print(f"[gate-fail] image provenance: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
