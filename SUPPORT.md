# Getting help

For setup and usage, start with the [online documentation](https://debark.dev/docs):
[quick start](https://debark.dev/docs/get-started/quickstart),
[supported systems](https://debark.dev/docs/get-started/supported-systems), and
[troubleshooting](https://debark.dev/docs/operate/troubleshooting).
Desktop users can also use the [desktop guide](https://debark.dev/docs/get-started/desktop).
The [repository documentation](docs/README.md) includes guides for offline
reading and for the source revision in your checkout.
Check [known limitations](docs/status.md) for validation gaps.

## Questions, bugs, and ideas

Open an issue using the appropriate form:

- [Ask a question](https://github.com/inferops/debark/issues/new?template=question.yml).
- [Report a bug](https://github.com/inferops/debark/issues/new?template=bug_report.yml).
- [Request a feature](https://github.com/inferops/debark/issues/new?template=feature_request.yml).

Search existing issues first. Support is provided by project contributors on a
best-effort basis; there is no guaranteed response time.

## Include enough context to reproduce

Useful details are:

- Output of `debark version --json`; for the desktop app, include its version
  from **About** and the CLI version.
- Builder OS and architecture, target release and architecture, and whether
  you used a captured snapshot or baseline.
- The command or UI steps, selected backend, expected result, and actual output.
- The smallest package list or synthetic fixture that reproduces the problem.
- Exit code, relevant logs, and container runtime details if applicable.
- For website or documentation problems, the affected page URL and the text
  or link that needs correcting.

Review everything before posting. Snapshots, bundle metadata, URLs, and logs can
contain internal infrastructure details or credentials. `--redact` removes
selected fields but is not a complete anonymizer. Never post a private signing key.

## Security and conduct reports

Report vulnerabilities privately through [SECURITY.md](SECURITY.md).
Use the reporting route in [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for
conduct concerns. Do not include either kind of sensitive report in a public
support issue.

To work on a fix or improve the docs, see [CONTRIBUTING.md](CONTRIBUTING.md).
