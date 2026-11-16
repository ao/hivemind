# Running hivemind under systemd

hivemind ships no service file. The units below are the ones the [soak test](soak.md) ran on Amazon Linux 2023 with Docker from the distribution, as root, on one server and three workers. With them, a machine that reboots or is powered on again runs hivemind without anyone logging in.

Both units use the name `hivemind.service`, so `systemctl` and `journalctl -u hivemind` work the same on every machine.

## Server

Create the cluster once, as root:

```sh
sudo hivemind init
```

Then write `/etc/systemd/system/hivemind.service`, with the server's own address in place of `10.0.0.5`:

```ini
[Unit]
Description=hivemind server
Documentation=https://github.com/ao/hivemind
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/hivemind serve --advertise 10.0.0.5
Restart=on-failure
RestartPreventExitStatus=3
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now hivemind
sudo hivemind status
```

`init` wrote the CLI's config for root, so `sudo hivemind ...` on the server finds the API and the token. Without `sudo`, the CLI cannot read `/var/lib/hivemind` and says so: `hivemind on this machine was set up by root: this user cannot read /var/lib/hivemind, so this CLI has no API token for its cluster; run sudo hivemind status, or give the token with --token or HIVEMIND_TOKEN`. Do not run `hivemind init` as that user: it would make a second cluster, whose `serve` cannot take port 4483 while the first one runs.

## Worker

`hivemind join` joins the cluster and then keeps running as the worker's agent, in the foreground. Under systemd, join once by hand, stop it, and let the unit run `hivemind serve --role worker`. Do not start the unit while `join` still runs: two agents on one machine would both manage its containers.

```sh
sudo hivemind join 10.0.0.5 --token hivemind-join-v1.<fingerprint>.<secret>
# When it logs "worker running", or /var/lib/hivemind/node.json exists, stop it with Ctrl-C.
```

Then write `/etc/systemd/system/hivemind.service`:

```ini
[Unit]
Description=hivemind worker
Documentation=https://github.com/ao/hivemind
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/hivemind serve --role worker
Restart=on-failure
RestartPreventExitStatus=3
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now hivemind
```

On the server, `sudo hivemind nodes` lists the worker as `ready`.

## What the settings do

- The units run hivemind as root, which is what the default data directory (`/var/lib/hivemind`) and the Docker socket expect. To run as another user, add `User=` and a `--data-dir` that user owns, and give the user access to the Docker socket.
- `After=docker.service` starts hivemind once Docker has started. Containers that hivemind starts have the restart policy `no`, so after a reboot Docker leaves them stopped, and the agent starts them, or removes them if their replica moved, on its first pass.
- `After=network-online.target` waits for the network. If the `--advertise` address is not configured yet, `serve` cannot bind it and exits; `Restart=on-failure` tries again 5 s later.
- `systemctl stop hivemind` sends SIGTERM. hivemind stops, and its containers keep running, as with Ctrl-C.
- A worker that was removed from the cluster with `hivemind node remove` exits with status 3, at once and at every start after, and says its containers may still be on the machine. `RestartPreventExitStatus=3` keeps systemd from starting it again; without it, `Restart=on-failure` restarts it every 5 s, and the server logs a refused request each time. Every other error exits with status 1. Status 3 is new in the unreleased 2.2.0: a removed 2.0.0 or 2.1.0 worker exits with 1, and the unit restarts it until it is disabled. Run `systemctl disable --now hivemind` on a machine you remove. The server never exits with 3; the line is in its unit too so that both units are the same.
- Logs go to the journal: `journalctl -u hivemind`. The server logs one line at `info` for each API request.

## Measured with these units

On c7g.large machines (2 vCPU, Graviton3, Amazon Linux 2023), from the soak test's log:

- `sudo systemctl reboot` on a worker: the node was `ready` again 18.8 s later.
- A worker powered off, then started again with the EC2 API: `ready` 15.1 s after the start request was sent, and its old containers were gone in the same second.
- A worker removed with `hivemind node remove` (second soak): its agent stopped 1.1 s after the removal. With the unit still enabled, systemd started it again every 5.2 s, eight times in 45 s, and the server refused it each time, until `systemctl disable --now hivemind`.

The method and the other numbers are in [soak.md](soak.md).
