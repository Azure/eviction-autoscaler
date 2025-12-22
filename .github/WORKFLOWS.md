# GitHub Actions Workflows

This document describes the GitHub Actions workflows in this repository and their configuration requirements.

## Workflows

### build-publish-mcr.yaml
Builds and publishes container images to Azure Container Registry (MCR).

**Trigger:** 
- Push of version tags matching pattern `[0-9]+.[0-9]+.[0-9]+` (e.g., `1.0.0`, `0.1.17`)
- Manual workflow dispatch

**Requirements:**
- Self-hosted runner with label `1ES.Pool=1es-aks-eviction-autoscaler-pool-ubuntu`
- Azure managed identity for ACR authentication

### manual-release-tag.yaml
Creates production release tags manually through workflow dispatch.

**Trigger:** Manual workflow dispatch

**Required Secret:**
- `PAT_TOKEN`: Personal Access Token with the following scopes:
  - `repo` - Full control of private repositories
  - `workflow` - Update GitHub Action workflows
  
  This token is required to trigger the `build-publish-mcr` workflow when a tag is pushed. The default `GITHUB_TOKEN` does not trigger other workflows as a security feature.

**Creating a PAT:**
1. Go to GitHub Settings → Developer settings → Personal access tokens → Tokens (classic)
2. Generate new token with `repo` and `workflow` scopes
3. Add the token as a repository secret named `PAT_TOKEN`

**Fallback:** If `PAT_TOKEN` is not configured, the workflow will use `GITHUB_TOKEN` but will NOT trigger the build workflow automatically.

### auto-tag.yaml
Automatically creates development tags when code is merged to main branch.

**Trigger:** Push to `main` branch

**Required Secret:**
- `PAT_TOKEN`: Same as above. Development tags include the commit SHA (e.g., `0.1.0-abc1234`) and currently don't trigger the build workflow, but the token ensures consistency if the trigger pattern changes in the future.

### ci.yml
Runs continuous integration tests.

**Trigger:** Pull requests and pushes

### scorecard.yml
Runs OpenSSF Scorecard security checks.

**Trigger:** Schedule and manual dispatch

## Troubleshooting

### Build workflow not triggered after tag push

**Problem:** The `build-publish-mcr` workflow doesn't run after `manual-release-tag` creates and pushes a new tag.

**Solution:** 
1. Verify that `PAT_TOKEN` secret is configured in repository settings
2. Ensure the PAT has both `repo` and `workflow` scopes
3. Check that the tag matches the pattern `[0-9]+.[0-9]+.[0-9]+` (no prefix like `v`)
4. Verify the workflow file `.github/workflows/build-publish-mcr.yaml` exists and is valid

**Why this happens:** 
GitHub Actions has a security feature that prevents workflows authenticated with `GITHUB_TOKEN` from triggering other workflows. This prevents recursive or cascading workflow executions. To trigger downstream workflows, you must use a Personal Access Token (PAT) or GitHub App token.
