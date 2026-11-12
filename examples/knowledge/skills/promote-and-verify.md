# Skill: promote and verify

Use this skill to promote an already-built digest and verify the rollout.

## Steps

1. Write the deploy-repository work-branch candidate for the exact commit.
2. Request approval of the exact ref, expected base and candidate from a current
   human role.
3. Integrate the approved protected update without rebuilding.
4. Observe readiness explicitly and record the result.

## Rollback

Rollback is a new promotion of an older digest through the same protected
update. It is not an automatic data rollback and it never rebuilds the image.

## Notes

- Promotion and rollback share the registered deploy resource and ref.
- Approval binds the exact candidate; extra display context does not change it.
- Readiness is an observed result, not a command exit.
