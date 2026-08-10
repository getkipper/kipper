# Security Policy

## Reporting a vulnerability

Kipper provisions and manages real production clusters, and the CLI connects to servers over SSH, so we take security reports seriously. Please report vulnerabilities privately so we can fix them before they are public.

Two ways to report, in order of preference:

1. **GitHub private advisory.** Open the repository's **Security** tab and choose **Report a vulnerability**. This keeps the report private and ties it to a fix.
2. **Email.** Send the details to security@getkipper.com.

Please do not open a public issue for a security problem, and please do not disclose it publicly until we have shipped a fix.

When you report, tell us what you found, where it lives in the code, how to reproduce it, and what an attacker could do with it. A minimal proof of concept helps a lot.

## What to expect

- We aim to acknowledge a report within three working days.
- We will confirm whether we can reproduce it and agree a rough timeline with you.
- We will keep you updated as we work on the fix, and we will credit you in the release notes once it ships, unless you would rather stay anonymous.

## Scope

Areas we especially want to hear about:

- Authentication and the Dex login flow, and session or token handling.
- Authorisation and tenant isolation, for example a project member reaching another project's data, or a non-admin gaining admin or cluster-wide access.
- Secret handling: service credentials, git tokens, and anything that leaks them.
- The install and upgrade path, host hardening, and the SSH bootstrap.
- The default components Kipper deploys and how it configures them.

Usually out of scope:

- Vulnerabilities in upstream projects Kipper installs, such as k3s, Traefik, cert-manager, Longhorn, and Dex. Report those upstream. We are happy to help coordinate.
- Attacks that need existing cluster-admin rights or root on the host.
- A user's own misconfiguration, such as an exposed kubeconfig or a weak password they chose.

## Supported versions

Kipper is pre-release. Until the first tagged release, only the `main` branch is supported. Fixes land on `main` and ship in the next release.
