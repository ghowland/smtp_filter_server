# SMTP Filter — Technical Specification

**Executable:** `smtp_filter`
**Module:** `smtpfilter`
**Language:** Go 1.22 or later
**Version:** 1.0

This document is written to be read from the beginning. Each section introduces the terms that later sections use. A reader who knows SMTP can start at section 3.

---

# Part 1 — Background

## 1. What SMTP is

The Simple Mail Transfer Protocol moves an electronic mail message from one computer to another. It is defined by RFC 5321.

The protocol is a conversation over a TCP connection. The computer that sends the message is the client. The computer that receives it is the server. The client sends short text commands. The server answers each command with a three-digit code and a line of text.

A complete delivery has this shape.

```
server:  220 mail.example.com ready
client:  EHLO sender.example
server:  250-mail.example.com greets sender.example
         250 SIZE 10485760
client:  MAIL FROM:<alice@sender.example>
server:  250 OK
client:  RCPT TO:<bob@example.com>
server:  250 OK
client:  DATA
server:  354 Send data, end with a single dot
client:  Subject: hello
         (blank line)
         message text
         .
server:  250 OK
client:  QUIT
server:  221 closing connection
```

Five terms come from this exchange and are used throughout this document.

| Term | Meaning |
|---|---|
| Peer | The remote computer that opened the connection. |
| Reverse-path | The address in `MAIL FROM`. It states who sent the message. |
| Forward-path | The address in `RCPT TO`. It states who should receive the message. |
| Envelope | The reverse-path and the forward-path together. |
| The dot | The single period on a line by itself that ends the message text. |

The reply code tells the client what to do next. The first digit is what matters.

| First digit | Meaning | Client behaviour |
|---|---|---|
| 2 | Success. | Continue, or consider the message delivered. |
| 3 | Send more data. | Used only for `DATA`. |
| 4 | Temporary failure. | Try again later, usually for several days. |
| 5 | Permanent failure. | Stop. Report the failure to the person who sent the message. |

## 2. What SPF is

The Sender Policy Framework, defined by RFC 7208, lets the owner of a domain publish a list of the computers that are permitted to send mail for that domain. The list is published as a DNS TXT record.

A receiving server takes the domain from the reverse-path, reads that domain's SPF record, and compares the record to the IP address of the peer. The comparison gives one of seven results.

| Result | Meaning |
|---|---|
| `pass` | The domain owner authorises this address. |
| `fail` | The domain owner denies this address. |
| `softfail` | The domain owner suspects this address but does not deny it. |
| `neutral` | The domain owner makes no statement about this address. |
| `none` | The domain publishes no SPF record. |
| `temperror` | A DNS query failed. The result is unknown. |
| `permerror` | The SPF record is malformed. |

SPF answers one question only. It states whether an address may send for a domain. It says nothing about the content of the message.

## 3. What this program is

`smtp_filter` is an SMTP server that accepts mail from a small, known set of senders and refuses everything else. For each accepted message it performs one configured action.

It is not a general purpose mail server. The following are outside its scope and are not implemented.

- Mailbox storage.
- Retrieval protocols such as POP3 and IMAP.
- User accounts and SMTP authentication.
- Message submission from mail clients.
- Delivery status notifications and bounce messages.
- Address enumeration commands.
- Relay to arbitrary destination domains.
- Outbound reputation management, Sender Rewriting Scheme, and DKIM signing.

The intended use is a machine-to-machine mail path. Examples are an alert from a monitoring system, a report from a supplier, or a scanned document from an office device. In each case the sender is known in advance and the message must trigger a specific action rather than sit in a mailbox.

## 4. The four design constraints

Every decision in this document follows from these four rules.

**Constraint 1. No disk write.** The process writes nothing to any filesystem. Message data exists only in memory. It needs read access to the configuration file, the certificate, and the private key. It can run on a read-only filesystem.

**Constraint 2. All configuration is JSON.** One file. No command line configuration beyond the path to that file.

**Constraint 3. No information is disclosed after acceptance.** Once the server has accepted a message, it reports `250` regardless of what happens to that message afterwards. Section 12 explains this rule and its cost.

**Constraint 4. Minimal protocol surface.** Only the commands needed for delivery are implemented. Nothing else is advertised.

---

# Part 2 — Core concepts

## 5. The two gates

