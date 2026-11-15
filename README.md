# SlideBolt Quickstart

`slidebolt` installs and runs a native, released SlideBolt system without Docker.
It is the production terminal and demo surface for Linux amd64 and arm64.

```bash
./slidebolt
```

The interactive flow asks where to place the installation (Enter creates a
temporary directory), lets you choose integration plugins, renders their own
serialized setup definitions, downloads checksum-verified release binaries,
and starts SlideBolt.

Noninteractive lifecycle:

```bash
slidebolt build --dir ./demo --plugin plugin-plex
slidebolt start --dir ./demo --detach
slidebolt status --dir ./demo
slidebolt devices --dir ./demo
slidebolt command --dir ./demo --entity plugin.device.entity --action switch_turn_on
slidebolt stop --dir ./demo
slidebolt configure --dir ./demo --plugin plugin-plex
slidebolt upgrade --dir ./demo --release v1.0.38
slidebolt destroy --dir ./demo
```

`configure` changes the selected plugins and their serialized settings without
changing the installed release. Use `--remove-plugin`, `--set`, or `--unset` as
needed. Sensitive values are read from a protected file so they do not appear
in shell history or process arguments:

```bash
slidebolt configure --dir ./demo --plugin plugin-example \
  --set-file plugin-example.token=/run/user/1000/plugin-token
```

`upgrade` requires an explicit release tag or exact catalog path. It downloads
and verifies the complete target installation before swapping any live file.
The installation identity, generated values, API token, data, and logs are
preserved. Downgrades require the explicit `--allow-downgrade` flag. Both
commands require the sandbox to be stopped.

Components may declare system-owned executable or file requirements. Quickstart
checks the resolved dependency graph before writing the installation and never
invokes a package manager or `sudo`. For example, selecting HDHomeRun pulls in
the hidden media service, which owns the shared FFmpeg and FFprobe requirements.
Install the requested package yourself, or provide non-standard executables
explicitly:

```bash
slidebolt build --dir ./tv-demo --plugin plugin-hdhomerun \
  --tool ffmpeg=/opt/media/bin/ffmpeg \
  --tool ffprobe=/opt/media/bin/ffprobe
```

Secrets are entered through the masked interactive setup. Sandboxes retain
their data and logs after stopping and are removed only by the guarded
`destroy` command.

