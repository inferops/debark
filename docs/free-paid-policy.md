# Free/paid policy

This is project policy, and it does not change
without the process described in [GOVERNANCE.md](../GOVERNANCE.md).

> debark's community edition will always include everything needed to
> securely prepare, understand, transfer, verify and install a bundle for one
> or a small number of targets — including every signature, verification,
> provenance and SBOM capability. We will never cap package counts, bundle
> sizes or target counts, and we will never charge for knowing whether an
> artifact is authentic. Commercial editions add capabilities whose value
> comes from coordinating people, policy, fleet state, enterprise keys or
> contractual assurance, and run only on the connected builder side. The core
> licence is Apache-2.0 and will not change.

## What this means in practice

The rule that generates the table below:

> Anything required to securely prepare, understand, transfer, verify and
> install one bundle for one or a small set of targets is community
> functionality. Anything whose principal value arises from coordinating
> people, policy, fleet state, enterprise keys or contractual assurance can be
> commercial.

Two things are explicitly refused, permanently: quantity caps (for example,
"free for three targets") and gating anything security-relevant. Both would
read as manufacturing insecurity in order to sell relief from it — see
[TRADEMARK.md](../TRADEMARK.md) and [GOVERNANCE.md](../GOVERNANCE.md) for how
this boundary is protected as policy, not a roadmap suggestion.

## The capability table

| Capability | Community (free, forever) | Commercial (builder side only) |
|---|---|---|
| Snapshot; online-side resolution; vendor `.deb` closure; exact lock; repository build | **Free** | — |
| Repository and manifest signing (local key, GPG, Sigstore) | **Free** | HSM/KMS-backed centralised key custody |
| Verification before install | **Free** | Central enforcement and reporting |
| Provenance record, basic CycloneDX/SPDX SBOM, one-bundle attestation | **Free** | Aggregation, retention, dashboards, GRC/SIEM export |
| `--json`, exit codes, non-interactive automation | **Free** | Orchestrated jobs / API scheduling |
| Incremental store, update mode, delta bundles | **Free** | Cross-site content store, fleet cache optimisation |
| Local policy file and hooks; CVE/licence visibility when data is available | **Free** | Centrally administered policy, exceptions, approvals, org-wide gating |
| Target registry / fleet history | — | **Paid** |
| RBAC, SSO, SCIM | — | **Paid** |
| Multi-person approval / dual control for media crossing the gap | — | **Paid** |
| Audit-retention policies | Local evidence free | **Paid centrally** |
| Certified / attested / **FIPS 140-3** builds (`GOFIPS140` toolchain, separate pipeline) | Reproducible community releases | **Paid assurance option** |
| LTS builds, SLA, named support, indemnification, procurement paperwork | — | **Paid** |

## Status

No commercial edition exists yet. Everything in this repository today is
community functionality under Apache-2.0; see [LICENSE](../LICENSE) and
[TRADEMARK.md](../TRADEMARK.md). If and when a commercial edition ships, it
will be a separate codebase built on the same open interfaces, never a crippled build of the community binary, and never a licence
check compiled into the community binary — see the non-negotiable rule in
[CONTRIBUTING.md](../CONTRIBUTING.md).

The table above states the free/paid *boundary*, which is policy and does not
move. It is not an inventory of what is built, and two rows name capabilities
that do not exist yet. **Sigstore** signing is a reserved `signer_kind`
value, not a signer: `sign.SignerFor` builds an ed25519 file signer, a
`gpg:` signer or a `plugin:` signer, and Sigstore would arrive as a plugin.
The SBOM writer emits **CycloneDX only**, with no SPDX output
(`core/sbom`). Both are free when they exist, which is what the table is
promising; neither exists today. See the README's
[Status and known gaps](../README.md#status-and-known-gaps) for the current
gap list.