A message must pass two checks before the server accepts it. Both happen before the message text is transmitted, so a refused message costs almost no bandwidth.

**Gate 1 runs at `MAIL FROM`.** It answers the question: may this peer send mail here, claiming to be this sender? Section 8 defines the rules.

**Gate 2 runs at `RCPT TO`.** It answers the question: is this recipient address one the server recognises, and what should be done with a message sent to it? Section 9 defines this.

If both gates pass, the server reads the message text and performs the action.

## 6. The recipient is a key, not a mailbox

In a normal mail server the forward-path names a mailbox where the message will be stored.

In this program there is no mailbox. The forward-path is a key into a table called the route table. Each entry in that table names one action. The recipient address therefore selects what happens to the message.

This has two consequences.

First, one server can perform many independent functions without inspecting the message content. A message to `alerts@filter.example.com` can trigger a program, and a message to `reports@filter.example.com` can be sent to a web endpoint, purely because of the address used.

Second, there is no catch-all entry. An address with no route is refused. An attempt to guess valid addresses returns nothing useful, and mail to any other address is refused before the text is sent.

## 7. Forward-confirmed reverse DNS

A large mail provider such as Gmail sends mail on behalf of many millions of individual addresses. Those addresses cannot be listed in advance. The server must therefore be able to recognise the provider itself.

Forward-confirmed reverse DNS is the procedure that does this. It has two steps.

1. A PTR query on the peer IP address returns one or more hostnames. This is the name the operator of the address block has published for that address.
2. An A or AAAA query on each returned hostname returns one or more addresses. If the original peer address appears in that set, the hostname is confirmed.

The confirmation matters because a PTR record alone can be set by the operator of the address block to any value. The forward query proves that the operator of the domain name agrees. Both parties must consent for a name to be confirmed.

**MX records are not used for this purpose.** An MX record identifies the hosts that receive mail for a domain. There is no requirement that a domain sends mail from the same hosts, and large providers commonly do not. Host identity is established by forward-confirmed reverse DNS. Send authorisation is established by SPF.

---

# Part 3 — The admission decision

## 8. Gate 1, at `MAIL FROM`

Both inputs are available when this command arrives. The peer IP address is known from the accepted TCP connection. The reverse-path is the argument of the command.

Four rules are evaluated in a fixed order. Evaluation stops at the first rule that matches. The order does not depend on configuration. Configuration supplies data to the rules and never changes the order in which they run.

### Rule A — network whitelist

The peer IP address is tested for containment in each configured network. If a network contains the address, and the entry either lists no sender domains or lists a set that includes the domain of the reverse-path, the sender is admitted.

No DNS query is made. No SPF evaluation is made. This rule is for hosts under the same administrative control, where the address alone is sufficient evidence.

### Rule B — provider whitelist

Forward-confirmed reverse DNS is performed on the peer IP address. If it yields a confirmed hostname, and that hostname equals one of the configured suffixes or is a subdomain of one, and the domain of the reverse-path appears in that provider's domain list, then SPF is evaluated and the sender is admitted on a `pass` result.

Three separate facts are established here.

- The provider entry authorises the host.
- The domain list states which identities that host may present.
- SPF confirms both against the record published by the owner of the sender domain.

The suffix comparison requires an exact match or a match on a dot boundary. A plain string suffix test would accept `notgoogle.com` for the suffix `google.com`.

### Rule C — sender whitelist

The reverse-path is compared to each whitelist entry. An `address` entry matches the complete address. A `domain` entry matches the domain and every subdomain of it. If an entry matches, SPF is evaluated and the sender is admitted on a `pass` result.

### Rule D — default

No rule matched. The sender is refused. The reply is `550` and the connection closes.

### The require_spf flag

Rules B and C evaluate SPF by default. Each individual entry can set `require_spf` to `false`, in which case the entry admits on the name match alone.

The flag belongs to the entry and not to the server, so that one sender with a defective SPF record can be excepted without weakening the check for every other sender.

### SPF result mapping

| SPF result | Action |
|---|---|
| `pass` | Admit. |
| `fail` | Reply `550`, close. |
| `softfail` | Reply `550`, close. |
| `neutral` | Reply `550`, close. |
| `none` | Reply `550`, close. |
| `permerror` | Reply `550`, close. |
| `temperror` | Reply `451`, close. |

