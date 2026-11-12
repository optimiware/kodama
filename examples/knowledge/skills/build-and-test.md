# Skill: build and test

Use this skill when a change must become a digest-pinned image and be tested
before promotion.

## Steps

1. Build the image and publish its bytes to the external registry.
2. Submit the immutable `sha256` digest as the declared `image` output.
3. Run the test recipe against the forwarded digest; never rebuild inside the
   test step.
4. Keep the exact commit and digest in the handoff so a retry can reuse the same
   candidate.

## Notes

- A mutable tag is not a pinned reference.
- A lost output response is retried with the same digest, not a new build.
- Command success is not proof that a deployment is healthy.
