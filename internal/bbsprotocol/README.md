# bbsmud protocol v1

The codec and tests are copied from `robbiew/bbsmud` commit `d5e9c10` (`internal/bbsprotocol`). Keep wire changes coordinated with the client. An independent GoMud module cannot import another module's `internal` package.

The authenticated transport, limits and engine lifecycle are documented in [docs/bbs.md](../../docs/bbs.md). The server requires stronger token lengths than the wire-format minimum; that is a server credential policy, not a different wire protocol.