Two of these need explanation.

`softfail` and `neutral` are refused. In a general purpose mail server these results are usually accepted, because refusing them would reject too much legitimate mail from senders with incomplete records. Here the set of legitimate senders is known in advance, so an uncertain statement from the domain owner carries no value.

`temperror` produces `451` and not `550`. A DNS failure is not evidence of forgery. A permanent refusal would discard valid mail during a network fault. A temporary refusal causes the sending server to try again later.

## 9. Gate 2, at `RCPT TO`

The forward-path is looked up in the route table. An entry that matches the complete address is selected before an entry that matches the domain. Comparison is case-insensitive.

If a route is found, it is recorded on the session and the reply is `250`.

If no route is found, the reply is `550` and the connection closes.

Only one recipient is accepted per message. A second `RCPT TO` in the same transaction returns `452`. The reason is that the recipient selects the action. Two recipients would select two actions, with no defined order and no defined behaviour if one succeeds and the other fails.

## 10. DNS

One shared `net.Resolver` value serves every session. That type is safe for concurrent use.

Every query carries a context deadline taken from `dns.timeout_sec`. A single unresponsive resolver must not hold an SMTP session open.

No result is cached. A sender that opens several connections in a short interval causes one set of queries per connection. This is a deliberate simplification. A cache would reduce query volume but adds expiry handling and a second source of stale data.

The SPF library used, `github.com/mileusna/spf`, accepts no context and exposes no resolver interface. The deadline is therefore applied around the call rather than inside it, by running the call in a goroutine and selecting against the context. The channel that carries the result is buffered with capacity one. Without that buffer the inner goroutine would block permanently on its send after a timeout and would leak. With the buffer the goroutine ends when the underlying query ends and is then collected. The session is released at the deadline in both cases.

The library selects its resolver through a package-level variable, `spf.DNSServer`, whose default is `8.8.8.8:53`. The `dns.server` configuration field exists only to set that variable.

---

# Part 4 — Acceptance and action

## 11. The commit point

The reply the server sends after the dot transfers responsibility for the message from the sender to the server. This is the most important boundary in the design.

Before that reply, the sender still owns the message. A `4xx` reply makes the sender try again. A `5xx` reply makes the sender report the failure to the person who wrote the message. Either way, the message is not lost.

After that reply, the server owns the message. The sender will not try again and will not report anything. If the server then loses the message, nobody is told.

## 12. The disclosure rule

**Before the dot, refusal is permitted and correct.**

A refusal at `MAIL FROM` or `RCPT TO` is a statement that the server will not accept mail from this sender, or for this recipient. It discloses nothing the sender does not already possess. It also prevents the transmission of a message text that would be discarded, which saves bandwidth and time.

**After the dot, the reply is always `250`.**

No failure of any system behind the filter is reported to the sending host. The sender is told the message was delivered, whatever actually happened. This prevents the sender from learning anything about the internal systems, and it prevents the sender from trying again, because retry is the server's responsibility from that point.

### The cost of this rule

The server accepts a duty of delivery that it may fail to discharge, and it fails silently. The retry queue in section 15 reduces how often that happens but does not remove the possibility.

**The log is therefore the only record that a message existed and the only evidence that one was lost.** This makes logging an operational requirement rather than a convenience. Section 19 lists the events that must be watched.

## 13. Reading the message text

The message text is read into a byte buffer owned by the session goroutine. It is released when the session ends, or transferred to the queue if the first delivery attempt fails temporarily.

The value of `max_message_bytes` is advertised in the `EHLO` reply through the `SIZE` extension and is enforced during the read, not after.

Two transformations are applied while reading.

**Dot-unstuffing.** A client that needs to send a line starting with a period must send two periods, so that the line is not mistaken for the terminating dot. The server removes the first period.

**Line ending normalisation.** A line terminated by a bare line feed is normalised to a carriage return and line feed.

If the size limit is exceeded, the buffer is released, the remainder of the stream is read and discarded until the dot is found, and the reply is `250`. A `552` reply is not sent, because the message has passed the commit boundary. The event is logged at error level.

## 14. The three actions

The route selects one of three actions. Each reports one of three results: success, temporary failure, or permanent failure.

### Command

A local program is started with `exec.CommandContext`. The program is invoked directly. **A shell is never used**, so no part of the envelope can be interpreted as a shell construction.

