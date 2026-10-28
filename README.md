# cpesim — TR-069 CPE load simulator

Simulates thousands of TR-069 (CWMP) CPEs checking in to an ACS. It is a single
Windows `.exe` with no dependencies.

Each simulated CPE:
- sends `Inform` with the correct events: `0 BOOTSTRAP`, `1 BOOT`, `2 PERIODIC`, `6 CONNECTION REQUEST`, `7 TRANSFER COMPLETE`, `M Reboot`, `M Download`, and so on
- answers Basic or Digest auth challenges and keeps the session cookie
- uses its own TCP connection for each session, like a real device
- answers ACS RPCs: GetRPCMethods, Get/SetParameterValues, GetParameterNames, Get/SetParameterAttributes,
  AddObject, DeleteObject, Reboot, FactoryReset, Download (then TransferComplete), ScheduleInform
- keeps its own TR-181 (`Device.`) or TR-098 (`InternetGatewayDevice.`) data model. Values the ACS sets are kept,
  and a change to `PeriodicInformInterval` changes how often the CPE informs
- retries failed sessions with the TR-069 backoff (section 3.2.1.1)

## Quick start (Windows)

```
cpesim.exe -acs http://acs.example.com:7547/ -user cpeuser -pass cpepass -n 1000 -interval 5m -ramp 5m
```

Stats are printed every 5 seconds. Press **Ctrl+C** to stop and see a summary with latency
percentiles, RPC counts and the most common errors.

## Common load test scenarios

Steady state: 10,000 CPEs informing every 5 minutes, about 33 informs/sec:
```
cpesim.exe -acs http://acs:7547/ -user u -pass p -n 10000 -interval 5m -ramp 5m -duration 30m -csv steady.csv
```

Boot storm, for example after a power outage. Every CPE sends BOOTSTRAP+BOOT once, all at the same time:
```
cpesim.exe -acs http://acs:7547/ -n 5000 -once -ramp 0
```

Same, but at most 500 sessions in flight:
```
cpesim.exe -acs http://acs:7547/ -n 5000 -once -ramp 0 -max-sessions 500
```

### Boot storm until everyone is in (time-to-recover test)

Every CPE boots at the same moment and keeps retrying with the TR-069 backoff until its session
succeeds. The program exits when all CPEs are in, or when `-duration` runs out.
```
cpesim.exe -acs http://acs:7547/ -user u -pass p -n 14000 -until-success -ramp 0 -csv storm.csv
```
Each report line shows how many devices are in so far, for example `in 10134/14000 (72.4%)`. The final summary shows:
- time from boot to each device's first successful session (p50/p90/p95/p99/slowest)
- how long until 25/50/75/90/95/99/100% of all devices were in
- a histogram of how many attempts devices needed
- the error breakdown (HTTP 503, timeouts, connection refused, ...). Errors caused by the test PC
  running out of sockets are labelled `[CLIENT-SIDE, not the ACS]`. If you see those, the result
  measures your PC and not the ACS. See *Windows tuning* below.

The retry waits follow TR-069 section 3.2.1.1: retry *n* waits a random time in
`[m·k^(n-1), m·k^n)`, where `m = -retry-min-wait` (default 5s) and `k = -retry-multiplier/1000` (default 2000 → 2).
That gives 5–10s, 10–20s, 20–40s ... capped at 2560–5120s from the 10th retry. Set these to match the
`CWMPRetryMinimumWaitInterval`/`CWMPRetryIntervalMultiplier` your real devices use. Real devices don't all
boot within the same millisecond, so `-ramp 30s` (spread the boots over 30s) is often more realistic than `-ramp 0`.

Run a second machine or process with a different serial range so the CPEs do not collide:
```
cpesim.exe -acs http://acs:7547/ -n 10000 -start 10001
```

Debug one CPE by dumping all of its SOAP traffic:
```
cpesim.exe -acs http://acs:7547/ -n 1 -once -trace 0
```

