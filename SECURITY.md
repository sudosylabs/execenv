# execenv Security Policy

execenv occupies isolated execution grants on one host. Security reports are
taken seriously, especially when they affect isolation, grant occupancy, the
remote protocol, host credentials, or the handling of tree bodies and PTY
octets.

## Supported Versions

Fixes are developed for the latest commit on `main` and for the most recent
tagged release.

| Version | Supported |
|---------|-----------|
| Latest tagged release | Yes |
| Latest `main` | Yes |
| Older tags and unofficial builds | No |

Before reporting an issue, check whether it is reproducible on the latest
tagged release or `main` when it is safe to do so.

## Reporting a Vulnerability

Do not disclose a suspected vulnerability, proof of concept, host token, TLS
private key, tree body, PTY transcript, or catalog hash in a public issue or
discussion.

To make a private report:

1. Prefer GitHub's private vulnerability reporting for
   [sudosylabs/execenv](https://github.com/sudosylabs/execenv/security/advisories/new)
   when that form is available.
2. Otherwise contact a maintainer privately using the contact information on
   their GitHub profile.
3. If no private contact method is available, open a public issue titled
   **Security contact request**. Include no vulnerability details. A maintainer
   will arrange a private channel for the report.

Code of Conduct incidents should instead follow the private reporting process
in [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

### What to Include

Provide as much of the following information as you safely can:

- A concise description of the vulnerability and its potential impact
- The affected commit, tag, operating system, and whether the in-memory
  adapter or a production host was involved
- Reproduction steps or a minimal proof of concept using synthetic data
- Required access, privileges, or network position
- Relevant logs or screenshots with tokens, keys, file bodies, PTY octets,
  hashes, and personal data redacted
- Any suggested mitigation or fix
- Whether the issue has been disclosed to anyone else

Do not attach real workspace files, live host tokens, or TLS private keys.
Generate isolated test fixtures where possible.

## Security-Sensitive Areas

Reports are especially useful when they involve:

- Escaping a grant into the host, other grants, or the caller's network
- Occupying a grant without a valid token, or with a mismatched process stamp
- Selecting the in-memory adapter where production TLS must refuse it
- Serving grants when the isolation device is missing or unusable
- Bypassing catalog hash verification, or fetching images at Ensure time
- Forging, leaking, or logging host tokens, TLS keys, tree bodies, or PTY
  octets
- Path traversal, symlink escape, or unbounded tree writes in the projection
- Guest traffic reaching destinations outside the host allowlist, or adding
  destinations from the grant caller
- Protocol drift between `remote.New` and `remote.Serve`, including
  unauthenticated methods
- Privilege escalation through Host configuration, systemd unit, or operator
  install paths

## Coordinated Disclosure

After receiving a report, maintainers will aim to:

1. Confirm receipt and establish a private communication channel.
2. Reproduce and assess the issue, including affected versions and adapters.
3. Develop and verify a fix without weakening existing fail-closed behavior.
4. Coordinate the release and public disclosure with the reporter.
5. Credit the reporter if they want public acknowledgement.

Response and remediation times depend on severity and complexity. Please allow
maintainers a reasonable opportunity to investigate and release a fix before
publishing technical details.

## Research Guidelines

When investigating execenv:

- Use machines, tokens, and catalog disks that you own or have permission to
  test.
- Do not probe hosts you do not operate.
- Minimize access to other people's data and stop testing if you encounter
  data that does not belong to you.
- Avoid service disruption, data destruction, and testing that affects other
  operators or callers.
- Keep vulnerability details confidential until disclosure is coordinated.
- Follow applicable laws and the project [Code of Conduct](CODE_OF_CONDUCT.md).

Thank you for helping keep execenv and the people who run it safe.