The message text is written to the program's standard input. The envelope is supplied in the environment and is never placed into a string that is later parsed.

| Variable | Content |
|---|---|
| `SMTPFILTER_FROM` | The reverse-path. |
| `SMTPFILTER_TO` | The forward-path. |
| `SMTPFILTER_PEER` | The peer IP address. |
| `SMTPFILTER_SIZE` | The message size in octets. |

Standard output and standard error are captured into buffers bounded at 8192 octets each and written to the log. The bound exists so that a program which writes without limit cannot consume the memory of this process.

| Condition | Result |
|---|---|
| Exit code zero | Success |
| Exit code equal to `temp_fail_exit_code` | Temporary failure |
| Any other exit code | Permanent failure |
| Deadline expiry | Temporary failure |
| The program could not be started | Temporary failure |

The last row deserves a note. A missing binary will not resolve itself, so permanent failure would be defensible. Temporary failure is chosen because a fork failure under memory pressure produces the same condition and does resolve itself.

### Webhook

The message text is sent as the body of an HTTP POST request, with content type `message/rfc822`.

The URL comes from configuration only and is never derived from message content. Redirects are not followed, so that a compromised or misconfigured endpoint cannot direct the message to a different host. The response body is drained and discarded up to 4096 octets so that the connection can be reused.

| Header | Content |
|---|---|
| `X-Smtpfilter-From` | The reverse-path. |
| `X-Smtpfilter-To` | The forward-path. |
| `X-Smtpfilter-Peer` | The peer IP address. |
| `X-Smtpfilter-Signature` | `sha256=` followed by the hexadecimal HMAC-SHA256 of the body, keyed with the route secret. |

The signature lets the receiving system prove the request came from this server. Verify it with a constant-time comparison.

| Status | Result |
|---|---|
| 2xx | Success |
| 5xx | Temporary failure |
| 429 | Temporary failure |
| Any other 4xx | Permanent failure |
| Transport fault or deadline expiry | Temporary failure |

### Forward

An SMTP session is opened to the configured host and port. The original reverse-path and forward-path are presented unchanged and the message text is sent with dot-stuffing applied.

The client is written directly rather than taken from `net/smtp`, so that the route deadline applies to every read and every write in that session.

**The target must be a mail server under the same administrative control**, such as a local Postfix instance. Because of that, Sender Rewriting Scheme is not applied, SPF alignment at the target is not considered, and outbound reputation is not managed. Pointing this at a third-party mail server will produce SPF failures at the target, because that server will see this machine sending for a domain that has not authorised it.

| Condition | Result |
|---|---|
| 250 after the dot | Success |
| 4xx reply | Temporary failure |
| 5xx reply | Permanent failure |
| Connection fault or deadline expiry | Temporary failure |

## 15. The retry queue

The action is attempted once while the sending host is still connected. The queue holds only what that attempt failed to deliver, so the ordinary path carries no queue overhead.

### Structure

The queue is a slice of entries protected by a mutex. Each entry holds the message, the time of acceptance, the time of the next attempt, and the number of attempts made.

### The worker

One goroutine runs a ticker at `retry.interval_sec`. On each tick it takes the mutex, removes expired entries, copies out the entries that are due, releases the mutex, attempts each one, then takes the mutex again to record the results.

**The mutex is not held during an attempt.** A slow endpoint would otherwise block every session goroutine that wants to add a message.

### Removal

An entry is removed by swapping the final element into the vacated position and truncating the slice. Order is not preserved, and order carries no meaning here, because each entry holds its own next-attempt time and the destinations are independent of one another.

The vacated final position is set to the zero value before truncation. Without that, the backing array keeps a reference to the message and the garbage collector cannot reclaim the message text until that position is overwritten.

When several entries are removed in one pass, iteration runs from the end of the slice toward the beginning.

### Limits

A message is not added when the entry count has reached `max_entries` or when adding it would take the total held size past `max_bytes`. A refused addition is a discard, because the sending host already received `250`. The event is logged at error level.

### Expiry

An entry is removed when its age exceeds `retry.expire_sec`. This is the only stop condition. The event is logged at error level, because it is the silent loss of a message.

### Restart

The queue does not survive a process restart. The shutdown procedure attempts one final delivery of every held message, bounded at 30 seconds. Anything still held when that limit passes is lost and each loss is logged.

