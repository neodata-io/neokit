# Security policy

neokit is the base other services are built on, so a flaw here is a flaw in
each of them.

## Reporting a vulnerability

Report it privately through GitHub: the repository's **Security** tab →
**Report a vulnerability**. Please do not open a public issue for it.

Include what you can: the package, the version or commit, and the smallest
request or program that shows the problem.

## Supported versions

Only the latest `v0.x.0` tag receives fixes. neokit is pre-1.0 and has no patch
releases; a fix ships as the next minor.

## What is in scope

- Anything that lets a caller bypass a guard, a rate limit, the cross-origin
  write check or the outbound address guard.
- A secret, token or upstream response body reaching a log, an error message or
  a response.
- Data loss in the SQLite, session, token or backup code.
