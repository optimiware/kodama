# Run-profile qualification images

T-111 builds and publishes **no** image. The qualification driver
(`test/integration/kube/runprofile/`) uses only public upstream images pinned by
index digest in its `pins.json`; the report
([`docs/acceptance/run-resource-profile.md`](../../docs/acceptance/run-resource-profile.md))
records the digests the nodes actually ran. Builder images for the product profile
(P4/P6) belong here only after the node/Pod Security handoffs in that report are
installed and the builds are re-qualified; until then no builder image is advertised.