---

# Part 5 — Implementation

## 16. Process model

```
main
 ├── listener goroutine (one per configured listener)
 │    └── session goroutine (one per accepted connection)
 │         └── performs DNS, SPF, text read, and the action inline
 └── queue worker goroutine (one)
```

There is no worker pool and no other background activity. The accept loop is written directly against `net.Listener`, because Go supplies no accept loop for raw TCP connections.

Three items are shared between goroutines.

| Item | Access model |
|---|---|
| Configuration | Immutable after load, held behind `atomic.Pointer`. A session reads the pointer once at start and uses that value for its whole lifetime. |
| Retry queue | Protected by a mutex. |
| Connection semaphore | A buffered channel that bounds the number of concurrent sessions. |

The session goroutine owns its envelope, its route, and its message buffer. Nothing else is shared.

## 17. Package layout

```
cmd/smtp_filter/main.go     Configuration load, listener start, signal handling
internal/config/            JSON structures, validation, lookup tables
internal/msg/               Message value and Dispatcher interface
internal/policy/            Resolver interface, rules A to D, SPF mapping
internal/server/            Accept loop, session state machine, transaction
internal/dispatch/          Command, webhook, and forward actions
internal/queue/             Slice queue and worker
```

Two points about the arrangement.

`internal/msg` exists so that `internal/dispatch`, `internal/queue`, and `internal/server` all share the message type and the action interface without any of them importing another. Every dependency arrow points one way.

`internal/policy` declares the `Resolver` interface itself, rather than taking a concrete type. The production implementation wraps `net.Resolver` and the SPF library. The admission rules therefore contain no I/O and can be exercised against any implementation.

The queue wraps the dispatcher rather than sitting beside it. The session sees one `Dispatcher` and the retry behaviour is invisible to it.

## 18. Transport and listeners

Each listener has an address and a mode. All listeners run the same session handler.

| Mode | Behaviour |
|---|---|
| `plain` | No encryption. `STARTTLS` is not advertised. |
| `starttls` | The connection starts unencrypted. `STARTTLS` is advertised in the `EHLO` reply. |
| `implicit` | The connection is encrypted from the first byte. The listener itself is wrapped in TLS. |

One `tls.Config` value is shared by every listener that needs it. It carries the certificate and a minimum version of TLS 1.2. Client certificates are not requested and not verified.

**The STARTTLS reset.** When the handshake completes, the session state returns to the state that exists immediately after connection establishment. The client must send `EHLO` again. Any reverse-path recorded before the upgrade is discarded. RFC 3207 requires this. It is a correctness requirement and not an option, because commands sent before the upgrade were not protected and must not carry over into the protected session.

The session records whether the transport is encrypted. The flag is written to the log.

### Protocol details

| Item | Value |
|---|---|
| Command line limit | 512 octets, as required by RFC 5321. A longer line gives `500` and closes the connection. |
| Line limit inside the message text | 65536 octets. Larger than the 1000 octets of RFC 5321, so that a sender with long header lines is not refused, but bounded so that one line cannot consume unlimited memory. |
| Bare line feed in the command stream | Rejected. |
| Command timeout | The permitted interval between individual commands. |
| Session timeout | The total permitted lifetime of one connection. Bounds the command timeout. |

### Commands

| Command | Behaviour |
|---|---|
| `EHLO` | Accepted. Returns the hostname and the extension list. |
| `HELO` | Accepted. Returns the hostname only. |
| `STARTTLS` | Accepted when the mode is `starttls` and the transport is not already encrypted. |
| `MAIL FROM` | Accepted. Runs Gate 1. |
| `RCPT TO` | Accepted. Runs Gate 2. |
| `DATA` | Accepted after a successful `RCPT TO`. |
| `RSET` | Accepted. Clears the envelope. |
| `NOOP` | Accepted. |
| `QUIT` | Accepted. Returns `221` and closes. |
| `AUTH`, `VRFY`, `EXPN`, `ETRN`, `HELP` | `502 Command not implemented`. |

The extension list contains only what is implemented: `SIZE`, `8BITMIME`, `PIPELINING`, and `STARTTLS` where the mode permits it.

## 19. Reply code summary