Test ACS-initiated sessions (connection requests):
```
cpesim.exe -acs http://acs:7547/ -n 1000 -cr-port 7547 -cr-host 192.168.1.50
```
Each CPE reports `http://<cr-host>:<cr-port>/<serial>` as its ConnectionRequestURL. Allow the port
through Windows Firewall. The simulator does not check connection request credentials.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `-acs` | (required) | ACS URL (http or https) |
| `-user`, `-pass` | | ACS credentials. Basic or Digest is picked from the ACS challenge |
| `-n` | 100 | number of CPEs |
| `-start` | 1 | first serial index |
| `-serial-prefix` / `-serial-digits` | SIM / 8 | serial = `SIM00000001`... |
| `-oui`, `-product-class`, `-manufacturer`, `-model`, `-sw-version`, `-hw-version` | | device identity |
| `-datamodel` | tr181 | `tr181` (Device.) or `tr098` (InternetGatewayDevice.) |
| `-extra-params` | 0 | add N dummy parameters (`X_LOADTEST.Param.N`) for heavier GetParameterValues |
| `-interval` | 5m | periodic inform interval |
| `-jitter` | 0.1 | ±10% random spread on the interval |
| `-ramp` | 1m | spread first informs over this time (0 = all at once) |
| `-duration` | 0 | stop after this long (0 = until Ctrl+C) |
| `-once` | false | one session per CPE, then exit |
| `-until-success` | false | boot storm: retry until one session succeeds, then exit; reports time-to-get-in |
| `-retry-min-wait` | 5s | CWMPRetryMinimumWaitInterval |
| `-retry-multiplier` | 2000 | CWMPRetryIntervalMultiplier (2000 = doubles each retry) |
| `-bootstrap` | true | include `0 BOOTSTRAP` in the first inform (`-bootstrap=false` to disable) |
| `-max-sessions` | 0 | limit concurrent sessions |
| `-timeout` | 30s | per HTTP request timeout |
| `-boot-delay` | 5s | simulated reboot time |
| `-insecure` | false | accept any TLS certificate |
| `-cwmp-ns` | urn:dslforum-org:cwmp-1-0 | CWMP namespace used in Inform |
| `-fetch-downloads` | false | actually download files sent with the Download RPC (load tests the file server too) |
| `-cr-port`, `-cr-host` | 0 / auto | connection request listener |
| `-bind-ips` | | comma separated local IPs to spread outgoing connections over |
| `-shared-conns` | false | reuse keep-alive connections across CPEs (fewer sockets) |
| `-report` | 5s | stats print interval |
| `-csv` | | write stats to a CSV file (open in Excel to chart) |
| `-log-errors` | false | log every failed session |
| `-trace` | -1 | dump SOAP traffic for CPE number N |

## Windows tuning for large tests

Each session opens a new TCP connection, as real CPEs do. Closed sockets stay in TIME_WAIT for a
while, and by default Windows has only about 16k ephemeral ports. Above roughly 100–200 sessions/sec
from one machine you may see `bind: An operation on a socket could not be performed because the system
lacked sufficient buffer space`, or similar errors. Fixes:

1. Widen the port range (admin cmd):
   ```
   netsh int ipv4 set dynamicport tcp start=10000 num=55535
   ```
2. Shorten TIME_WAIT: set `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\TcpTimedWaitDelay`
   (DWORD) to `30`, then reboot.
3. Add more local IPs to the NIC and use `-bind-ips 10.0.0.5,10.0.0.6,...`, or run on several machines.
4. As a last resort, use `-shared-conns`. This is less realistic, and it breaks ACSs that track the
   session by TCP connection instead of by cookie.

If the client machine runs out of CPU before the ACS does, the numbers measure the client and not
the ACS. Watch Task Manager while the test runs.

## Building from source

Install Go 1.22+ and run:
```
go build -o cpesim.exe .
```
To cross-compile from Linux or macOS: `GOOS=windows GOARCH=amd64 go build -o cpesim.exe .`
