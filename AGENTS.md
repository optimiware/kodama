# Repository instructions

Read [AGENT.md](AGENT.md) for the contribution workflow and task-specific specification pointers before working in this repository.

## Git workflow

1. At the start of every task, including documentation work, create a dedicated task branch and Git worktree under the primary checkout's `.work/<task-id-or-slug>/`. Resume an existing task in its assigned worktree. Keep `.work/` ignored by Git.
2. Create the worktree from the agreed integration branch. Run the following from the primary checkout, substituting the task name and integration branch:

   ```bash
   mkdir -p .work
   git worktree add -b task/T-001-scaffold .work/T-001-scaffold main
   ```

3. Make task edits and run checks inside that worktree. Follow the task's owned paths and use isolated test resources as described in [the task workflow](docs/tasks/README.md#ownership-and-integration).
4. Before merge, the current changes must receive code review approval and pass the task's scoped required tests and checks. Documentation-only tasks require the relevant documentation checks. Include the commands and results in the review handoff. Address review findings, rerun affected checks, and obtain review approval for the updated changes.
5. Product-wide unit validation runs against integrated `main` every five task merges and at queue drain; M1/M2 checkpoints add bounded integration suites. Go-only work does not rerun OpenForge or development Python tests. A failed batch pauses publication; repair its cause and retry with `bun scripts/openforge/src/cli.ts batch --retry`. Do not add a second full suite at publication.
6. Keep the task branch and worktree available while review or tests are pending. Do not merge until both review approval and passing scoped checks are recorded. The coordinator marks a task complete only after the reviewed and tested changes are merged.
7. Clean up as the final step of every merge: from the primary checkout, run `git worktree remove .work/<task-id-or-slug>` and `git branch -d task/<task-id-or-slug>`, then confirm with `git worktree list` that only other tasks remain. Use `-d`, not `-D`, so Git refuses to delete an unmerged branch. Keep the worktree and branch only when the user asks to keep them or the merge has not happened. Never remove another task's worktree. Remove cached tools or test resources only when they belong to the finished task.
8. Automated runs use OpenForge in `scripts/openforge/` (Bun/TypeScript, embedded OpenCode SDK) as described in [the harness guide](docs/implementation/harness.md). Bootstrap each worktree with `make openforge-install`; checks do not install dependencies or rewrite the lockfile. Its independent reviewer-agent verdict plus passing scoped checks, bound to the exact candidate and review context, is the review approval in step 4, and its merge step performs steps 6-7 for the task it merged; batch validation runs outside the per-task publication lane. Autonomous workers never push, never touch another task's worktree, and park the task for a human on any decision they cannot make from the specifications.