| Stage | Condition | Reply |
|---|---|---|
| `MAIL FROM` | No rule matched | `550`, close |
| `MAIL FROM` | SPF fail, softfail, neutral, none, or permerror | `550`, close |
| `MAIL FROM` | SPF temperror, or a DNS deadline | `451`, close |
| `RCPT TO` | No route for the recipient | `550`, close |
| `RCPT TO` | Second recipient in one transaction | `452` |
| Final dot | Delivered | `250` |
| Final dot | Temporary failure, message queued | `250` |
| Final dot | Temporary failure, queue full | `250`, discard, log |
| Final dot | Permanent failure | `250`, discard, log |
| Final dot | Message text exceeded the size limit | `250`, discard, log |

Every row above the dot carries a real status code. Every row at the dot is `250`.

## 20. Logging

The log is written to standard output in JSON. Every entry for a message carries the peer address, the reverse-path, the forward-path, the route, and the outcome.

**Every entry at error level is a message that was accepted and then lost.** Set an alert on all of them.

| Level | Event | Meaning |
|---|---|---|
| error | body exceeded size limit | The sender exceeded `max_message_bytes` after the envelope was admitted. |
| error | permanent failure | The action reported a permanent fault on the first attempt. |
| error | queue entry limit reached | The queue held `max_entries`. |
| error | queue byte limit reached | The queue held `max_bytes`. |
| error | queue entry expired | The message was retried until `expire_sec` and never delivered. |
| error | permanent failure on retry | The action reported a permanent fault during a retry. |
| error | queue not drained before shutdown | The drain limit passed with messages still held. |
| warn | sender rejected | Rule D, or an SPF result other than pass. Carries the rule and the SPF result. |
| warn | sender deferred | An SPF temporary error or a DNS deadline. |
| warn | recipient rejected | No route matched the forward-path. |
| warn | message queued | The first attempt failed temporarily. |
| info | sender admitted | Carries the rule, the SPF result, and the confirmed reverse DNS name. |
| info | message delivered | |
| info | command output | Captured standard output and standard error. |

A `sender rejected` or `recipient rejected` entry is the only symptom of a fault in the whitelist or the route table, because the sending host receives an ordinary refusal and reports nothing back to the operator. Read these after every configuration change.

---

# Part 6 — Configuration

## 21. Loading and validation

The file is read once at start and parsed into an immutable structure. Unknown fields are rejected. Validation faults accumulate, so one run reports every fault in the file rather than the first.

Reload replaces the structure by swapping the atomic pointer. Sessions already running continue with the structure they began with. The listener set and the retry parameters are fixed for the lifetime of the process.

Validation rules:

- Every listener address must parse and every mode must be one of the three defined values.
- The certificate and key must be present when any listener uses `starttls` or `implicit`.
- Every CIDR entry must parse as a network.
- Every route must specify a type and the parameters that type requires.
- No two routes may specify the same recipient with the same match type.
- Every timeout and interval must be greater than zero.
- `retry.expire_sec` must be greater than `retry.interval_sec`.

## 22. Full configuration reference

```json
{
  "hostname": "filter.example.com",

  "listeners": [
    { "addr": ":25",  "tls": "starttls" },
    { "addr": ":465", "tls": "implicit" }
  ],

  "tls": {
    "cert": "cert.pem",
    "key":  "key.pem"
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

### Field reference

**hostname** — The name presented in the banner and the `EHLO` reply.

**listeners** — `addr` is a bind address. `tls` is `plain`, `starttls`, or `implicit`.

**tls** — Paths to the certificate and private key. Relative paths resolve against the working directory.

**limits** — See the table in section 18. The two size values determine the memory budget; see section 24.

**dns** — `timeout_sec` is the deadline for every query. `server` sets the resolver used by the SPF library, in `ip:port` form.

**retry** — See section 15. Set `enabled` to `false` to discard on the first temporary failure.

**cidr_whitelist** — `cidr` is a network. `domains` is optional; when present the sender domain must be in the list, when absent any sender domain is accepted from that network.

**providers** — `name` labels the entry in the log. `ptr_suffixes` is matched against the confirmed reverse DNS name. `domains` lists the sender domains the provider may present. `require_spf` defaults to `true`.

**whitelist** — `match` is `address` or `domain`. A `domain` entry matches the domain and every subdomain. An `address` entry must contain an at sign. `require_spf` defaults to `true`.

**routes** — `recipient` and `match` form the key. `type` selects the action. `timeout_sec` bounds one attempt. Type-specific fields are `path`, `args`, and `temp_fail_exit_code` for a command; `url` and `secret` for a webhook; `host` and `port` for a forward.

---

# Part 7 — Build and operation

## 23. Build

```sh
#!/bin/sh
set -eu

