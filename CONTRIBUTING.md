# Contributing

Issues and pull requests are welcome.

1. Read [docs/development.md](docs/development.md) for the layout and how to
   run the server against a cluster.
2. Keep changes focused, with tests: the manager is tested against fake
   clientsets, the API through `httptest`.
3. Run `make test lint js` before opening a pull request.
4. Write plain ASCII: no em or en dashes, no curly quotes.
5. Never commit secrets, kubeconfigs or `.env` files. Use `dev/` (git-ignored)
   for local credentials.

Security issues: see [SECURITY.md](SECURITY.md), not public issues.
