# Git Providers

Kipper supports deploying applications from Git repositories. The Git provider is detected automatically from the repository URL.

## Supported providers

| Provider | Detection | Auth method |
|---|---|---|
| GitHub | `github.com` in URL | Personal Access Token |
| GitLab (cloud) | `gitlab.com` in URL | Deploy Token |
| GitLab (self-hosted) | Everything else | Deploy Token |

## Authentication

### GitHub: Personal Access Token

```bash
kip app deploy --name api \
  --git https://github.com/acme/api \
  --git-token github_pat_xxx \
  --port 3000
```

### GitLab: Deploy Token

```bash
kip app deploy --name api \
  --git https://gitlab.com/acme/api \
  --git-token gldt-xxx \
  --port 3000
```

## Other providers

GitHub and GitLab are the only providers Kipper currently ships with. The internal `GitProvider` interface is provider-agnostic, so adding Gitea, Forgejo, Bitbucket, or another self-hosted system is a contained addition. Contributions welcome.
