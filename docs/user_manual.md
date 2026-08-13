# SMTP Filter — User Manual

**Executable:** `smtp_filter`
**Version:** 1.0

---

## Contents

**Part 1 — Getting started**
1. [What this program does](#1-what-this-program-does)
2. [Requirements](#2-requirements)
3. [Install](#3-install)
4. [First run](#4-first-run)

**Part 2 — Configuration**
5. [The configuration file](#5-the-configuration-file)
6. [hostname](#6-hostname)
7. [listeners](#7-listeners)
8. [tls](#8-tls)
9. [limits](#9-limits)
10. [dns](#10-dns)
11. [retry](#11-retry)
12. [cidr_whitelist](#12-cidr_whitelist)
13. [providers](#13-providers)
14. [whitelist](#14-whitelist)
15. [routes](#15-routes)

**Part 3 — The three actions**
16. [Command action](#16-command-action)
17. [Webhook action](#17-webhook-action)
18. [Forward action](#18-forward-action)

**Part 4 — Running as a service**
19. [Create the service account](#19-create-the-service-account)
20. [Install the files](#20-install-the-files)
21. [The systemd unit](#21-the-systemd-unit)
22. [Start, stop, and reload](#22-start-stop-and-reload)
23. [Certificate renewal](#23-certificate-renewal)

**Part 5 — Operation**
24. [Reading the log](#24-reading-the-log)
25. [Monitoring](#25-monitoring)
26. [Changing the configuration safely](#26-changing-the-configuration-safely)
27. [Testing a delivery](#27-testing-a-delivery)

**Part 6 — Reference**
28. [Command line reference](#28-command-line-reference)
29. [Signal reference](#29-signal-reference)
30. [Reply code reference](#30-reply-code-reference)
31. [Log event reference](#31-log-event-reference)
32. [Troubleshooting](#32-troubleshooting)
33. [Worked examples](#33-worked-examples)

---

# Part 1 — Getting started

## 1. What this program does

`smtp_filter` receives electronic mail, accepts it only from senders you have listed, and performs one action on each accepted message. The action is one of three things: it runs a local program, it sends an HTTP request, or it passes the message to another mail server.

It is not a normal mail server. It has no mailboxes, no user accounts, and no way for a mail client to collect mail from it. Use it where a machine sends mail to another machine, and where the message must cause something to happen rather than be stored.

### The two questions it asks

For every message, the program asks two questions before it accepts anything.

**Question 1, asked when the sender identifies itself.** May this computer send mail here, claiming to be this sender? Your `cidr_whitelist`, `providers`, and `whitelist` settings answer this. A sender that matches none of them is refused.

**Question 2, asked when the recipient is named.** Is this recipient address one I recognise? Your `routes` setting answers this. The recipient address is not a mailbox. It is the key that selects the action. An address with no route is refused.

Both questions are answered before the message text is sent, so a refused message costs almost nothing.

### One rule to understand before you start

**Once the program accepts a message, it tells the sender the message was delivered, whatever actually happens to it afterwards.**

If the program you run fails, if the web endpoint is down, if the other mail server refuses the message, the sender is still told `250 OK`. The sender will not try again, and it will not report a failure to the person who wrote the message.

The program holds failed messages in memory and retries them, which covers most temporary problems. But if a message is finally lost, **nothing tells you except the log**. This is why section 25 asks you to set up monitoring before you put the program into service.

---

## 2. Requirements

| Item | Requirement |
|---|---|
| Operating system | Linux. The systemd unit in Part 4 assumes systemd. |
| Go | Version 1.22 or later, to build. Not needed to run. |
| OpenSSL | To generate a test certificate. Not needed if you supply your own. |
| Network | Outbound DNS. Inbound TCP on the ports you configure. |
| Privilege | Root once, to install and to grant the port 25 capability. |

The program writes nothing to disk. It reads the configuration file, the certificate, and the private key. Nothing else.

---

## 3. Install

### 3.1 Build

From the source directory:

```sh
./build.sh
```

This resolves the dependencies, compiles, and writes the `smtp_filter` binary into the current directory. On the first run it also generates a self-signed `cert.pem` and `key.pem`, if they are not already present.

Rerun `./build.sh` after any source change.

### 3.2 The certificate

The certificate the build script generates is self-signed and names `localhost`. It is suitable for local testing only.

Sending mail servers do not usually verify the certificate of the server they deliver to, so a self-signed certificate often works in production as well. If you need a certificate signed by a public authority, obtain one and point the `tls` section at it. Section 23 covers renewal.

To generate the test certificate by hand:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout key.pem -out cert.pem \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```

The private key must be readable only by the account that runs the program:

```sh
chmod 600 key.pem
```

---

## 4. First run

### 4.1 Write a minimal configuration

Create `config.json` in the same directory as the binary:

```json
{
  "hostname": "localhost",
  "listeners": [
    { "addr": "127.0.0.1:2525", "tls": "starttls" }
  ],
  "tls": { "cert": "cert.pem", "key": "key.pem" },
  "limits": {
    "max_message_bytes": 10485760,
    "max_connections": 16,
    "command_timeout_sec": 60,
    "session_timeout_sec": 300
  },
  "dns": { "timeout_sec": 5, "server": "" },
  "retry": {
    "enabled": true,
    "interval_sec": 60,
    "expire_sec": 3600,
    "max_entries": 100,
    "max_bytes": 67108864
  },
  "cidr_whitelist": [
    { "cidr": "127.0.0.0/8" }
  ],
  "providers": [],
  "whitelist": [],
  "routes": [
    {
      "recipient": "test@localhost",
      "match": "address",
      "type": "command",
      "path": "/bin/cat",
      "timeout_sec": 30
    }
  ]
}
```

This configuration accepts anything from the local machine and pipes it to `/bin/cat`, whose output appears in the log. Port 2525 is used so that no special privilege is needed for this first test.

### 4.2 Check the configuration

```sh
./smtp_filter -config config.json -check
```

The program prints `configuration is valid` and exits. If there are faults, it lists all of them at once. Fix them and check again.

### 4.3 Start it

```sh
./smtp_filter -config config.json
```

You should see a startup line and one line per listener. Leave it running.

### 4.4 Send a test message

In another terminal:

```sh
swaks --server 127.0.0.1:2525 --from test@example.com --to test@localhost
```

If `swaks` is not installed, use `nc`:

```sh
printf 'EHLO test\r\nMAIL FROM:<a@b.test>\r\nRCPT TO:<test@localhost>\r\nDATA\r\nSubject: hi\r\n\r\nbody\r\n.\r\nQUIT\r\n' \
  | nc 127.0.0.1 2525
```

The log should show `sender admitted`, then `command output` containing your message, then `message delivered`.

Stop the program with `Ctrl+C`.

---

# Part 2 — Configuration

## 5. The configuration file

All configuration is one JSON file. There are no other settings.

Three rules apply to the whole file.

**Unknown fields are rejected.** A misspelled field name stops the program from starting. This is intentional. A silently ignored setting is worse than a refusal to start.

**All faults are reported at once.** You do not have to fix one fault, rerun, and find the next.

**Always check before you apply.** Run `-check` on any file you have edited, before you reload or restart the service.

Relative paths in the file resolve against the working directory of the process. In the systemd unit in Part 4, that is the installation directory.

---

## 6. hostname

```json
"hostname": "filter.example.com"
```

The name the program gives in its greeting and in its `EHLO` reply. Use the name that the sending servers resolve to reach you. It has no effect on the filtering rules.

---

## 7. listeners

```json
"listeners": [
  { "addr": ":25",  "tls": "starttls" },
  { "addr": ":465", "tls": "implicit" }
]
```

One entry for each address the program listens on. Every listener behaves identically except for encryption.

| `addr` form | Effect |
|---|---|
| `":25"` | Listen on port 25 on every address of the machine. |
| `"127.0.0.1:2525"` | Listen on port 2525 on the loopback address only. |
| `"192.0.2.10:25"` | Listen on port 25 on one specific address. |

| `tls` value | Effect |
|---|---|
| `"plain"` | No encryption. `STARTTLS` is not offered. |
| `"starttls"` | The connection starts unencrypted. The client may upgrade it with the `STARTTLS` command. |
| `"implicit"` | The connection is encrypted from the first byte. The client must start TLS immediately. |

**Which to use.** Port 25 with `starttls` is what other mail servers expect when they deliver mail to you. Port 465 with `implicit` is what some devices and applications expect. Configure both if you need both. `plain` is for a listener bound to the loopback address only.

Ports below 1024 need extra privilege. Section 21 shows how to grant it.

---

## 8. tls

```json
"tls": {
  "cert": "cert.pem",
  "key":  "key.pem"
}
```

Paths to the certificate and the private key. Both are required if any listener uses `starttls` or `implicit`.

The certificate is presented to connecting clients. Client certificates are not requested and not checked. The minimum protocol version is TLS 1.2.

Keep the private key readable only by the service account.

---

## 9. limits

```json
"limits": {
  "max_message_bytes": 10485760,
  "max_connections": 64,
  "command_timeout_sec": 60,
  "session_timeout_sec": 300
}
```

| Field | Meaning |
|---|---|
| `max_message_bytes` | The largest message the program accepts. It is announced to senders, and enforced while the message is read. |
| `max_connections` | The largest number of connections handled at one time. Beyond this, new connections are closed with no reply. |
| `command_timeout_sec` | How long the program waits for the next command before it closes the connection. |
| `session_timeout_sec` | The total time any one connection may last. |

### Setting the memory budget

The program holds every message in memory. Two products decide how much memory it can use:

```
message buffers  =  max_message_bytes  x  max_connections
retry queue      =  retry.max_bytes
```

The values above give 640 MiB of buffers plus whatever the queue holds. If that is too much for your machine, reduce `max_connections` first. Reducing `max_message_bytes` also refuses larger messages, which may not be what you want.

### What happens at the size limit

If a sender exceeds `max_message_bytes` after its envelope was accepted, the message is discarded and the sender is told `250`. This is logged at error level. There is no way to refuse it, because the acceptance already happened.

---

## 10. dns

```json
"dns": {
  "timeout_sec": 5,
  "server": "1.1.1.1:53"
}
```

| Field | Meaning |
|---|---|
| `timeout_sec` | The time limit on every DNS query. A query that exceeds it is treated as a temporary failure. |
| `server` | The resolver used for SPF checks, in `address:port` form. Leave empty to use the built-in default of `8.8.8.8:53`. |

No DNS result is cached. Every connection performs its own queries. Keep `timeout_sec` short, because a slow resolver holds SMTP connections open.

The `server` field affects SPF checks only. The reverse DNS queries use the system resolver from `/etc/resolv.conf`.

---

## 11. retry

```json
"retry": {
  "enabled": true,
  "interval_sec": 60,
  "expire_sec": 3600,
  "max_entries": 500,
  "max_bytes": 268435456
}
```

When the first delivery attempt fails for a temporary reason, the message is held in memory and tried again.

| Field | Meaning |
|---|---|
| `enabled` | Set to `false` to discard immediately on the first temporary failure. |
| `interval_sec` | How often held messages are retried. The interval is fixed and does not increase. |
| `expire_sec` | How long a message may be held. After this it is discarded. Must be larger than `interval_sec`. |
| `max_entries` | The largest number of held messages. |
| `max_bytes` | The largest total size of held messages. |

### Three things to know

**The queue is in memory only.** A restart or a crash discards it. On a clean shutdown the program makes one final attempt on every held message, for up to 30 seconds, then gives up and logs each loss.

**A full queue discards.** When either limit is reached, the new message is dropped, because the sender has already been told the message was accepted. This is logged at error level.

**Expiry is a silent loss.** When `expire_sec` passes, the message is gone and nobody outside the log knows. Choose `expire_sec` to match how long your downstream systems can be down. One hour is short. Six hours or a day is reasonable if the memory is available.

---

## 12. cidr_whitelist

```json
"cidr_whitelist": [
  { "cidr": "10.0.0.0/8" },
  { "cidr": "203.0.113.44/32", "domains": ["vendor.example"] }
]
```

Accepts a sender based on its IP address alone. No DNS query and no SPF check.

| Field | Meaning |
|---|---|
| `cidr` | A network in CIDR notation. Use `/32` for a single IPv4 address, `/128` for a single IPv6 address. |
| `domains` | Optional. When given, the sender's domain must be in this list. When omitted, any sender address is accepted from this network. |

Use this for machines you control, where the address is sufficient proof. It is the fastest rule, because it involves no network activity.

An entry with no `domains` list accepts any sender address at all from that network. Only use it for networks entirely under your control.

---

## 13. providers

```json
"providers": [
  {
    "name": "google",
    "ptr_suffixes": ["google.com", "googlemail.com"],
    "domains": ["gmail.com", "googlemail.com"],
    "require_spf": true
  }
]
```

Accepts mail from a large provider whose individual sending addresses cannot be listed in advance.

| Field | Meaning |
|---|---|
| `name` | A label that appears in the log. |
| `ptr_suffixes` | The confirmed reverse DNS name of the sending computer must equal one of these, or be below one of them. |
| `domains` | The sender domains this provider is permitted to use. |
| `require_spf` | Whether to also check SPF. Defaults to `true`. |

### How the check works

Three separate things are established:

1. **The computer is identified** by forward-confirmed reverse DNS. A reverse lookup on the sending address returns a name. A forward lookup on that name must return the original address. Both the address block owner and the domain owner must agree before a name is accepted.
2. **The identity is authorised** by the `domains` list. A Google server may present a `gmail.com` address, and nothing else, unless you list more.
3. **The domain owner agrees** through SPF.

The suffix match is exact or on a dot boundary. A sender at `notgoogle.com` does not match the suffix `google.com`.

### A caution

If Google changes its reverse DNS naming, this rule stops matching and mail from Gmail is refused. There is no visible symptom other than `sender rejected` entries in your log, because the sender receives an ordinary refusal and tells you nothing.

---

## 14. whitelist

```json
"whitelist": [
  { "match": "address", "value": "alerts@vendor.com", "require_spf": true },
  { "match": "domain",  "value": "partner.example",   "require_spf": true },
  { "match": "domain",  "value": "legacy.example",    "require_spf": false }
]
```

Accepts a sender based on the address it presents.

| Field | Meaning |
|---|---|
| `match` | `"address"` to match one complete address. `"domain"` to match a domain and all its subdomains. |
| `value` | The address or domain. An `address` entry must contain an at sign. |
| `require_spf` | Whether to check SPF. Defaults to `true`. |

A `domain` entry for `partner.example` matches `anyone@partner.example` and `anyone@mail.partner.example`.

### About require_spf

Leave this at `true`. It is what stops another computer from claiming to be your whitelisted sender.

Set it to `false` only for a specific sender whose SPF record is missing or broken, and only after you have asked them to fix it. The flag belongs to the individual entry, so turning it off for one sender does not weaken any other.

### About SPF results

An SPF check gives one of seven results. This program treats them as follows.

| Result | Effect |
|---|---|
| `pass` | Accepted. |
| `fail`, `softfail`, `neutral`, `none`, `permerror` | Refused with `550`. |
| `temperror` | Refused with `451`, which asks the sender to try again later. |

This is stricter than a normal mail server, which usually accepts `softfail`, `neutral`, and `none`. Here your senders are known in advance, so an uncertain statement from the domain owner is of no use. If a whitelisted sender publishes no SPF record at all, either ask them to publish one or set `require_spf` to `false` for that entry.

---

## 15. routes

```json
"routes": [
  {
    "recipient": "hook@filter.example.com",
    "match": "address",
    "type": "webhook",
    "url": "https://internal.example/api/mail",
    "secret": "change-this",
    "timeout_sec": 20
  }
]
```

Each route maps a recipient address to an action.

| Field | Applies to | Meaning |
|---|---|---|
| `recipient` | all | The address, or the domain when `match` is `"domain"`. |
| `match` | all | `"address"` or `"domain"`. Defaults to `"address"`. |
| `type` | all | `"command"`, `"webhook"`, or `"forward"`. |
| `timeout_sec` | all | Time limit for one delivery attempt. |
| `path` | command | The program to run. |
| `args` | command | Fixed arguments given to the program. |
| `temp_fail_exit_code` | command | The exit code that means "try again later". |
| `url` | webhook | The destination URL. |
| `secret` | webhook | The key used to sign the request. |
| `host`, `port` | forward | The downstream mail server. |

### How a route is chosen

An `address` route is chosen before a `domain` route. Comparison ignores case.

Two routes may not use the same recipient with the same match type. The program refuses to start if they do.

There is no catch-all. Mail to any address with no route is refused with `550`.

### Choosing timeout_sec

The sending computer waits while the action runs. Keep the value short enough that the sender does not give up first. Most mail servers wait ten minutes after the final dot. A value under 60 seconds is safe. If your action needs longer, make it queue the work and return immediately.

---

# Part 3 — The three actions

## 16. Command action

Runs a local program and gives it the message.

```json
{
  "recipient": "run@filter.example.com",
  "match": "address",
  "type": "command",
  "path": "/usr/local/bin/handle",
  "args": ["--from-smtp"],
  "temp_fail_exit_code": 75,
  "timeout_sec": 30
}
```

### How the program is called

The program is started directly. **No shell is involved**, so nothing in the message or the envelope can be interpreted as a shell command. The `path` must be a full path to an executable file, not a shell line.

The message text arrives on standard input. The envelope arrives in the environment:

| Variable | Content |
|---|---|
| `SMTPFILTER_FROM` | The sender address. |
| `SMTPFILTER_TO` | The recipient address. |
| `SMTPFILTER_PEER` | The IP address of the sending computer. |
| `SMTPFILTER_SIZE` | The message size in bytes. |

Standard output and standard error are captured, limited to 8192 bytes each, and written to the log.

### What the exit code means

| Exit code | Result |
|---|---|
| `0` | Success. The message is done. |
| The value in `temp_fail_exit_code` | Temporary failure. The message is queued and retried. |
| Anything else | Permanent failure. The message is discarded and logged at error level. |
| Timeout | Temporary failure. |
| Could not start the program | Temporary failure. |

Exit code 75 is the conventional Unix value for a temporary failure, which is why the examples use it. Any value works.

### Example handler

```sh
#!/bin/sh
# /usr/local/bin/handle

target="/var/spool/incoming/$(date +%s).eml"

if ! cat > "$target"; then
    exit 75          # could not write, ask for a retry
fi

echo "stored $target from $SMTPFILTER_FROM"
exit 0
```

Make it executable with `chmod +x`. Remember that this runs as the service account, so that account needs write access to the target.

### Cautions

The program runs with the same identity and permissions as `smtp_filter`. Treat everything it receives as untrusted input, because the message content comes from outside.

Never pass `SMTPFILTER_FROM` or the message content to a shell inside your handler.

---

## 17. Webhook action

Sends the message to a web endpoint.

```json
{
  "recipient": "hook@filter.example.com",
  "match": "address",
  "type": "webhook",
  "url": "https://internal.example/api/mail",
  "secret": "a-long-random-string",
  "timeout_sec": 20
}
```

### The request

A POST with content type `message/rfc822`. The body is the complete message text. Redirects are not followed.

| Header | Content |
|---|---|
| `X-Smtpfilter-From` | The sender address. |
| `X-Smtpfilter-To` | The recipient address. |
| `X-Smtpfilter-Peer` | The IP address of the sending computer. |
| `X-Smtpfilter-Signature` | `sha256=` then the hexadecimal HMAC-SHA256 of the body, keyed with `secret`. |

### Verifying the signature

Always verify it. Without verification, anyone who can reach your endpoint can send it whatever they like.

Python:

```python
import hmac, hashlib

def verify(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

Node.js:

```js
const crypto = require("crypto");

function verify(body, header, secret) {
  const expected = "sha256=" +
    crypto.createHmac("sha256", secret).update(body).digest("hex");
  return crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(header));
}
```

Use a constant-time comparison, as both examples do. A plain string comparison leaks information about the correct value through its timing.

### What the status code means

| Status | Result |
|---|---|
| 2xx | Success. |
| 5xx | Temporary failure. Queued and retried. |
| 429 | Temporary failure. Queued and retried. |
| Any other 4xx | Permanent failure. Discarded and logged. |
| Connection fault or timeout | Temporary failure. |

Return 5xx from your endpoint if it cannot process the message right now. Return 4xx only if the message will never be processable, because that discards it.

### Choosing the secret

Use at least 32 random characters:

```sh
openssl rand -hex 32
```

The configuration file contains this secret, so restrict its permissions. Section 20 covers this.

---

## 18. Forward action

Passes the message to another mail server.

```json
{
  "recipient": "fwd@filter.example.com",
  "match": "address",
  "type": "forward",
  "host": "127.0.0.1",
  "port": 2525,
  "timeout_sec": 30
}
```

The original sender and recipient addresses are presented unchanged.

### Important restriction

**The target must be a mail server you control**, such as a Postfix instance on the same machine or the same private network.

The program applies no Sender Rewriting Scheme and manages no sending reputation. If you point it at a third-party mail server, that server sees your machine sending mail for a domain that has not authorised it, and the SPF check at the target will fail. Mail will be rejected or classified as spam.

### What the reply means

| Reply from the target | Result |
|---|---|
| 250 after the message | Success. |
| Any 4xx | Temporary failure. Queued and retried. |
| Any 5xx | Permanent failure. Discarded and logged. |
| Connection fault or timeout | Temporary failure. |

### Postfix note

The program identifies itself with `EHLO [address]`, using an address literal in square brackets. If your Postfix rejects address literals, add a rule that permits them from this machine, or allow the connection through `mynetworks`.

Example receiving configuration for a Postfix on the same host, listening on port 2525:

```
# master.cf
2525      inet  n       -       y       -       -       smtpd
  -o smtpd_client_restrictions=permit_mynetworks,reject
  -o smtpd_helo_restrictions=
```

---

# Part 4 — Running as a service

## 19. Create the service account

Use a dedicated account with no login shell and no home directory:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin smtp_filter
```

---

## 20. Install the files

```sh
sudo mkdir -p /opt/smtp_filter
sudo cp smtp_filter config.json cert.pem key.pem /opt/smtp_filter/
sudo chown -R root:smtp_filter /opt/smtp_filter
sudo chmod 750 /opt/smtp_filter
sudo chmod 640 /opt/smtp_filter/config.json
sudo chmod 640 /opt/smtp_filter/key.pem
sudo chmod 644 /opt/smtp_filter/cert.pem
sudo chmod 755 /opt/smtp_filter/smtp_filter
```

The result: the service account can read everything it needs and can change nothing. The configuration file and the private key are not world-readable, because the configuration contains webhook secrets.

### Grant the port 25 capability

Ports below 1024 need extra privilege. Grant it to the binary:

```sh
sudo setcap cap_net_bind_service=+ep /opt/smtp_filter/smtp_filter
```

**This must be repeated after every time you replace the binary.** The capability attaches to the file, not to the path.

Verify it:

```sh
getcap /opt/smtp_filter/smtp_filter
```

If you use only ports above 1024, skip this step and remove `AmbientCapabilities` from the unit below.

---

## 21. The systemd unit

Save as `/etc/systemd/system/smtp_filter.service`:

```ini
[Unit]
Description=SMTP Filter Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=smtp_filter
Group=smtp_filter
WorkingDirectory=/opt/smtp_filter
ExecStart=/opt/smtp_filter/smtp_filter -config config.json

AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
ReadOnlyPaths=/opt/smtp_filter

Restart=on-failure
RestartSec=5s

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

### Notes on this unit

`WorkingDirectory` is what makes the relative paths in `config.json` resolve. Keep it.

`ProtectSystem=strict` makes the whole filesystem read-only for this process. The program writes nothing, so this costs nothing.

`NoNewPrivileges=yes` prevents the process and anything it starts from gaining privilege. **If you use a command route whose program must change identity, for example through `sudo` or a setuid binary, remove this line.** Otherwise leave it.

`Restart=on-failure` restarts the program if it exits with an error. Note that a restart discards the retry queue.

If you use command routes, the program you run must be reachable and executable from inside these restrictions. `ProtectSystem=strict` does not block reading or executing, only writing. If your handler needs to write somewhere, add that path with `ReadWritePaths=`.

### Enable and start

```sh
sudo systemctl daemon-reload
sudo systemctl enable smtp_filter
sudo systemctl start smtp_filter
sudo systemctl status smtp_filter
```

---

## 22. Start, stop, and reload

| Task | Command |
|---|---|
| Start | `sudo systemctl start smtp_filter` |
| Stop | `sudo systemctl stop smtp_filter` |
| Restart | `sudo systemctl restart smtp_filter` |
| Reload the configuration | `sudo systemctl reload-or-restart smtp_filter` or `sudo kill -HUP $(pidof smtp_filter)` |
| Status | `sudo systemctl status smtp_filter` |
| Follow the log | `sudo journalctl -u smtp_filter -f` |

### What reload changes and what it does not

`SIGHUP` reloads the configuration file. If the new file is faulty, it is rejected and the running configuration continues unchanged. The rejection is logged.

Reload **does** apply changes to `cidr_whitelist`, `providers`, `whitelist`, `routes`, `limits`, and `dns`.

Reload **does not** apply changes to `listeners`, `tls`, or `retry`. Those are fixed when the process starts. Restart to change them, and remember that a restart discards the queue.

Connections already in progress finish with the configuration they started with.

### Stopping cleanly

On stop, the program stops accepting new connections, then tries once more to deliver every queued message, for up to 30 seconds. Anything still undelivered is lost and each loss is logged at error level.

Before a planned restart, check whether the queue is empty. Look for recent `message queued` entries with no matching `queued message delivered`.

---

## 23. Certificate renewal

The program reads the certificate once, at start. **A renewed certificate takes effect only after a restart.** A reload does not reload it.

With certbot, add a deploy hook:

```sh
sudo tee /etc/letsencrypt/renewal-hooks/deploy/smtp_filter.sh <<'EOF'
#!/bin/sh
DOMAIN=filter.example.com
install -o root -g smtp_filter -m 644 \
    /etc/letsencrypt/live/$DOMAIN/fullchain.pem /opt/smtp_filter/cert.pem
install -o root -g smtp_filter -m 640 \
    /etc/letsencrypt/live/$DOMAIN/privkey.pem /opt/smtp_filter/key.pem
systemctl restart smtp_filter
EOF
sudo chmod +x /etc/letsencrypt/renewal-hooks/deploy/smtp_filter.sh
```

Set the renewal to run at a quiet time, because the restart discards any queued messages.

---

# Part 5 — Operation

## 24. Reading the log

The log is JSON, one object per line, written to standard output. Under systemd it goes to the journal.

```sh
sudo journalctl -u smtp_filter -f
```

Format it with `jq` for readability:

```sh
sudo journalctl -u smtp_filter -f -o cat | jq .
```

Every message-related entry carries the sender, the recipient, the route, and the outcome.

### The three levels

**info** — normal activity. A sender was accepted. A message was delivered.

**warn** — something was refused, or a message is being retried. These are normal in ordinary operation. Read them after any configuration change, because a refusal is the only symptom of a mistake in your whitelist or route table.

**error** — **a message was accepted and then lost.** Every one of these needs attention.

### Useful queries

Refusals in the last hour:

```sh
sudo journalctl -u smtp_filter --since "1 hour ago" -o cat \
  | jq -r 'select(.level=="WARN") | "\(.msg) from=\(.from) rule=\(.rule)"'
```

All losses today:

```sh
sudo journalctl -u smtp_filter --since today -o cat \
  | jq -r 'select(.level=="ERROR")'
```

Count refusals by reason:

```sh
sudo journalctl -u smtp_filter --since today -o cat \
  | jq -r 'select(.msg=="sender rejected") | .rule' | sort | uniq -c | sort -rn
```

---

## 25. Monitoring

**Set this up before you put the program into service.**

The reason is in section 1. Once a message is accepted, the sender is told it was delivered. If the message is then lost, the log is the only place that records it. Nothing else will tell you, and nobody will complain, because as far as every other party is concerned the message arrived.

### The alert you must have

Alert on every log entry at error level. There are seven of them, listed in section 31, and each one means a lost message.

With journald and a simple watcher:

```sh
journalctl -u smtp_filter -f -o cat \
  | jq -r 'select(.level=="ERROR") | @json' \
  | while read -r line; do
        notify "smtp_filter: message lost: $line"
    done
```

Replace `notify` with your alerting command.

### The alert you should have

Alert when `sender rejected` or `recipient rejected` entries rise sharply. A sudden increase usually means one of these:

- A provider changed its reverse DNS naming, so a `providers` entry no longer matches.
- A whitelisted sender changed or broke its SPF record.
- Somebody removed a route or changed a recipient address.
- Your resolver is failing, producing SPF temporary errors.

The sender receives an ordinary refusal and tells you nothing. Your log is the only warning.

### Health check

The program has no health endpoint. Check that the port answers:

```sh
printf 'QUIT\r\n' | nc -w 5 127.0.0.1 25 | grep -q '^220' && echo up || echo down
```

---

## 26. Changing the configuration safely

Follow this order every time.

**Step 1. Copy the file.**

```sh
sudo cp /opt/smtp_filter/config.json /tmp/config.new.json
```

**Step 2. Edit the copy.**

**Step 3. Check it.**

```sh
/opt/smtp_filter/smtp_filter -config /tmp/config.new.json -check
```

Fix anything reported and check again until it passes.

**Step 4. Install it.**

```sh
sudo install -o root -g smtp_filter -m 640 \
    /tmp/config.new.json /opt/smtp_filter/config.json
```

**Step 5. Apply it.**

For changes to whitelists, providers, routes, limits, or DNS:

```sh
sudo kill -HUP $(pidof smtp_filter)
```

For changes to listeners, TLS, or retry:

```sh
sudo systemctl restart smtp_filter
```

**Step 6. Watch the log.**

```sh
sudo journalctl -u smtp_filter -f
```

Confirm the reload succeeded, then watch for unexpected refusals for several minutes.

### If a reload fails

The program logs `reload failed, keeping the current configuration` and continues with what it had. Nothing is disrupted. Fix the file and reload again.

---

## 27. Testing a delivery

### With swaks

```sh
swaks --server filter.example.com:25 \
      --from alerts@vendor.com \
      --to hook@filter.example.com \
      --header "Subject: test" \
      --body "test message"
```

Test the STARTTLS path:

```sh
swaks --server filter.example.com:25 --tls \
      --from alerts@vendor.com --to hook@filter.example.com
```

Test the implicit TLS port:

```sh
swaks --server filter.example.com:465 --tlsc \
      --from alerts@vendor.com --to hook@filter.example.com
```

Confirm that an unlisted sender is refused:

```sh
swaks --server filter.example.com:25 \
      --from nobody@nowhere.test --to hook@filter.example.com
```

You should see `550` after `MAIL FROM`.

Confirm that an unknown recipient is refused:

```sh
swaks --server filter.example.com:25 \
      --from alerts@vendor.com --to nothing@filter.example.com
```

You should see `550` after `RCPT TO`.

### Checking the certificate

```sh
openssl s_client -connect filter.example.com:25 -starttls smtp
openssl s_client -connect filter.example.com:465
```

---

# Part 6 — Reference

## 28. Command line reference

```
smtp_filter [-config PATH] [-check] [-debug]
```

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `config.json` | Path to the configuration file. |
| `-check` | off | Validate the configuration, print the result, and exit. |
| `-debug` | off | Log at debug level. |

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Normal exit. |
| 1 | A listener failed. |
| 2 | The configuration could not be loaded or is invalid, or the certificate could not be loaded. |

---

## 29. Signal reference

| Signal | Effect |
|---|---|
| `SIGHUP` | Reload the configuration. Listeners, TLS, and retry settings are not changed. A faulty file is rejected and the running configuration is kept. |
| `SIGINT` | Stop the listeners, drain the queue for up to 30 seconds, exit. |
| `SIGTERM` | Same as `SIGINT`. This is what systemd sends. |

---

## 30. Reply code reference

What a sender sees:

| Stage | Condition | Reply |
|---|---|---|
| Connect | Ready | `220` |
| `EHLO` | Accepted | `250` multi-line |
| `STARTTLS` | Ready to upgrade | `220` |
| `MAIL FROM` | Accepted | `250` |
| `MAIL FROM` | No rule matched | `550`, connection closes |
| `MAIL FROM` | SPF fail, softfail, neutral, none, or permerror | `550`, connection closes |
| `MAIL FROM` | SPF temporary error, or a DNS timeout | `451`, connection closes |
| `RCPT TO` | Route found | `250` |
| `RCPT TO` | No route | `550`, connection closes |
| `RCPT TO` | Second recipient | `452` |
| `DATA` | Ready for the message | `354` |
| Final dot | **Every outcome** | `250` |
| `QUIT` | | `221` |
| `AUTH`, `VRFY`, `EXPN`, `ETRN`, `HELP` | | `502` |
| Unknown command | | `500` |

Only one recipient is accepted per message, because the recipient selects the action.

---

## 31. Log event reference

### Error level — a message was lost

| Event | Meaning | What to do |
|---|---|---|
| `body exceeded size limit` | The sender sent more than `max_message_bytes` after its envelope was accepted. | Raise the limit, or tell the sender to send less. |
| `permanent failure` | The action reported a permanent fault on the first attempt. | Check the action. For a command, check the exit code and captured output. |
| `queue entry limit reached` | The queue held `max_entries` and could not take another. | Raise `max_entries`, or fix whatever is failing downstream. |
| `queue byte limit reached` | The queue held `max_bytes`. | Raise `max_bytes`, or fix whatever is failing downstream. |
| `queue entry expired` | A message was retried until `expire_sec` and never delivered. | The downstream system was down too long. Raise `expire_sec` or fix the downstream system. |
| `permanent failure on retry` | The action reported a permanent fault during a retry. | Check the action. |
| `queue not drained before shutdown` | The 30-second drain limit passed with messages still held. | Check that downstream systems are up before you restart. |

### Warning level — normal, but read them

| Event | Meaning |
|---|---|
| `sender rejected` | Rule D, or an SPF result other than pass. The entry carries the rule and the SPF result. |
| `sender deferred` | An SPF temporary error or a DNS timeout. The sender was asked to try again. |
| `recipient rejected` | No route matched. |
| `message queued` | The first attempt failed for a temporary reason. |
| `connection limit reached` | `max_connections` was reached and a connection was closed. |
| `tls handshake failed` | A client began STARTTLS but the handshake failed. |
| `reload failed` | A `SIGHUP` was received but the file was faulty. The running configuration was kept. |

### Info level

| Event | Meaning |
|---|---|
| `listening` | One listener started. |
| `started` | The process started. |
| `sender admitted` | Carries the rule, the SPF result, and the confirmed reverse DNS name. |
| `message delivered` | |
| `queued message delivered` | A retry succeeded. |
| `command output` | Captured standard output and standard error from a command route. |
| `configuration reloaded` | A `SIGHUP` succeeded. |

---

## 32. Troubleshooting

### The program will not start

**`permission denied` on port 25.** The capability is missing. Run `sudo setcap cap_net_bind_service=+ep /opt/smtp_filter/smtp_filter` and confirm with `getcap`. It must be reapplied after every binary replacement.

**`address already in use`.** Another mail server holds the port. Check with `sudo ss -tlnp | grep :25`. Stop it, or move `smtp_filter` to another port.

**Configuration faults listed at start.** Fix all of them. The list is complete.

**`load certificate:` error.** Check the paths in the `tls` section. Relative paths resolve against `WorkingDirectory`. Check that the service account can read both files.

### A sender is refused

Find the log entry:

```sh
sudo journalctl -u smtp_filter -o cat | jq -r 'select(.msg=="sender rejected")'
```

Read the `rule` and `spf` fields.

| `rule` | `spf` | Cause |
|---|---|---|
| `default` | empty | No rule matched at all. The sender is not in any whitelist. |
| `whitelist:...` | `FAIL` | The sender's domain explicitly forbids that IP address. |
| `whitelist:...` | `NONE` | The sender publishes no SPF record. |
| `whitelist:...` | `SOFTFAIL` or `NEUTRAL` | The sender's SPF record is not definite. This program requires `pass`. |
| `provider:...` | `FAIL` | The address is not authorised for the sender's domain. |

Check the SPF record yourself:

```sh
dig +short TXT vendor.com | grep spf
```

If the record is missing or wrong, ask the sender to fix it. As a last resort set `require_spf` to `false` on that one entry.

If `rule` is `default` and you expected a provider match, the reverse DNS check failed. Test it:

```sh
dig +short -x 209.85.128.1
dig +short mail-1.google.com
```

The second command must return the address you started with. If it does not, or the name does not fall under your `ptr_suffixes`, update the provider entry.

### Every sender is deferred with 451

Your resolver is failing. Every SPF check gives a temporary error.

```sh
dig @1.1.1.1 TXT gmail.com
```

Check the `dns.server` setting and the network path to it. Raise `dns.timeout_sec` if the resolver is slow.

### Messages are accepted but nothing happens

Check the log for the outcome of each message. If you see `message queued` repeatedly, the action is failing temporarily.

For a command route, look at `command output` entries for the reason.

For a webhook route, test the endpoint by hand:

```sh
curl -v -X POST -H "Content-Type: message/rfc822" \
     --data-binary "test" https://internal.example/api/mail
```

For a forward route, test the target:

```sh
swaks --server 127.0.0.1:2525 --from a@b.test --to c@d.test
```

### A command route fails with "permission denied"

The program runs as the service account. Check that the account can execute the program and can access whatever the program needs.

```sh
sudo -u smtp_filter /usr/local/bin/handle < /dev/null
```

If your handler writes somewhere, add that path to the unit with `ReadWritePaths=`, because `ProtectSystem=strict` makes everything else read-only.

### The queue keeps filling

The downstream system is failing and the retry interval is not keeping up, or messages are arriving faster than they are being delivered.

Raise `max_entries` and `max_bytes` to hold more, but the real fix is downstream. Watch for `queue entry limit reached`, which is a discard.

### Memory use is high

The formula is in section 9. Reduce `max_connections` first.

---

## 33. Worked examples

### Example A — device scanner to a local program

An office scanner on the internal network emails scanned documents. A program stores them.

```json
{
  "hostname": "scan.internal",
  "listeners": [
    { "addr": ":25", "tls": "starttls" }
  ],
  "tls": { "cert": "cert.pem", "key": "key.pem" },
  "limits": {
    "max_message_bytes": 52428800,
    "max_connections": 8,
    "command_timeout_sec": 60,
    "session_timeout_sec": 600
  },
  "dns": { "timeout_sec": 5, "server": "" },
  "retry": {
    "enabled": true,
    "interval_sec": 30,
    "expire_sec": 7200,
    "max_entries": 50,
    "max_bytes": 536870912
  },
  "cidr_whitelist": [
    { "cidr": "192.168.10.50/32" }
  ],
  "providers": [],
  "whitelist": [],
  "routes": [
    {
      "recipient": "scans@scan.internal",
      "match": "address",
      "type": "command",
      "path": "/usr/local/bin/store_scan",
      "timeout_sec": 45
    }
  ]
}
```

Points to note. The scanner is admitted by address alone, so no DNS or SPF is involved, which matters because the device may present any sender address it likes. `max_message_bytes` is 50 MiB for scanned documents, and `max_connections` is only 8, which keeps the memory ceiling at 400 MiB. `session_timeout_sec` is long, because a scanner on a slow link may take time to upload.

### Example B — supplier reports to a web endpoint

Two suppliers send daily reports. An internal API receives them.

```json
{
  "hostname": "mail.example.com",
  "listeners": [
    { "addr": ":25",  "tls": "starttls" },
    { "addr": ":465", "tls": "implicit" }
  ],
  "tls": { "cert": "cert.pem", "key": "key.pem" },
  "limits": {
    "max_message_bytes": 10485760,
    "max_connections": 32,
    "command_timeout_sec": 60,
    "session_timeout_sec": 300
  },
  "dns": { "timeout_sec": 5, "server": "1.1.1.1:53" },
  "retry": {
    "enabled": true,
    "interval_sec": 120,
    "expire_sec": 21600,
    "max_entries": 200,
    "max_bytes": 134217728
  },
  "cidr_whitelist": [],
  "providers": [],
  "whitelist": [
    { "match": "domain", "value": "supplier-one.example" },
    { "match": "domain", "value": "supplier-two.example" }
  ],
  "routes": [
    {
      "recipient": "reports@mail.example.com",
      "match": "address",
      "type": "webhook",
      "url": "https://api.internal/v1/reports",
      "secret": "REPLACE_WITH_openssl_rand_hex_32",
      "timeout_sec": 25
    }
  ]
}
```

Points to note. Both suppliers must publish working SPF records, because `require_spf` defaults to `true`. `expire_sec` is six hours, so a maintenance window on the API does not lose reports. `timeout_sec` on the route is 25 seconds, well under what a sending mail server will wait.

### Example C — Gmail to a local Postfix

Selected people send mail from Gmail. It is relayed to a Postfix on the same machine.

```json
{
  "hostname": "gw.example.com",
  "listeners": [
    { "addr": ":25", "tls": "starttls" }
  ],
  "tls": { "cert": "cert.pem", "key": "key.pem" },
  "limits": {
    "max_message_bytes": 26214400,
    "max_connections": 64,
    "command_timeout_sec": 60,
    "session_timeout_sec": 300
  },
  "dns": { "timeout_sec": 5, "server": "1.1.1.1:53" },
  "retry": {
    "enabled": true,
    "interval_sec": 60,
    "expire_sec": 14400,
    "max_entries": 500,
    "max_bytes": 268435456
  },
  "cidr_whitelist": [],
  "providers": [
    {
      "name": "google",
      "ptr_suffixes": ["google.com", "googlemail.com"],
      "domains": ["gmail.com", "googlemail.com"],
      "require_spf": true
    }
  ],
  "whitelist": [],
  "routes": [
    {
      "recipient": "gw.example.com",
      "match": "domain",
      "type": "forward",
      "host": "127.0.0.1",
      "port": 2525,
      "timeout_sec": 30
    }
  ]
}
```

Points to note. The route uses `"match": "domain"`, so every address at `gw.example.com` is forwarded. This is the one configuration where address enumeration becomes possible again, so use it only when you genuinely want to accept every local address. The provider entry accepts any Gmail address, so the filtering here is by provider and not by individual person. Watch for `sender rejected` entries in case Google changes its reverse DNS naming.

### Example D — two actions on one server

Alerts run a program. Reports go to a webhook. Everything else is refused.

```json
"routes": [
  {
    "recipient": "alert@filter.example.com",
    "match": "address",
    "type": "command",
    "path": "/usr/local/bin/page_oncall",
    "temp_fail_exit_code": 75,
    "timeout_sec": 20
  },
  {
    "recipient": "report@filter.example.com",
    "match": "address",
    "type": "webhook",
    "url": "https://api.internal/v1/reports",
    "secret": "...",
    "timeout_sec": 25
  }
]
```

The sender chooses the action by choosing the address. Nothing inspects the message content.

---

## Quick reference card

```
BUILD           ./build.sh
CHECK CONFIG    ./smtp_filter -config config.json -check
RUN             ./smtp_filter -config config.json
PORT 25         sudo setcap cap_net_bind_service=+ep ./smtp_filter

SERVICE         sudo systemctl {start|stop|restart|status} smtp_filter
RELOAD CONFIG   sudo kill -HUP $(pidof smtp_filter)
LOG             sudo journalctl -u smtp_filter -f -o cat | jq .

RELOAD APPLIES  whitelist, providers, cidr_whitelist, routes, limits, dns
NEEDS RESTART   listeners, tls, retry

TEST            swaks --server HOST:25 --from FROM --to TO

WATCH FOR       every log entry at ERROR level = one lost message
```
