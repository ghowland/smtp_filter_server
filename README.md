# smtp_filter

A minimal SMTP server that admits mail only from a configured set of senders,
and performs a configured action on each admitted message.

This is not a general purpose mail server. It has no mailboxes, no user
accounts, no authentication, no retrieval protocols, and no open relay. It
accepts a small set of known senders, and for each accepted message it runs a
local program, sends an HTTP request, or relays the message to a downstream
mail server.

The process performs no disk write operations. Message data exists only in
memory and does not survive a restart.

## Contents

- [How it works](#how-it-works)
- [Build](#build)
- [Run](#run)
- [Configuration](#configuration)
- [Dispositions](#dispositions)
- [Reply codes](#reply-codes)
- [Logs](#logs)
- [Memory use](#memory-use)
- [Deployment](#deployment)
- [Limitations](#limitations)

## How it works

A message passes two gates before the server accepts it.

**Gate 1, at `MAIL FROM`.** The peer IP address and the reverse-path are both
known at this point. Four rules are evaluated in a fixed order, and evaluation
stops at the first rule that matches.

| Rule | Test | DNS used |
|---|---|---|
| A | The peer IP address falls inside a configured network. | None |
| B | Forward-confirmed reverse DNS identifies the peer as a known provider, and the sender domain is listed for that provider. | PTR, A, AAAA, SPF |
| C | The reverse-path matches the whitelist by address or by domain. | SPF |
| D | Nothing matched. The sender is rejected. | None |

Rules B and C evaluate SPF by default. Each entry can disable that check with
`require_spf`.

**Gate 2, at `RCPT TO`.** The forward-path is not a mailbox. It is a key into
the route table. The route selects the action. If the address has no route,
the server rejects the recipient. There is no catch-all route, so an attempt
to enumerate addresses produces nothing.

After both gates pass, the server reads the message body into memory, performs
the action, and replies.

**The reply after the final dot is always `250`.** No failure of any
downstream system is reported to the sending host. Rejection is permitted
before the message is accepted, and never after. This means the log is the
only record of what happens to an accepted message. See [Logs](#logs).

## Build

Requires Go 1.22 or later.

```sh
./build.sh
```

The script initialises the module on first run, resolves the SPF dependency,
vets, and writes the `smtpfilter` binary. Run it again after any source
change.

## Run

Validate a configuration file without starting the server:

```sh
./smtpfilter -config config.json -check
```

Start the server:

```sh
./smtpfilter -config config.json
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `/etc/smtpfilter/config.json` | Path to the configuration file. |
| `-check` | off | Validate the configuration and exit. |
| `-debug` | off | Log at debug level. |

Signals:

| Signal | Effect |
|---|---|
| `SIGHUP` | Reload the configuration. The listener set and the retry parameters do not change. A defective file is rejected and the running configuration is kept. |
| `SIGINT`, `SIGTERM` | Stop the listeners, attempt one final delivery of every queued message for up to 30 seconds, then exit. |

Test a delivery with `swaks`:

```sh
swaks --server 127.0.0.1:25 \
      --from alerts@vendor.com \
      --to hook@filter.example.com
```

## Configuration

All configuration is one JSON file. Unknown fields are rejected. Every fault
in the file is reported in one run.

```json
{
  "hostname": "filter.example.com",

  "listeners": [
    { "addr": ":25",  "tls": "starttls" },
    { "addr": ":465", "tls": "implicit" }
  ],

  "tls": {
    "cert": "/etc/smtpfilter/cert.pem",
    "key":  "/etc/smtpfilter/key.pem"
  },

  "limits": {
    "max_message_bytes": 10485760,
    "max_connections": 64,
    "command_timeout_sec": 60,
    "session_timeout_sec": 300
  },

  "dns": {
    "timeout_sec": 5,
    "server": "1.1.1.1:53"
  },

  "retry": {
    "enabled": true,
    "interval_sec": 60,
    "expire_sec": 3600,
    "max_entries": 500,
    "max_bytes": 268435456
  },

  "cidr_whitelist": [
    { "cidr": "10.0.0.0/8" },
    { "cidr": "203.0.113.44/32", "domains": ["vendor.example"] }
  ],

  "providers": [
    {
      "name": "google",
      "ptr_suffixes": ["google.com", "googlemail.com"],
      "domains": ["gmail.com", "googlemail.com"],
      "require_spf": true
    }
  ],

  "whitelist": [
    { "match": "address", "value": "alerts@vendor.com", "require_spf": true },
    { "match": "domain",  "value": "partner.example",   "require_spf": true },
    { "match": "domain",  "value": "legacy.example",    "require_spf": false }
  ],

  "routes": [
    {
      "recipient": "hook@filter.example.com",
      "match": "address",
      "type": "webhook",
      "url": "https://internal.example/api/mail",
      "secret": "change-this",
      "timeout_sec": 20
    },
    {
      "recipient": "run@filter.example.com",
      "match": "address",
      "type": "command",
      "path": "/usr/local/bin/handle",
      "args": ["--from-smtp"],
      "temp_fail_exit_code": 75,
      "timeout_sec": 30
    },
    {
      "recipient": "fwd@filter.example.com",
      "match": "address",
      "type": "forward",
      "host": "127.0.0.1",
      "port": 2525,
      "timeout_sec": 30
    }
  ]
}
```

### hostname

The name the server presents in its banner and in the `EHLO` response.

### listeners

One entry per bound address. All listeners run the same session handler.

| `tls` value | Behaviour |
|---|---|
| `plain` | No encryption. `STARTTLS` is not advertised. |
| `starttls` | The connection begins unencrypted. `STARTTLS` is advertised. |
| `implicit` | The connection is encrypted from the first byte. |

Port 25 with `starttls` is the usual choice for delivery from remote mail
servers. Port 465 with `implicit` is the usual choice where encryption is
required from the start.

### tls

Paths to the server certificate and the private key. Both are required when
any listener uses `starttls` or `implicit`. Client certificates are not
requested and not verified.

### limits

| Field | Meaning |
|---|---|
| `max_message_bytes` | Largest accepted body. Advertised through the `SIZE` extension and enforced during the read. |
| `max_connections` | Largest number of concurrent sessions. Further connections are closed without a reply. |
| `command_timeout_sec` | Permitted interval between individual commands. |
| `session_timeout_sec` | Total permitted lifetime of one connection. |

### dns

| Field | Meaning |
|---|---|
| `timeout_sec` | Deadline applied to every DNS query. |
| `server` | Resolver used by the SPF library, in `ip:port` form. Leave empty to keep the library default of `8.8.8.8:53`. |

No DNS result is cached. A sender that opens several connections in a short
interval causes one set of queries per connection.

### retry

Messages whose first delivery attempt fails temporarily are held in memory and
retried on a fixed interval.

| Field | Meaning |
|---|---|
| `enabled` | Set to `false` to discard on the first temporary failure. |
| `interval_sec` | Period between retry passes. |
| `expire_sec` | Age at which a held message is discarded. Must be greater than `interval_sec`. |
| `max_entries` | Largest number of held messages. |
| `max_bytes` | Largest total size of held message bodies. |

When a limit refuses a message, the message is discarded, because the sending
host already received `250`. The queue does not survive a restart.

### cidr_whitelist

Admits a peer by address alone. No DNS query is performed and SPF is not
evaluated.

| Field | Meaning |
|---|---|
| `cidr` | Network in CIDR notation. |
| `domains` | Optional. When present, the sender domain must appear in this list. When absent, any sender domain is accepted from this network. |

### providers

Admits a large mail provider whose individual sender addresses cannot be
listed in advance.

| Field | Meaning |
|---|---|
| `name` | Label used in the log. |
| `ptr_suffixes` | The confirmed reverse DNS name of the peer must equal one of these, or be a subdomain of one. |
| `domains` | Sender domains this provider is permitted to present. |
| `require_spf` | Default `true`. |

The peer name is established by forward-confirmed reverse DNS. A PTR query
returns a hostname, and a forward query on that hostname must return the
original peer address. This shows that the operator of the address block and
the operator of the domain name agree.

MX records are not used. An MX record identifies the hosts that receive mail
for a domain. There is no requirement that a domain sends mail from those
hosts, and large providers commonly do not.

### whitelist

Admits a peer by the reverse-path it presents.

| Field | Meaning |
|---|---|
| `match` | `address` for a complete address, `domain` for a domain and all its subdomains. |
| `value` | The address or domain. An `address` entry must contain an at sign. |
| `require_spf` | Default `true`. Set to `false` for a sender with a defective SPF record. |

### routes

Maps a forward-path to an action.

| Field | Applies to | Meaning |
|---|---|---|
| `recipient` | all | The address, or the domain when `match` is `domain`. |
| `match` | all | `address` or `domain`. Default `address`. An `address` entry is selected before a `domain` entry. |
| `type` | all | `command`, `webhook`, or `forward`. |
| `timeout_sec` | all | Deadline for one delivery attempt. |
| `path` | command | Program to run. |
| `args` | command | Fixed arguments. |
| `temp_fail_exit_code` | command | Exit code that means a temporary failure. |
| `url` | webhook | Destination URL. |
| `secret` | webhook | Key used to sign the request body. |
| `host`, `port` | forward | Downstream mail server. |

Two routes may not specify the same recipient with the same match type.

## Dispositions

### command

The program is started directly. A shell is never used, so no part of the
envelope can be interpreted as a shell construction.

The message body is written to the standard input of the program. The envelope
is supplied in the environment:

| Variable | Content |
|---|---|
| `SMTPFILTER_FROM` | The reverse-path. |
| `SMTPFILTER_TO` | The forward-path. |
| `SMTPFILTER_PEER` | The peer IP address. |
| `SMTPFILTER_SIZE` | The body size in octets. |

The standard output and the standard error are captured, bounded at 8 KiB
each, and written to the log.

Exit code zero means success. The value in `temp_fail_exit_code` means a
temporary failure. Any other value means a permanent failure. A deadline
expiry is a temporary failure.

### webhook

The body is posted to the configured URL with content type `message/rfc822`.
Redirects are not followed. The URL comes from configuration only and is never
derived from message content.

| Header | Content |
|---|---|
| `X-Smtpfilter-From` | The reverse-path. |
| `X-Smtpfilter-To` | The forward-path. |
| `X-Smtpfilter-Peer` | The peer IP address. |
| `X-Smtpfilter-Signature` | `sha256=` followed by the hex HMAC of the body, keyed with `secret`. |

A 2xx status means success. A 5xx status or 429 means a temporary failure. Any
other 4xx status means a permanent failure.

Verify the signature at the receiving end. Example in Python:

```python
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
if not hmac.compare_digest(expected, request.headers["X-Smtpfilter-Signature"]):
    reject()
```

### forward

The message is relayed to the configured host with the original envelope
unchanged.

The target is expected to be a mail server under the same administrative
control, such as a local Postfix instance. Sender Rewriting Scheme is not
applied, SPF alignment at the target is not considered, and outbound
reputation is not managed. Do not point this at a third-party mail server.

A 4xx reply or a connection fault is a temporary failure. A 5xx reply is a
permanent failure.

## Reply codes

| Stage | Condition | Reply |
|---|---|---|
| `MAIL FROM` | No rule matched | `550`, close |
| `MAIL FROM` | SPF fail, softfail, neutral, none, or permerror | `550`, close |
| `MAIL FROM` | SPF temperror, or a DNS deadline | `451`, close |
| `RCPT TO` | No route for the recipient | `550`, close |
| `RCPT TO` | Second recipient in one transaction | `452` |
| Final dot | Any outcome | `250` |

Only one recipient per message is accepted, because the recipient selects the
action, and two recipients would select two actions with no defined ordering.

An SPF temporary error returns `451` and not `550`. A DNS fault is not
evidence of forgery, and a permanent rejection would discard valid mail during
a network problem.

## Logs

The log is written to standard output in JSON. The sending host is told
nothing after the message is accepted, so the log is the only view of what
happens to a message.

**Every entry at error level is a message that was accepted and then lost.**
Set an alert on all of them.

| Level | Event | Meaning |
|---|---|---|
| error | `body exceeded size limit` | The sender exceeded `max_message_bytes` after the envelope was admitted. |
| error | `permanent failure` | The action reported a permanent fault on the first attempt. |
| error | `queue entry limit reached` | The queue held `max_entries`. |
| error | `queue byte limit reached` | The queue held `max_bytes`. |
| error | `queue entry expired` | The message was retried until `expire_sec` and never delivered. |
| error | `permanent failure on retry` | The action reported a permanent fault during a retry. |
| error | `queue not drained before shutdown` | The 30 second drain limit passed with messages still held. |
| warn | `sender rejected` | Rule D, or an SPF result other than pass. Carries the rule and the SPF result. |
| warn | `sender deferred` | An SPF temporary error or a DNS deadline. |
| warn | `recipient rejected` | No route matched the forward-path. |
| warn | `message queued` | The first attempt failed temporarily. |
| info | `sender admitted` | Carries the rule, the SPF result, and the confirmed reverse DNS name. |
| info | `message delivered` | |

A `sender rejected` or `recipient rejected` entry is the only symptom of a
fault in the whitelist or the route table, because the sending host receives
an ordinary rejection and reports nothing back to you. Read these after every
configuration change.

## Memory use

Two products bound the memory used by message data:

```
sessions = max_message_bytes x max_connections
queue    = retry.max_bytes
```

The sample configuration gives 640 MiB of session buffers and 256 MiB of
queue, plus a fixed process overhead. Reduce `max_connections` or
`max_message_bytes` if that exceeds the host.

## Deployment

Port 25 needs either a capability on the binary or a socket supplied by the
service supervisor.

```sh
sudo setcap cap_net_bind_service=+ep /usr/local/bin/smtpfilter
```

The process performs no write operations, so it can run on a read-only
filesystem. It needs read access to the configuration file, the certificate,
and the key only.

```ini
[Unit]
Description=SMTP filter server
After=network.target

[Service]
ExecStart=/usr/local/bin/smtpfilter -config /etc/smtpfilter/config.json
User=smtpfilter
AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadOnlyPaths=/etc/smtpfilter
NoNewPrivileges=yes
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Remove `NoNewPrivileges` if a command route needs to change identity. A
restart discards the queue, so drain it or accept the loss.

## Logs

```
{"time":"2026-08-13T10:28:23.459121268+07:00","level":"INFO","msg":"started","hostname":"filter.example.com","listeners":2,"routes":3,"retry":true}
{"time":"2026-08-13T10:28:23.460233372+07:00","level":"INFO","msg":"listening","addr":":25","tls":"starttls"}
{"time":"2026-08-13T10:28:23.46033189+07:00","level":"INFO","msg":"listening","addr":":465","tls":"implicit"}
{"time":"2026-08-13T10:30:14.897499071+07:00","level":"WARN","msg":"sender rejected","peer":"127.0.0.1","tls":false,"from":"a@test","rule":"default","spf":"","reason":"sender not whitelisted"}
```

## Limitations

These are design decisions, not defects.

- **An accepted message can be lost silently.** The reply after the dot is
  always `250`, so the sending host is never told. The queue reduces the
  frequency but does not remove the possibility. The log is the only record.
- **The queue is in memory only.** A restart or a crash discards it.
- **One recipient per message.**
- **No DNS cache.** Every session performs its own queries.
- **No authentication.** `AUTH` is not implemented. Do not use this for mail
  submission from clients.
- **No DKIM verification.** DKIM could only be checked after the body is read,
  which is past the point where the server can still refuse a message, so it
  would give a discard rather than a rejection.
- **A stale provider list rejects valid mail silently** from the sender's point
  of view. Watch the `sender rejected` entries.
- **The retry interval is fixed.** There is no exponential backoff.
```

Two notes on things that are true but did not fit the README.

The `-debug` flag currently only lowers the log level. Nothing in the code logs at debug level yet, so it has no visible effect until you add debug lines.

The forward disposition presents `EHLO [address]` using the local address of the outbound connection. If your Postfix checks the HELO name, configure it to accept an address literal from localhost, or tell me and I will add a configurable name to the route.
