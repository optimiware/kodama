#!/bin/sh
# Deterministic fixture workload: print the marker baked into the image.
# No network, no package installation and no runtime writes.
set -eu
cat /fixture/marker
