# Real target fixtures

Captured target state from actual machines, in the **Bash prototype's**
`snapshot-target.sh` format (`target-state/` containing `dpkg-status`, `apt/`,
`keyrings/`, `arch`, `codename`, …). They are not `debark.snapshot/v1`
archives; they are the raw material a real capture reads, and they exist so
the integration matrix and the apt adapter can be exercised
against genuine, messy input rather than hand-written fixtures.

| File | Source | Captured | Installed packages |
|---|---|---|---|
| `ubuntu-2404-state.tar.gz` | WSL2 Ubuntu 24.04.3 LTS, apt 2.8.3, amd64 | 2026-09-03 | 588 |
| `debian-12-state.tar.gz` | `debian:bookworm-slim` container, apt 2.6.1, amd64 | 2026-09-03 | (base image set) |

To refresh one:

```bash
# from a target machine, or a container of the target release
./snapshot-target.sh /tmp/state.tar.gz
```

These contain no secrets: no machine id is captured by the prototype script,
and the sources are the distributions' own. They are checked in deliberately —
a fixture you cannot reproduce is a fixture you cannot trust — and each row
above records exactly where it came from and when, because apt archives move.
