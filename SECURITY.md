# Security

## Reporting a vulnerability

Report it privately with GitHub private vulnerability reporting on this repository: open the Security tab, then
"Report a vulnerability". Do not open a public issue or pull request for it.
If that form is not available, email ops@sengi.run and say only that you have a security report; do not include
details.

We will acknowledge your report within 7 days. After that we respond on a best-effort basis; there is no
fixed timeline.

## In scope

- A guest VM reaching the host, the local network or another guest.
- A secret reaching a guest it was not sent to, a log, a result or a process list.
- A VM outliving its lease.
- A driver accepting a forged handle.

## Known limits, not bugs

- The machine that runs a box can read everything in it. Do not put secrets in a box on a machine you do not trust.
- `zm-run-local` runs jobs unsandboxed as its own user.
- A network on the OCI layer is only as filtered as the firewall the host's owner set up.
- The conformance suite proves how a driver behaves. It does not prove that the driver's author can be trusted.

The project is early. Expect interfaces to change.
