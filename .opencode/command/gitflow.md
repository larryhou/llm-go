---
description: Create and push a release tag to trigger the GitHub Actions release workflow.
---

You are helping the user publish a new version of this Go module.

The GitHub Actions workflow at `.github/workflows/release.yml` fires on any
tag matching `v*.*.*` and builds binaries for linux/amd64, linux/arm64,
darwin/amd64, darwin/arm64, and windows/amd64, then creates a GitHub release
with archives attached.

## Steps

1. **Determine the next version.**

   Run `git tag --list 'v*' | sort -V | tail -1` to find the latest tag.
   If the user provided a version in `$ARGUMENTS`, use that (ensure it has a
   `v` prefix, e.g. `v1.2.3`). Otherwise, propose the next patch version
   (e.g. `v0.1.0` → `v0.1.1`) and confirm with the user before proceeding.

2. **Check the working tree is clean.**

   Run `git status --short`. If there are uncommitted changes, stop and tell
   the user to commit or stash them first.

3. **Confirm the current branch is `main` (or `master`).**

   Run `git branch --show-current`. Warn the user if they are on a different
   branch; ask whether to proceed anyway.

4. **Show a summary of commits since the last tag.**

   Run `git log <last-tag>..HEAD --oneline` (or `git log --oneline -20` if
   there is no prior tag). Show this to the user so they can confirm the
   release content.

5. **Create and push the tag.**

   ```
   git tag <version>
   git push origin <version>
   ```

6. **Report the result.**

   Tell the user the tag has been pushed and that the GitHub Actions release
   workflow will now run. Provide the link:
   `https://github.com/larryhou/llm-go/actions`

## Arguments

`$ARGUMENTS` — optional version string, e.g. `v1.2.0`. If omitted, the
command proposes the next patch version automatically.
