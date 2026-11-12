# 0012. Release integrity

- Status: Accepted
- Date: 2026-11-12

## Context

retrace ships as prebuilt binaries ([ADR 0001](0001-go-single-binary.md)): a tag `vX.Y.Z` makes the `release` workflow run GoReleaser, which publishes one archive per target with the binary, `LICENSE` and `README.md`, and a `checksums.txt` with their SHA-256 sums (CONTRIBUTING.md, *Branches and releases*). Users run the binary next to their API keys and point their agents at it, so they need to know that an archive is the one this repository's workflow built.

The checksums protect against a corrupted download, not against a tampered one: whoever can replace an archive in a release can replace `checksums.txt` too. The security review before `v0.1.0` (#100, #134) had to decide what else a release carries. It also found that the archives miss third-party notices: the binary links the Go standard library and modules under the BSD-3-Clause and MIT licenses (for example `modernc.org/sqlite` and `github.com/mattn/go-isatty`), and both licenses require their notices to travel with binary redistributions.

## Decision

1. **Checksums stay.** Every release has `checksums.txt` with the SHA-256 of each archive.
2. **The checksums are attested.** The `release` job runs `actions/attest-build-provenance` over `dist/checksums.txt` (its `subject-checksums` input), so each archive listed there gets a signed SLSA build provenance attestation that ties it to this repository, the workflow and the commit. Users verify an archive with `gh attestation verify <archive> --repo pmlucero/retrace`. The `id-token: write` and `attestations: write` permissions are granted to the release job only, next to its `contents: write`. The workflow keeps `contents: read` at the top level.
3. **Releases are immutable.** Immutable releases are enabled in the repository settings, so once a release is published its tag cannot move and its assets cannot be replaced or added. A broken release is fixed with a new patch version. It is a setting the maintainer turns on by hand, so CONTRIBUTING.md (*Branches and releases*) lists it among the release prerequisites.
4. **The archives carry third-party notices.** Each archive includes a notices file, next to `LICENSE` and `README.md`, with the license text of the Go standard library and of every module linked into the binary. Producing and checking it is part of the implementation (#134) and adds no runtime dependency.
5. **Not in `v0.1.0`:**
   - *An SBOM.* A Go binary already embeds its module list with versions and checksums (`go version -m retrace`), which is what an SBOM of this binary would say.
   - *cosign signatures.* They would sign the same artifacts the attestation already covers, with a second tool and a second verification path for users.

   Both are revisited if users ask for them, for example to feed a scanner that needs an SPDX or CycloneDX file.

## Alternatives considered

### Checksums only

It is what releases have today and needs no work. But checksums published next to the archives say nothing about who built them, so they do not help against a tampered release.

### Signing with cosign (keyless) instead of attestations

It gives a similar guarantee through Sigstore. But users would need `cosign` installed, while `gh attestation verify` comes with the GitHub CLI most of them already have. The attestation is also stored by GitHub outside the release, so whoever replaces a release asset cannot replace its attestation with it.

### An SBOM per archive (Syft through GoReleaser)

It is a common supply-chain artifact. For a single static Go binary with ten modules it repeats what `go version -m` reports, and it adds a tool to the release build. It can be added later without changing anything else.

### Attesting each archive separately instead of `checksums.txt`

It gives the same result: `subject-checksums` makes each archive listed in `checksums.txt` a subject of the attestation. Using the checksums file keeps the step independent of the list of targets.

## Consequences

### Positive

- Users can check that an archive was built by this repository's release workflow from a given commit, with a tool they likely have.
- A published release cannot be changed afterwards, by mistake or by an attacker with write access to releases.
- The archives meet the notice requirements of the linked modules' licenses.

### Negative

- The release job gets two more permissions, including an OIDC token. They are scoped to that job, which runs only on tag pushes.
- Immutable releases cannot be repaired in place: a release with a wrong asset needs a new version.
- The notices file must be kept in step with the module graph whenever a dependency changes.

### Risks and mitigations

- *GoReleaser publishes the release before every asset is uploaded, and immutability then refuses the rest.* Mitigation: the implementation makes GoReleaser upload to a draft release and publish it last, and the first release candidate tag checks the whole flow.
- *The notices file falls out of date.* Mitigation: CI checks it against the modules linked into the built binary.
- *Users do not verify.* The README and the release notes show the `gh attestation verify` command. Checksums remain for users without the GitHub CLI.
