# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems.

Use GitHub's private reporting instead: open the **Security** tab of this repository and choose **Report a vulnerability**. Include the affected version or commit, steps to reproduce, and the impact you observed.

We will acknowledge your report and keep you updated as we investigate and fix the issue.

## Deployment notes

- Set a strong, unique `JWT_SECRET`. The built-in default is for local development only.
- Change the default database credentials.
- Provide secrets through environment variables or a secret manager. Never commit `.env` files.
- Build with Go 1.26.11 or newer.
