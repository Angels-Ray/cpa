#!/usr/bin/env bash
# Push current branch and create a fork release tag for GitHub Actions (release.yaml).
# Default remote: origin (Angels-Ray/cpa).
# Tags are calendar-based (not upstream-synced): vYYYY.MM.DD, then vYYYY.MM.DD.1, .2, ...
#
# Usage:
#   ./scripts/cpa-fork-release.sh
#   ./scripts/cpa-fork-release.sh --tag v2026.07.20.1
#   ./scripts/cpa-fork-release.sh --utc --dry-run
#   ./scripts/cpa-fork-release.sh --no-push-branch --wait
#
# Env:
#   REMOTE=origin
#   USE_UTC=0            # 1 = date in UTC
#   WAIT_TIMEOUT=1800    # seconds when --wait is set

set -euo pipefail

REMOTE="${REMOTE:-origin}"
USE_UTC="${USE_UTC:-0}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-1800}"
DRY_RUN=0
PUSH_BRANCH=1
WAIT_RELEASE=0
EXPLICIT_TAG=""
FORCE_TAG=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_ok() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_err() { echo -e "${RED}[ERR]${NC} $*" >&2; }
log_step() { echo -e "${CYAN}[STEP]${NC} $*"; }

usage() {
  cat <<'EOF'
cpa-fork-release.sh — push branch + date tag to trigger fork GitHub release CI

Usage:
  ./scripts/cpa-fork-release.sh [options]

Tag scheme (default, independent of upstream):
  first release today : vYYYY.MM.DD
  same day again      : vYYYY.MM.DD.1, vYYYY.MM.DD.2, ...
  example             : v2026.07.20  →  v2026.07.20.1

Options:
  --tag <name>       Use this tag exactly (e.g. v2026.07.20.1)
  --utc              Use UTC date instead of local timezone
  --remote <name>    Git remote (default: origin)
  --no-push-branch   Do not push current branch (only push tag)
  --force-tag        Move existing local/remote tag to current HEAD
  --wait             After push, poll until release assets appear
  --dry-run          Print actions only
  -h, --help         Show help

Env:
  REMOTE, USE_UTC, WAIT_TIMEOUT, GH_TOKEN (optional for --wait on private repo)

Examples:
  ./scripts/cpa-fork-release.sh
  ./scripts/cpa-fork-release.sh --wait
  ./scripts/cpa-fork-release.sh --tag v2026.07.20.1 --wait
  USE_UTC=1 ./scripts/cpa-fork-release.sh --dry-run
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      EXPLICIT_TAG="${2:-}"
      shift 2
      ;;
    --utc)
      USE_UTC=1
      shift
      ;;
    --remote)
      REMOTE="${2:-}"
      shift 2
      ;;
    --no-push-branch)
      PUSH_BRANCH=0
      shift
      ;;
    --force-tag)
      FORCE_TAG=1
      shift
      ;;
    --wait)
      WAIT_RELEASE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      log_err "unknown option: $1"
      usage
      exit 1
      ;;
  esac
done

require_git_repo() {
  if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    log_err "not inside a git repository"
    exit 1
  fi
}

run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log_info "dry-run: $*"
    return 0
  fi
  "$@"
}

parse_remote_repo() {
  local url
  url="$(git remote get-url "$REMOTE" 2>/dev/null || true)"
  if [[ -z "$url" ]]; then
    log_err "remote not found: $REMOTE"
    exit 1
  fi

  # git@github.com:Owner/Repo.git  or  https://github.com/Owner/Repo.git
  local owner_repo=""
  if [[ "$url" =~ github\.com[:/]([^/]+)/([^/.]+)(\.git)?$ ]]; then
    owner_repo="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  else
    log_warn "could not parse GitHub owner/repo from remote URL: $url"
    owner_repo=""
  fi
  echo "$owner_repo"
}

# Return 0 if tag exists locally or on remote.
tag_exists() {
  local tag="$1"
  if git rev-parse "$tag" >/dev/null 2>&1; then
    return 0
  fi
  if git ls-remote --tags "$REMOTE" "refs/tags/${tag}" 2>/dev/null | grep -q .; then
    return 0
  fi
  return 1
}

# Calendar tags: vYYYY.MM.DD, then vYYYY.MM.DD.1, .2, ... (independent of upstream).
next_tag() {
  if [[ -n "$EXPLICIT_TAG" ]]; then
    echo "$EXPLICIT_TAG"
    return
  fi

  local day
  if [[ "$USE_UTC" == "1" ]]; then
    day="$(date -u +%Y.%m.%d)"
  else
    day="$(date +%Y.%m.%d)"
  fi

  local base="v${day}"
  if ! tag_exists "$base"; then
    echo "$base"
    return
  fi

  # base exists → find max N for vYYYY.MM.DD.N
  local max_n=0
  local t n
  while IFS= read -r t; do
    [[ -z "$t" ]] && continue
    if [[ "$t" =~ ^v${day}\.([0-9]+)$ ]]; then
      n="${BASH_REMATCH[1]}"
      if (( n > max_n )); then
        max_n=$n
      fi
    fi
  done < <(git tag -l "v${day}.*" 2>/dev/null || true)

  while IFS= read -r t; do
    t="${t##*/}"
    t="${t%^{}}"
    [[ -z "$t" ]] && continue
    if [[ "$t" =~ ^v${day}\.([0-9]+)$ ]]; then
      n="${BASH_REMATCH[1]}"
      if (( n > max_n )); then
        max_n=$n
      fi
    fi
  done < <(git ls-remote --tags "$REMOTE" "refs/tags/v${day}.*" 2>/dev/null | awk '{print $2}' || true)

  echo "v${day}.$((max_n + 1))"
}

