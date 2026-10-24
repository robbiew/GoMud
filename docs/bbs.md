# bbsmud BBS listener

This fork accepts the [bbsmud door client](https://github.com/robbiew/bbsmud) on a dedicated TLS TCP listener. The engine owns the world and character state; the door binary only relays the BBS terminal. The listener is disabled by default and does not change normal Telnet, WebSocket, or SSH authentication.

## Configuration

Use Go 1.24 or newer and the normal `make build` command. Put these flat keys in your config override file (`CONFIG_PATH`, or the normal world `config-overrides.yaml`). Restart the server after changing BBS settings, certificates, or credentials.

```yaml
Network.BBS.Host: "0.0.0.0"
Network.BBS.Port: 4445
Network.BBS.CertificateFile: "/etc/gomud/bbs-cert.pem"
Network.BBS.KeyFile: "/etc/gomud/bbs-key.pem"
Network.BBS.CredentialsFile: "/etc/gomud/bbs-credentials.json"
Network.BBS.HandshakeSeconds: 10
Network.BBS.MaxConnections: 50
Network.BBS.MaxConnectionsPerBBS: 10
Network.BBS.MaxSessionMinutes: 60
Network.BBS.AttemptsPerMinute: 30
```

Certificate and key must form a valid TLS pair. Clients verify both the certificate chain and hostname; use a publicly trusted certificate or give sysops the private CA certificate. Only TLS 1.3 is accepted. Invalid enabled configuration, unreadable credentials, or a failed bind prevents startup. Port `0` disables this listener.

Store credentials in a separate owner-only regular file (`chmod 600`), outside your repository and world files:

```json
{
  "example-bbs": ["REPLACE-WITH-A-RANDOM-SECRET-OF-AT-LEAST-32-BYTES"]
}
```

Each BBS ID has one to four tokens, each containing 32..1024 bytes. Generate tokens with a cryptographically secure generator, for example `openssl rand -hex 32`. Never use the example string. To rotate a token, temporarily retain both the old and new tokens, restart, update sysops, then remove the old token and restart again. Tokens are checked only for their assigned BBS ID and compared as constant-time hashes. They are not exposed through the config API or passed to game callbacks.

`MaxConnections` includes pending TLS/handshake connections. `MaxConnectionsPerBBS` applies after authentication. `AttemptsPerMinute` limits accepted connections per peer IP (including successful attempts); the tracking table is bounded. BBS sessions have an independent connection pool, so they do not consume the existing Telnet limit. Limit login traffic further at your network boundary as needed for your installation.

## Sysop side

Run the bbsmud binary in the current node's `door32.sys` directory with the caller connected to stdin/stdout:

```sh
/bbs/doors/doorclient \
  -server mud.example.org:4445 \
  -bbs-id example-bbs \
  -token-file /bbs/secrets/bbsmud-token
```

For private trust roots add `-ca-file /bbs/secrets/bbs-ca.pem`. The configured BBS ID must match the credentials file exactly. The engine clamps `time_left` to `MaxSessionMinutes`; zero or invalid allowances are rejected. No plaintext fallback is available.

## Identity and gameplay

Identity is the exact pair `(BBS ID, UTF-8 alias)`, including case and whitespace. Alias changes or case changes create distinct identities; sysops should keep aliases stable. Nodes are retained as session metadata and do not identify characters.

A deterministic 32-character internal username indexes the account in the existing user index. Its YAML record also stores the full BBS ID and alias, and both must match on lookup. The server never attaches a BBS alias to an existing local account. A new account receives the normal `RoleUser` role with no permissions, a safe display name derived from its alias plus a stable suffix, and the standard world character defaults. Reserved or invalid display aliases fall back to a safe BBS name. The original alias remains unchanged in the identity record. Worlds with a username maximum below 32 or a regex excluding these internal names must adjust that policy before enabling the listener.

BBS accounts cannot authenticate through any password path, including Telnet, SSH and web login. An explicit external-only marker and `PasswordMatches` guard enforce this even if a password is set later. Account creation and password prompts are bypassed; normal in-world tutorials and world scripts still apply.

Aliases from other BBSes remain separate even when equal. Duplicate live sessions for one identity are rejected. User YAML remains compatible with existing local accounts, which have no `bbs` field. The existing character index tracks safe display names; no second identity database is needed.

## Wire protocol and lifecycle

`internal/bbsprotocol` is the v1 codec copied from bbsmud, with matching tests. It uses a four-byte big-endian length and one UTF-8 JSON object, bounded to 8192 bytes. Handshakes include version, BBS ID/token, alias, node number, BBS minutes, and terminal dimensions. Read exactly one frame so coalesced terminal bytes are preserved. Authentication and attachment finish before a positive framed acknowledgment; no splash, probes, music, Telnet IAC negotiation, or plugin output precedes it. Rejections are generic framed packets followed by close.

After acknowledgment, both directions are unframed terminal bytes. The engine initializes display dimensions from the handshake and uses its existing ANSI, signal, cleanser, echo and history handlers. A small stream reader preserves logical keys across fragmented UTF-8/ANSI sequences and separates pasted commands and CR/LF correctly. BBS output normalizes line endings once and suppresses Telnet protocol writes. Terminal writes have a ten-second deadline.

A per-session ticker checks an absolute expiry on a monotonic clock. Input also checks the deadline, so a queued key cannot extend an expired session. Account attachment, commands, spawn, and forced despawn are serialized by the world worker. Expiry, EOF and client idle hangup share one idempotent teardown: run normal logout hooks, announce the room departure, save final state, and close the descriptor. BBS sessions never enter the link-dead grace period or wait for meditation/logout rounds. Save failures are logged explicitly.

Copyover requests are queued onto the world worker. BBS sessions are saved and disconnected before copyover; their TLS descriptors and user sessions are explicitly excluded from raw TCP restoration. A failed copyover resumes acceptance with a fresh BBS listener. Sysops can invoke the door again after a restart. There is no transparent TLS reconnect or daily-play quota in v1: repeated door invocations supply a new BBS allowance. Enforce daily budgets independently if your game requires them.

## Validation

```sh
make validate
make test
```

Focused tests cover invalid/oversized frames, TLS/handshake shutdown, token binding/rotation, authentication rate limits, per-BBS caps, persisted identity, duplicate sessions, password denial, local-account collisions, failed provisioning, acknowledgment order, fragmented keys, backspace/CRLF command boundaries, expiry, exactly one room announcement, saved mutable state, and BBS exclusion from TCP copyover.

A reproducible smoke test runs the real server and door binaries with an isolated copy of the default world. Supply a temporary certificate valid for `localhost`:

```sh
python3 scripts/bbs-smoke.py \
  --server-binary /path/to/go-mud-server \
  --client-binary /path/to/doorclient \
  --cert-file /path/to/localhost-cert.pem \
  --key-file /path/to/localhost-key.pem
```

It checks TLS authentication rejection, normal Telnet startup, actual door initialization/game output, the real one-minute expiry, status `0`, saved identity, and reconnect to the same character. This takes roughly 65 seconds and leaves the checked-in world untouched. The smoke test has passed with Linux ARM64 engine/client binaries in Docker. A production BBS node and a full live copyover with simultaneous Telnet/WebSocket/SSH users have not been exercised.
