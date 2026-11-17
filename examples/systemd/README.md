# systemd units

| Unit | Runs |
|------|------|
| `ehlo.service` | `ehlo serve` with `/etc/ehlo/ehlo.yaml` and state in `/var/lib/ehlo` |
| `ehlo-tail-postfix.service` | `journalctl` piped into `ehlo tail-postfix`, for Debian 12+ and Ubuntu without rsyslog |
| `ehlo-tail-postfix-file.service` | `tail` of `/var/log/mail.log` piped into `ehlo tail-postfix`, for hosts with rsyslog |

Run one of the two tailers, not both: each line would count twice.

## Install

```console
$ sudo install -m 0755 ehlo /usr/local/bin/ehlo
$ sudo install -d /etc/ehlo && sudo install -m 0644 ehlo.example.yaml /etc/ehlo/ehlo.yaml
$ sudo cp examples/systemd/ehlo.service examples/systemd/ehlo-tail-postfix.service /etc/systemd/system/
$ printf 'EHLO_SOURCE=%s\nEHLO_TARGET=http://127.0.0.1:8080\n' "$(hostname -s)" | sudo tee /etc/default/ehlo-tail-postfix
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now ehlo.service ehlo-tail-postfix.service
```

`/etc/default/ehlo-tail-postfix` sets `EHLO_SOURCE` (the `source` label, default `mx-1`) and `EHLO_TARGET` (the ehlo URL). The file variant also reads `EHLO_LOG` (default `/var/log/mail.log`).

## What the units do

- Both tailers run as a dynamic user. The journald unit joins `systemd-journal` to read the journal; the file unit joins `adm`, the group of `/var/log/mail.log` on Debian and Ubuntu.
- `journalctl --unit="postfix*"` matches `postfix@-.service` on Debian and Ubuntu and every multi-instance `postfix@<name>.service`.
- Both start at the end of the log (`--lines=0`, `tail --lines=0`). Lines written while the tailer is stopped are not read. `journalctl --cursor-file` would resume, but journalctl 252 does not save the cursor when systemd stops it with SIGTERM.
- When `ehlo tail-postfix` or the reader exits, the pipe closes, the unit stops and systemd restarts it after 5 seconds.
- Hardening: `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, no capabilities, `NoNewPrivileges`, `@system-service` system calls only, `MemoryDenyWriteExecute`, and only IPv4, IPv6 and Unix sockets. `systemd-analyze security` rates each unit 1.2 or 1.3 (OK) on systemd 252.

## Verified

On Debian 12 (systemd 252, Postfix 3.7.11) in a container: `systemd-analyze verify` passes for the three units; with the journald unit, then the file unit after installing rsyslog 8.2302, mail sent through Postfix to local SMTP sinks raised `ehlo_smtp_attempts_total` and `ehlo_smtp_replies_total` for `250`, `450 4.3.0`, `500 5.3.0` and `connect`.
