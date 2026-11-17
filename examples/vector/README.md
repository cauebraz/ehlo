# Vector to ehlo

[Vector](https://vector.dev) reads an MTA log, turns each line into ehlo events with a `remap` transform and posts them as NDJSON to `POST /v1/events`. Tested with Vector `0.62.0` (`timberio/vector:0.62.0-debian@sha256:6266b1d35c844918ff1c48ea2ec4f57c2b0664c46f3e17fc2418d5dda1692068`).

| Directory | Reads |
|-----------|-------|
| `powermta/` | PowerMTA accounting CSV files |
| `json/` | One JSON object per line from any MTA, as a template |

Edit `uri` in the `http` sink to point at your ehlo. Vector 0.62 does not expand `${VAR}` in config files unless started with `--dangerously-allow-env-var-interpolation`, so the examples use literal values.

`test.sh` builds ehlo, starts it on port 18081, runs both configs in Docker on the sample files and checks the event totals:

```console
$ examples/vector/test.sh
...
PASS: both Vector examples posted the expected events
```

## PowerMTA

The mapping follows the accounting file chapter of the PowerMTA 5.0 User's Guide (an unofficial copy is at https://jnotes.github.io/pmta-doc; the official guide needs a vendor login). It is not verified against a running PowerMTA. Check the header line of your file against the column list below before relying on it.

Add an accounting file for ehlo next to your existing ones, with the same fields for `d`, `b` and `t`, and a second one for feedback loop reports:

```
<acct-file /var/log/pmta/ehlo-acct.csv>
    records d, b, t
    record-fields d timeLogged,timeQueued,rcpt,dsnStatus,dsnDiag,dsnMta,bounceCat,dlvSourceIp,vmta
    record-fields b timeLogged,timeQueued,rcpt,dsnStatus,dsnDiag,dsnMta,bounceCat,dlvSourceIp,vmta
    record-fields t timeLogged,timeQueued,rcpt,dsnStatus,dsnDiag,dsnMta,bounceCat,dlvSourceIp,vmta
    move-interval 1d
    max-size 250M
</acct-file>

<acct-file /var/log/pmta/ehlo-fbl.csv>
    records f
    record-fields f timeLogged,feedbackType,rcpt
    move-interval 1d
</acct-file>
```

The transform reads columns by position: `type,timeLogged,timeQueued,rcpt,dsnStatus,dsnDiag,dsnMta,bounceCat,dlvSourceIp,vmta` and, for the feedback file, `type,timeLogged,feedbackType,rcpt`. It skips the header line PowerMTA writes at the start of each file. Timestamps may be `2026-03-10 08:15:02+0000`, `03/10/2026 08:15:02+0000` or Unix seconds.

| Record | Events |
|--------|--------|
| `d` | `attempt` and a `2xx` `reply` at stage `data` |
| `t` | `attempt` and the `reply`; a `dsnDiag` without an SMTP code is a connection failure |
| `b` | `attempt`, `reply` and `bounce`. `bounceCat` `message-expired`, or no `dsnMta` and no code: the `bounce` only |
| `f` with `feedbackType` `abuse` | `complaint` with source `powermta` |

- `source` is `vmta`, else `dlvSourceIp`, else `powermta`.
- `code` comes from `dsnDiag` (`smtp;421 ...`), `enhanced` from `dsnStatus` (`4.7.0 (...)`), `text` from `dsnDiag` cut to 128 characters.
- `attempt` is `retry` when the message waited 60 seconds or more between `timeQueued` and `timeLogged`, the same rule as `ehlo tail-postfix -retry-after`.
- `bounce` is `policy` for `policy-related`, `spam-related`, `content-related`, `invalid-sender` or a `5.7.x` status, `soft` for `message-expired` or a `4.x.x` status, else `hard`.
- Feedback records name no VirtualMTA, so complaints land on source `powermta`, not on the lane that sent the message.

## Any MTA writing JSON lines

`json/vector.yaml` expects lines like `json/sample.json`:

```json
{"time":"2026-03-10T08:15:03Z","status":"deferred","rcpt":"user2@yahoo.com","pool":"pool-a","attempt":1,"code":421,"enhanced":"4.7.0","response":"421 4.7.0 ..."}
```

`status` is `sent`, `deferred`, `bounced`, `expired` or `complaint`; other values and lines that are not JSON are dropped. Rename the fields in the `remap` source to match your log.
