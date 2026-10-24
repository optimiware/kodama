# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub: go to the [Security tab](https://github.com/optimiware/kodama/security) and choose **Report a vulnerability**. Include what you found, how to reproduce it, and the affected version or commit.

We will acknowledge your report, keep you updated while we investigate, and credit you in the fix if you wish.

## Supported versions

Kodama is early and has no supported release line yet. Security fixes land on `main`.

## Scope

Kodama's design treats tenant isolation, secret handling and agent permissions as security boundaries. Reports about any of them are welcome, including ones that only affect the documented design. The [project status](docs/status.md) lists what is not production-ready yet.