wait_for_assets() {
  local owner_repo="$1"
  local tag="$2"
  local version="${tag#v}"
  local deadline=$((SECONDS + WAIT_TIMEOUT))
  local api="https://api.github.com/repos/${owner_repo}/releases/tags/${tag}"
  local auth_args=()
  if [[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
    auth_args=(-H "Authorization: Bearer ${GH_TOKEN:-$GITHUB_TOKEN}")
  fi

  log_step "waiting for release assets on ${owner_repo} @ ${tag} (timeout ${WAIT_TIMEOUT}s)"
  while (( SECONDS < deadline )); do
    local body
    body="$(curl -fsSL "${auth_args[@]}" -H "Accept: application/vnd.github+json" "$api" 2>/dev/null || true)"
    if [[ -n "$body" ]] && echo "$body" | grep -q "CLIProxyAPI_${version}_linux_"; then
      log_ok "release assets are ready"
      if command -v python3 >/dev/null 2>&1; then
        echo "$body" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("assets:"); [print(" -",a["name"]) for a in d.get("assets",[])]'
      else
        echo "$body" | grep -o '"name": *"[^"]*"' | head -20
      fi
      return 0
    fi
    log_info "assets not ready yet, sleep 15s..."
    sleep 15
  done
  log_err "timed out waiting for release assets"
  log_info "check Actions: https://github.com/${owner_repo}/actions"
  return 1
}

main() {
  require_git_repo

  local branch
  branch="$(git rev-parse --abbrev-ref HEAD)"
  local head
  head="$(git rev-parse --short HEAD)"
  local owner_repo
  owner_repo="$(parse_remote_repo)"

  if [[ -n "$(git status --porcelain 2>/dev/null || true)" ]]; then
    log_warn "working tree is dirty; uncommitted changes will NOT be in the release"
    git status -sb || true
  fi

  local tag
  tag="$(next_tag)"

  log_step "branch=${branch} head=${head} remote=${REMOTE}"
  log_step "tag=${tag}"
  if [[ -n "$owner_repo" ]]; then
    log_info "repo=https://github.com/${owner_repo}"
    log_info "actions=https://github.com/${owner_repo}/actions"
    log_info "release=https://github.com/${owner_repo}/releases/tag/${tag}"
  fi

  if git rev-parse "$tag" >/dev/null 2>&1; then
    if [[ "$FORCE_TAG" -eq 1 ]]; then
      log_warn "tag exists locally, moving with --force-tag"
      run git tag -f "$tag" HEAD
    else
      log_err "tag already exists: $tag (use --force-tag or --tag <new>)"
      exit 1
    fi
  else
    run git tag -a "$tag" -m "fork release ${tag}"
  fi

  if [[ "$PUSH_BRANCH" -eq 1 ]]; then
    log_step "push branch ${branch} → ${REMOTE}"
    run git push "$REMOTE" "HEAD:refs/heads/${branch}"
  fi

  log_step "push tag ${tag} → ${REMOTE}"
  if [[ "$FORCE_TAG" -eq 1 ]]; then
    run git push -f "$REMOTE" "refs/tags/${tag}"
  else
    run git push "$REMOTE" "refs/tags/${tag}"
  fi

  log_ok "tag pushed; release workflow should start (trigger: push tags)"
  if [[ -n "$owner_repo" ]]; then
    log_info "watch: https://github.com/${owner_repo}/actions"
  fi

  if [[ "$WAIT_RELEASE" -eq 1 ]]; then
    if [[ -z "$owner_repo" ]]; then
      log_err "--wait needs a parseable GitHub remote"
      exit 1
    fi
    if [[ "$DRY_RUN" -eq 1 ]]; then
      log_info "dry-run: skip wait"
      exit 0
    fi
    wait_for_assets "$owner_repo" "$tag"
  else
    log_info "server install after CI finishes:"
    if [[ -n "$owner_repo" ]]; then
      echo "  curl -fsSL https://raw.githubusercontent.com/${owner_repo}/main/scripts/cpa-fork-installer.sh | bash"
      echo "  # or pin branch/path if raw is not on main yet"
    fi
    echo "  REPO_OWNER=... REPO_NAME=... bash scripts/cpa-fork-installer.sh"
  fi
}

main "$@"
