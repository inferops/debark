# debark documentation

[Website](https://debark.dev) · [Online documentation](https://debark.dev/docs)

The website has an [online quick start](https://debark.dev/docs/get-started/quickstart)
and [CLI reference](https://debark.dev/docs/reference/cli). For documentation
in this checkout, start with the [project overview](../README.md), then choose
a guide below. These repository docs describe the source in this checkout
and can be read offline. For a released version, browse the same files at
its tag and read the [changelog](../CHANGELOG.md).

## Using debark

| Guide | What it covers |
| --- | --- |
| [Quick start](quickstart.md) | Capture a target, build a signed bundle, transfer it, and install |
| [CLI guide](cli.md) | Package inputs, updates, signing, configuration, JSON, and exit codes |
| [Platforms and prerequisites](platforms.md) | Target releases, baseline IDs, containers, and Windows setup |
| [Troubleshooting](troubleshooting.md) | Common failures and recovery steps |
| [FAQ](faq.md) | Snapshots, baseline assumptions, offline behavior, and project scope |
| [Desktop app](../gui/README.md) | Install or build the package browser and bundle builder |
| [Desktop user guide](../gui/docs/user-guide.md) | Target, Packages, Bundle, and system checks |
| [Status and known limitations](status.md) | Validation coverage and open gaps |
| [Support](../SUPPORT.md) | Ask a question or report a problem |

## Trust and file formats

| Reference | What it covers |
| --- | --- |
| [Security model](security-model.md) | Archive trust, operator signatures, and installation boundaries |
| [Threat model](threat-model.md) | Adversaries, defenses, and residual risks |
| [Independent verification](security/verification-guide.md) | Verify a bundle without relying on the debark verifier |
| [Formats](formats.md) | Snapshots, locks, manifests, bundles, configuration, and plugins |
| [JSON Schemas](../api/schema/) | Versioned machine-readable contracts |
| [Signer plugin example](../examples/signer-plugin/README.md) | Implement the stdio JSON signing protocol |
| [Security policy](../SECURITY.md) | Report vulnerabilities privately |

## Contributing and design

- [Contributing](../CONTRIBUTING.md): fork and branch setup, local checks,
  signed-off commits, pull requests, and review/merge requirements.
- [Engineering contract](dev/contract-brief.md): package boundaries and public contracts.
- [Architecture decisions](adr/README.md): the reasons behind the design.
- [Desktop development](../gui/README.md#development): UI contracts and checks.
- [Governance](../GOVERNANCE.md), [Code of Conduct](../CODE_OF_CONDUCT.md),
  [free/paid policy](free-paid-policy.md), and [trademark policy](../TRADEMARK.md).

## Validation records

These are scoped records of particular tests and investigations. Dates, host
details, and revisions describe those runs, rather than ongoing support guarantees.
Some records reference revisions from development history that predates the public
repository.

- [Experiments](experiments/README.md), including [baseline fidelity](experiments/E8-base-fidelity.md).
- [Integration matrix](../hack/matrix/README.md) and [result format](../hack/matrix/results/README.md).
- [End-to-end test fixtures](../test/e2e/README.md).
- [Security review](security/review-2026-09.md), [findings](security/review-findings.md),
  and [dependency review](security/dependency-review.md).
- [Pre-publication review and fixes](security/pre-publication-2026-09-08.md).
- Desktop [packaging](../gui/docs/packaging.md), [Windows](../gui/docs/windows.md),
  [accessibility](../gui/docs/accessibility.md), [performance](../gui/docs/performance.md),
  and [security review](../gui/docs/security-review.md).
