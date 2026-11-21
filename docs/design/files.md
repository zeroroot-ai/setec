# Files

A launch can place files in the machine before the workload starts. The
workload then finds each file from its first instruction. The files are
read-only for the workload.

## The path of a file

1. The caller sends `files` in the `Launch` request, or sets `spec.files` on the
   `Sandbox`.
2. The frontend and the admission webhook check the files with one set of
   rules (`internal/sandboxfiles`).
3. The operator keeps the content in the Secret `<name>-files`. The Sandbox
   owns the Secret, so it goes away with the Sandbox.
4. Only the launcher container mounts the Secret. The machine cannot read it.
5. The launcher copies each file into the machine through the control channel
   of the guest agent (`copy-in`). The channel is the vsock device. The files
   never cross the network of the Sandbox.
6. The launcher starts the workload after the last file arrives. A file that
   fails to arrive stops the launch.

## The rules

- A path is relative to `/setec/files`. It is clean and has no `..` element.
  No path appears twice, and no path is a file and the directory of another.
- The mode has no write, setuid or setgid bit. It is at most `0555`. The
  default is `0444`.
- The guest agent makes the directories of a path with the owner root and the
  mode `0755`. The workload user can read and enter them. It cannot write to
  them or to the files. A workload that runs as root can still change them.
- The total content is at most 512 KiB, because a Secret holds at most 1 MiB.
  The number of files has no bound.
- A review Sandbox and a Sandbox that restores a snapshot take no files. The
  workload of those already runs.

## The network

Files change nothing in the network of a Sandbox. The generated NetworkPolicy
and the network mode of the class apply as they do for each Sandbox. A script
that the Sandbox runs from `/setec/files` reaches only the hosts that the
policy allows.