[ -f go.mod ] || {
    go mod init smtpfilter
    go get github.com/mileusna/spf@latest
}

go mod tidy
go vet ./... || true
CGO_ENABLED=0 go build -trimpath -o smtp_filter ./cmd/smtp_filter

[ -f cert.pem ] || {
    echo "-- generating cert.pem and key.pem"
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout key.pem -out cert.pem \
        -subj "/CN=localhost" \
        -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
}
```

**Note on the executable name.** The current source tree has the main package at `cmd/smtpfilter`. To produce `smtp_filter` as specified, rename that directory:

```sh
git mv cmd/smtpfilter cmd/smtp_filter
```

The build script above assumes the rename has been made. The module name stays `smtpfilter`, so no import path changes.

## 24. Running

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `config.json` | Path to the configuration file. |
| `-check` | off | Validate the configuration and exit. |
| `-debug` | off | Log at debug level. |

```sh
./smtp_filter -config config.json -check
./smtp_filter -config config.json
```

Port 25 needs the bind capability. Apply it after each build, since the capability attaches to the file:

```sh
sudo setcap cap_net_bind_service=+ep ./smtp_filter
```

| Signal | Effect |
|---|---|
| `SIGHUP` | Reload the configuration. A defective file is rejected and the running configuration is kept. |
| `SIGINT`, `SIGTERM` | Stop the listeners, attempt one final delivery of every queued message for up to 30 seconds, then exit. |

Test a delivery:

```sh
swaks --server 127.0.0.1:25 --from alerts@vendor.com --to hook@filter.example.com
```

## 25. Memory budget

Two products bound the memory used by message data.

```
sessions = max_message_bytes x max_connections
queue    = retry.max_bytes
```

The sample configuration gives 640 MiB of session buffers and 256 MiB of queue, plus a fixed process overhead. Reduce `max_connections` or `max_message_bytes` if that exceeds the host.

## 26. Deployment

The process writes nothing, so it can run on a read-only filesystem with read access to the configuration, the certificate, and the key only.

```ini
[Unit]
Description=SMTP filter server
After=network.target

[Service]
WorkingDirectory=/opt/smtp_filter
ExecStart=/opt/smtp_filter/smtp_filter -config config.json
User=smtp_filter
AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadOnlyPaths=/opt/smtp_filter
NoNewPrivileges=yes
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Remove `NoNewPrivileges` if a command route needs to change identity. A restart discards the queue.

---

# Part 8 — Boundaries

## 27. Known limitations

These are design decisions, not defects.

- **An accepted message can be lost silently.** The reply after the dot is always `250`. The queue reduces the frequency but does not remove the possibility. The log is the only record.
- **The queue is in memory only.** A restart or a crash discards it.
- **One recipient per message.**
- **No DNS cache.** Every session performs its own queries.
- **No authentication.** Do not use this for mail submission from clients.
- **No DKIM verification.** DKIM can only be checked after the message text is read, which is past the commit point, so a failure could produce a discard but not a refusal. That makes it much weaker than SPF here.
- **A stale provider list refuses valid mail** with no visible symptom other than the `sender rejected` log entries.
- **The retry interval is fixed.** There is no exponential backoff.
- **The forward action presents `EHLO [address]`** using the local address of the outbound connection. A downstream server that checks the HELO name must be configured to accept an address literal.

## 28. Deferred items

Each of these is additive and none requires rework of what exists.

1. **Per-route TLS requirement.** The session already records the encryption flag. Enforcement would sit in the `RCPT TO` handler with a `530` reply.
2. **Per-route size limit.** The `SIZE` value in the `EHLO` reply precedes route selection, so it would have to advertise the largest value across all routes, with the smaller per-route value enforced during the read.
3. **Per-address connection limit.** A map of counters with expiry, checked at accept time.
4. **Exponential retry backoff.** A doubling interval bounded by `expire_sec`, to reduce load on a downstream system that is down for an extended period.
5. **Configurable HELO name for the forward action.** One string field on the route.
