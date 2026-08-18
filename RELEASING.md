# Releasing

Engram ships as prebuilt binaries attached to a GitHub Release. Releases are
fully automated by [GoReleaser](https://goreleaser.com) via GitHub Actions —
you never build or upload artifacts by hand. Pushing a `v*` tag is the entire
release trigger.

## What a release produces

Tagging `vX.Y.Z` runs `.github/workflows/release.yml`, which invokes GoReleaser
(`.goreleaser.yaml`) to:

- Cross-compile a single static binary (`CGO_ENABLED=0`, pure-Go SQLite) for
  **6 targets**: `linux`, `darwin`, `windows` × `amd64`, `arm64`.
- Ad-hoc codesign the macOS binaries (best-effort, only if `codesign` is present).
- Package each as `engram_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows).
- Emit `checksums.txt` (SHA-256 of every archive).
- Generate a changelog from Conventional Commit subjects since the previous tag
  (excluding `docs:`, `test:`, `ci:`).
- Create the GitHub Release and attach all archives + checksums.

The version string is baked in via `-ldflags -X main.version={{.Version}}`, so
`engram --version` on a released binary prints the tag.

There is **no Homebrew tap** and no other package-manager publishing — only the
GitHub Release archives. (Homebrew was intentionally dropped; do not re-add a
`brews:` block to `.goreleaser.yaml`.)

## Prerequisites

- No secrets to configure. The workflow authenticates with the automatic
  `GITHUB_TOKEN`; the repo's default Actions permissions (`contents: write`)
  are sufficient to create the Release.
- Public repo → GitHub Actions minutes are free/unlimited.

## Cutting a release

1. **Land everything on `main`.** The release builds from the tagged commit, so
   make sure `main` has the code and the correct `.goreleaser.yaml` you want to
   ship. Verify locally:

   ```bash
   git checkout main && git pull --ff-only
   go build ./cmd/engram && go test ./...
   ```

2. **Pick the version.** Follow SemVer against the previous tag
   (`git tag -l | sort -V | tail -1`):
   - `fix:` only → patch (`vX.Y.(Z+1)`)
   - any `feat:` → minor (`vX.(Y+1).0`)
   - a `!` / `BREAKING CHANGE:` commit → major (`v(X+1).0.0`)

3. **Bump the plugin manifests to the same version, and commit that on `main`
   before tagging.** The plugin version is not cosmetic: `claude plugin update`
   decides whether to refresh by comparing version *strings*, so a tag that
   ships unchanged manifests leaves every installed user on the previous
   plugin content — it reports `already at the latest version` and exits 0,
   which no error handling can catch. Three files, always together:

   ```bash
   # .claude-plugin/marketplace.json
   # plugin/claude-code/.claude-plugin/plugin.json
   # plugin/codex/.codex-plugin/plugin.json
   ./tools/check-plugin-version.sh X.Y.Z   # verifies all three agree and match
   ```

   The marketplace clone tracks `main`, not the tag, so this commit is what
   actually reaches users. `release.yml` re-runs this check against the tag and
   fails the release before publishing if they disagree.

4. **(Optional) Dry-run GoReleaser locally** to catch config errors without
   publishing anything:

   ```bash
   goreleaser release --snapshot --clean --skip=publish
   # or just validate the config:
   goreleaser check
   ```

5. **Tag and push.** The tag on `main` is what fires the workflow:

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

6. **Watch the workflow.**

   ```bash
   gh run watch $(gh run list --workflow=release.yml -L1 --json databaseId --jq '.[0].databaseId')
   ```

   When it succeeds, the Release appears at
   `https://github.com/relytcloud/engram/releases/tag/vX.Y.Z` with all 6
   archives + `checksums.txt`.

## Installing a released binary

Preferred — the installer script (auto-discovers the latest release, verifies
sha256, installs the binary AND wires the Claude Code plugin):

```bash
curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh | bash
# pin a specific release instead:
curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh | bash -s -- --version X.Y.Z
```

Manual (pinned-version) alternative:

```bash
# Example: linux/amd64
curl -fsSL -o engram.tar.gz \
  https://github.com/relytcloud/engram/releases/download/vX.Y.Z/engram_X.Y.Z_linux_amd64.tar.gz
tar -xzf engram.tar.gz engram
mkdir -p ~/.local/bin
install -m 0755 engram ~/.local/bin/engram   # ensure ~/.local/bin is on PATH
engram --version
```

macOS binaries are ad-hoc signed, not notarized. On first run Gatekeeper may
require `xattr -d com.apple.quarantine ./engram` (or right-click → Open).

## If a release fails

- **Fix forward.** Push the fix to `main`, then either delete and re-push the
  same tag or cut the next patch tag:

  ```bash
  git push --delete origin vX.Y.Z && git tag -d vX.Y.Z   # remove bad tag
  # …commit fix on main…
  git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z
  ```

- GoReleaser runs with `--clean`, so re-running a tag rebuilds from scratch; it
  will refuse to overwrite an existing non-draft Release for the same tag unless
  that Release is removed first.
